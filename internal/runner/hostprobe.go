package runner

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/probe"
	"github.com/zggnh12345/mirror-lens/internal/secret"
	"github.com/zggnh12345/mirror-lens/internal/store"
	"github.com/zggnh12345/mirror-lens/internal/v2auth"
)

// HostProbeJob 是一次 Host 能力探测。Repo 必须在该 Host 上真实存在，
// 否则 404 会把可用源判成未知。
type HostProbeJob struct {
	SourceID int64
	Host     string
	BaseURL  string
	Repo     string
	Tag      string
}

// HostProbeOptions 是 Host 探测 Runner 的装配参数。
//
// Repos 是 Host → 探测仓库的映射：不同 Registry 没有通用仓库，猜一个
// 只会制造 404。没有映射的 Host 会被跳过并集中告警一次。
type HostProbeOptions struct {
	Store *store.Store
	Creds *secret.Store
	Repos map[string]string

	Interval    time.Duration
	Timeout     time.Duration
	Concurrency int
	// Backoff 是 stale_snapshot / 认证暂不可用时的重排退避起点，按次数翻倍。
	Backoff time.Duration

	// RecoveryTTL 是 cap=1 成功结论的有效期；0 用 store.RecoveryDefaultTTL。
	RecoveryTTL time.Duration

	// ScanInterval 是「捞到期任务」的主节拍。它是恢复延迟的上界：
	// 事务写入的 next_probe_at 最迟这么久被 Runner 看见。默认 30s。
	ScanInterval time.Duration

	Realms map[string][]string

	// Probe 可注入，测试用；默认 probe.ProbeRegistryHost。
	Probe func(context.Context, probe.RegistryHostProbe) probe.RegistryHostOutcome
	// Jobs 可注入，测试用；默认从 Store.ListDueHostProbes 生成。
	Jobs func(context.Context) ([]HostProbeJob, error)

	Now func() time.Time
}

// HostProbeRunner 只做一件事：把未验证的 Source×Host 探测一次并写回。
// 它不碰业务回源、不改 sources.status、不写健康度。
type HostProbeRunner struct {
	o HostProbeOptions

	mu    sync.Mutex
	state map[string]*probeState // key = sourceID|host
}

type probeState struct {
	next time.Time
	// running 覆盖「已 claim、未 finish」的整个探测生命周期。占位不能只
	// 靠 next=now：并发调用 Once 时会把它当成过期状态直接抢走。
	running bool
	backoff time.Duration
	// gen/digest 是上一次探测用的凭据版本：版本一变，旧版本的连续失败
	// 就不该继续把退避往上抬。
	gen    int64
	digest string
}

const (
	// defaultHostProbeInterval 是「全量对账」周期（reconcileRepos）。
	defaultHostProbeInterval = 6 * time.Hour
	// defaultHostProbeScan 是捞到期任务的节拍。恢复延迟上界由它决定，
	// 与 6h 对账周期无关。
	defaultHostProbeScan    = 30 * time.Second
	defaultHostProbeBackoff = 15 * time.Second
	maxHostProbeBackoff     = 10 * time.Minute
)

func NewHostProbeRunner(o HostProbeOptions) *HostProbeRunner {
	if o.Interval <= 0 {
		o.Interval = defaultHostProbeInterval
	}
	if o.Timeout <= 0 {
		o.Timeout = 25 * time.Second
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 2
	}
	if o.Backoff <= 0 {
		o.Backoff = defaultHostProbeBackoff
	}
	if o.RecoveryTTL <= 0 {
		o.RecoveryTTL = store.RecoveryDefaultTTL
	}
	if o.ScanInterval <= 0 {
		o.ScanInterval = defaultHostProbeScan
	}
	if o.Probe == nil {
		o.Probe = probe.ProbeRegistryHost
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &HostProbeRunner{o: o, state: map[string]*probeState{}}
}

// Run 启动周期扫描。第一条探测在启动后立即执行：未验证的源不该等到
// 下一个周期才拿到结论。
func (r *HostProbeRunner) Run(ctx context.Context) {
	if r.o.Store == nil || r.o.Creds == nil {
		log.Printf("Host 探测未启用：缺少 Store 或凭据存储")
		return
	}
	r.reconcileRepos()
	r.Once(ctx)
	scan := time.NewTicker(r.o.ScanInterval)
	defer scan.Stop()
	recon := time.NewTicker(r.o.Interval)
	defer recon.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-scan.C:
			r.Once(ctx)
		case <-recon.C:
			// 仓库映射变化会让旧结论失效；这与到期扫描是两件事。
			r.reconcileRepos()
			r.Once(ctx)
		}
	}
}

