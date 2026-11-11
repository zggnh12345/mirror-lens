package runner

import (
	"context"
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

	Realms map[string][]string

	// Probe 可注入，测试用；默认 probe.ProbeRegistryHost。
	Probe func(context.Context, probe.RegistryHostProbe) probe.RegistryHostOutcome
	// Jobs 可注入，测试用；默认从 Store.ListPendingHostCaps 生成。
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
	next    time.Time
	backoff time.Duration
}

const (
	defaultHostProbeInterval = 6 * time.Hour
	defaultHostProbeBackoff  = 15 * time.Second
	maxHostProbeBackoff      = 10 * time.Minute
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
	r.Once(ctx)
	t := time.NewTicker(r.o.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
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

func (r *HostProbeRunner) jobs(ctx context.Context) ([]HostProbeJob, error) {
	if r.o.Jobs != nil {
		return r.o.Jobs(ctx)
	}
	rows, err := r.o.Store.ListPendingHostCaps()
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
		if st.next.After(now) {
			return false
		}
		delete(r.state, key)
	}
	r.state[key] = &probeState{next: now} // 占位，跑完由 finish 改成 true/false
	return true
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
	if st != nil && st.backoff > 0 {
		prev = st.backoff * 2
	}
	if prev > maxHostProbeBackoff {
		prev = maxHostProbeBackoff
	}
	r.state[key] = &probeState{next: r.o.Now().Add(prev), backoff: prev}
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
	res, err := r.o.Store.SetHostCaps(j.SourceID, j.Host, capv, mode, r.o.Now().Unix(), note, snap.Generation, snap.Digest)
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
		// 不写错误、不计失败，退避后重新取快照。
		retry = true
	case store.HostCapsSourceChanged:
		// 源已换 Host：下一轮按新归属重新入队。
	case store.HostCapsSourceUnavailable:
		// 归档或删除：不再重排。
	}
}