// Once 跑一轮：取任务、按并发执行、等同批结束。供测试与手动触发。
func (r *HostProbeRunner) Once(ctx context.Context) {
	jobs, err := r.jobs(ctx)
	if err != nil {
		log.Printf("Host 探测队列生成失败: %v", err)
		return
	}
	sem := make(chan struct{}, r.o.Concurrency)
	var wg sync.WaitGroup
	for _, j := range jobs {
		key := jobKey(j)
		if !r.claim(key) {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(j HostProbeJob) {
			defer wg.Done()
			defer func() { <-sem }()
			r.runOne(ctx, j)
		}(j)
	}
	wg.Wait()
}

// reconcileRepos 在启动时对账「结论是用哪个代表性仓库测出来的」。
// 仓库映射变了（含映射被删掉），旧结论立即失效：它可能只是另一个仓库
// 的性质，继续拿它选路等于没验证。
//
// 只清 registry_cap/path_mode，保留诊断与时间；也不动凭据代次，避免把
// 「探测输入变更」误报成「凭据变更」。
func (r *HostProbeRunner) reconcileRepos() {
	if r.o.Store == nil {
		return
	}
	rows, err := r.o.Store.ListHostCaps()
	if err != nil {
		log.Printf("对账探测仓库失败: %v", err)
		return
	}
	cleared := 0
	for _, x := range rows {
		if x.Cap == store.RegistryCapUnknown {
			continue
		}
		want, ok := r.o.Repos[x.RepoHost]
		var d probe.Diagnostics
		// 只有明确的 v2 诊断才算「有 repo」；v1 没有这个字段，未知版本
		// 更不能当成可信证据。
		hasRepo := json.Unmarshal([]byte(x.ProbeError), &d) == nil &&
			d.Version == probe.DiagnosticsVersion && d.Repo != ""
		if ok && hasRepo && d.Repo == probe.NormalizeRepo(want) {
			continue
		}
		done, err := r.o.Store.ResetHostCaps(x.SourceID, x.RepoHost, x.Cap)
		if err != nil {
			log.Printf("清除 %s（%s）旧结论失败: %v", x.Name, x.RepoHost, err)
			continue
		}
		if done {
			cleared++
		}
	}
	if cleared > 0 {
		log.Printf("代表性仓库已变更，清除 %d 条 Host 能力结论，等待重新探测", cleared)
	}
}

func (r *HostProbeRunner) jobs(ctx context.Context) ([]HostProbeJob, error) {
	if r.o.Jobs != nil {
		return r.o.Jobs(ctx)
	}
	rows, err := r.o.Store.ListDueHostProbes(r.o.Now().Unix())
	if err != nil {
		return nil, err
	}
	var out []HostProbeJob
	var missing []string
	seen := map[string]bool{}
	for _, x := range rows {
		repo := r.o.Repos[x.RepoHost]
		if repo == "" {
			if !seen[x.RepoHost] {
				seen[x.RepoHost] = true
				missing = append(missing, x.RepoHost)
			}
			continue
		}
		out = append(out, HostProbeJob{
			SourceID: x.SourceID,
			Host:     x.RepoHost,
			BaseURL:  x.BaseURL,
			Repo:     repo,
		})
	}
	if len(missing) > 0 {
		log.Printf("Host 探测缺少仓库配置，已跳过这些 Host: %v（用 ML_HOSTPROBE_REPOS=host=repo,... 配置）", missing)
	}
	return out, nil
}

func jobKey(j HostProbeJob) string {
	return j.Host + "|" + itoa(j.SourceID)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// claim 返回 false 表示同一 (source, host) 正在跑或还在退避窗口内。
func (r *HostProbeRunner) claim(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.state[key]
	now := r.o.Now()
	if st != nil {
		if st.running || st.next.After(now) {
			return false
		}
		delete(r.state, key)
	}
	r.state[key] = &probeState{next: now, running: true}
	return true
}

// noteSnapshot 记住本次探测使用的凭据版本。版本变化时把退避重置，
// 否则凭据早已稳定，仍会被旧版本的 10 分钟退避压着。
func (r *HostProbeRunner) noteSnapshot(key string, gen int64, digest string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.state[key]
	if st == nil {
		return
	}
	if st.gen != gen || st.digest != digest {
		st.backoff = 0
		st.gen, st.digest = gen, digest
	}
}

func (r *HostProbeRunner) finish(key string, retry bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.state[key]
	if !retry {
		delete(r.state, key)
		return
	}
	prev := r.o.Backoff
	var gen int64
	var digest string
	if st != nil {
		gen, digest = st.gen, st.digest
		if st.backoff > 0 {
			prev = st.backoff * 2
		}
	}
	if prev > maxHostProbeBackoff {
		prev = maxHostProbeBackoff
	}
	r.state[key] = &probeState{next: r.o.Now().Add(prev), backoff: prev, gen: gen, digest: digest}
}

func (r *HostProbeRunner) runOne(parent context.Context, j HostProbeJob) {
	key := jobKey(j)
	retry := false
	defer func() { r.finish(key, retry) }()

	snap, err := r.o.Creds.Snapshot(j.Host)
	if err != nil {
		switch {
		case errors.Is(err, secret.ErrNotReconciled):
			// 启动门禁应当已经拦住这种情况；再遇到就是装配错误，
			// 不能靠重试掩盖。
			log.Printf("Host 探测中止：凭据未对账（%v）", err)
		case errors.Is(err, secret.ErrCredentialPending):
			log.Printf("Host %s 凭据正在变更，稍后重试: %v", j.Host, err)
			retry = true
		default:
			log.Printf("读取 %s 凭据失败: %v", j.Host, err)
			retry = true
		}
		return
	}

	r.noteSnapshot(key, snap.Generation, snap.Digest)

	ctx, cancel := context.WithTimeout(parent, r.o.Timeout)
	defer cancel()
	out := r.o.Probe(ctx, probe.RegistryHostProbe{
		BaseURL: j.BaseURL,
		Host:    j.Host,
		Repo:    j.Repo,
		Tag:     j.Tag,
		Snapshot: v2auth.CredentialSnapshot{
			Host:       snap.Host,
			Generation: snap.Generation,
			Digest:     snap.Digest,
			Credentials: v2auth.Credentials{
				Username:    snap.Credential.Username,
				Password:    snap.Credential.Password,
				TokenMethod: snap.Credential.TokenMethod,
				TokenAuth:   snap.Credential.TokenAuth,
			},
		},
		Realms:  r.o.Realms,
		Timeout: r.o.Timeout,
	})

	capv, mode, note, err := out.StoreFields()
	if err != nil {
		log.Printf("Host %s 探测结果自相矛盾，拒绝写库: %v", j.Host, err)
		return
	}
	// 一次事务写回 cap / scoped 健康 / recovery。gen 与 digest 都来自
	// 探测起点的快照，事务内只做相等校验，不重新读取当前代次。
	res, err := r.o.Store.CommitHostProbe(store.HostProbeCommit{
		SourceID:      j.SourceID,
		Host:          j.Host,
		Cap:           capv,
		PathMode:      mode,
		Reason:        out.Reason,
		Transport:     out.Transport,
		At:            r.o.Now().Unix(),
		ErrText:       note,
		Gen:           snap.Generation,
		CredDigest:    snap.Digest,
		HasCredential: snap.HasCredential,
		TTL:           r.o.RecoveryTTL,
	})
	if err != nil {
		log.Printf("写回 %s 能力失败: %v", j.Host, err)
		retry = true
		return
	}
	switch res {
	case store.HostCapsCommitted:
		if out.Cap == probe.CapYes {
			log.Printf("Host %s 验证通过（%s）", j.Host, out.Selected)
		}
	case store.HostCapsStaleSnapshot:
		// 整笔拒绝：cap、scoped、recovery 都没有写入。退避后重新取快照。
		retry = true
	case store.HostCapsSourceChanged:
		// 源已换 Host：下一轮按新归属重新入队。
	case store.HostCapsSourceUnavailable:
		// 归档或删除：不再重排。
	}
}
