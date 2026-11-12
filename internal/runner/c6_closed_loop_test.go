package runner

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/probe"
	"github.com/zggnh12345/mirror-lens/internal/secret"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// unknownOutcome 模拟传输层错误：没有新结论（cap 保持上次的 1），
// 只累积失败证据，派生为 failed。
func unknownOutcome() probe.RegistryHostOutcome {
	path := probe.PathOutcome{
		Mode: "native", Cap: probe.CapUnknown,
		Reason: "transport_error", Transport: "timeout", Error: "timeout",
	}
	return probe.RegistryHostOutcome{
		Host:       "quay.io",
		Cap:        probe.CapUnknown,
		Reason:     "transport_error",
		Transport:  "timeout",
		Native:     path,
		HostPrefix: path,
	}
}

// observingExpirer 包住真实 Store，在物化完成的那一刻记录 recovery 投影，
// 用来证明「Once 内部确实先物化、再扫描」。
type observingExpirer struct {
	st     *store.Store
	srcID  int64
	host   string
	saw    bool
	state  string
	nextAt int64
}

func (o *observingExpirer) MaterializeExpired(now time.Time) (int, error) {
	n, err := o.st.MaterializeExpired(now)
	v, verr := o.st.RecoveryState(o.srcID, "registry", o.host)
	if verr == nil && v.State == store.RecoveryExpired {
		o.saw = true
		o.state = v.State
		o.nextAt = v.NextProbeAt
	}
	return n, err
}

type c6Harness struct {
	f             *hostProbeFixture
	guard         *store.RecoveryGuard
	calls         int
	invalidations int
	probe         func(context.Context, probe.RegistryHostProbe) probe.RegistryHostOutcome
}

func newC6Harness(t *testing.T, outcome func() probe.RegistryHostOutcome) *c6Harness {
	t.Helper()
	h := &c6Harness{f: newHostProbeFixture(t, "quay.io"), guard: store.NewRecoveryGuard()}
	// AddSource 用真实时间写首轮 next_probe_at，测试时钟必须落在同一量级。
	h.f.now = time.Now().Add(2 * time.Second).Truncate(time.Second)
	h.probe = func(context.Context, probe.RegistryHostProbe) probe.RegistryHostOutcome {
		h.calls++
		return outcome()
	}
	return h
}

func (h *c6Harness) runner(exp RecoveryExpirer, repos map[string]string, now func() time.Time) *HostProbeRunner {
	return NewHostProbeRunner(HostProbeOptions{
		Store:       h.f.st,
		Creds:       h.f.creds,
		Repos:       repos,
		Concurrency: 1,
		Backoff:     15 * time.Second,
		Guard:       h.guard,
		Invalidate:  func() { h.invalidations++ },
		Expirer:     exp,
		Now:         now,
		Probe: func(ctx context.Context, p probe.RegistryHostProbe) probe.RegistryHostOutcome {
			return h.probe(ctx, p)
		},
	})
}

func (h *c6Harness) recovery(t *testing.T) store.RecoveryView {
	t.Helper()
	v, err := h.f.st.RecoveryState(h.f.srcID, "registry", "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (h *c6Harness) candidates(t *testing.T) int {
	t.Helper()
	rows, err := h.f.st.RegistryCandidatesRaw("quay.io")
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

// 完整闭环：成功 → ok → 超 TTL → 物化 expired + next=now → 同轮重探 → ok。
func TestC6ClosedLoopOKExpiredOK(t *testing.T) {
	h := newC6Harness(t, yesOutcome)
	base := h.f.now
	obs := &observingExpirer{st: h.f.st, srcID: h.f.srcID, host: "quay.io"}
	r := h.runner(obs, map[string]string{"quay.io": "prometheus/node-exporter"},
		func() time.Time { return h.f.now })

	r.Once(context.Background())
	if h.calls != 1 {
		t.Fatalf("首轮应探测一次，实际 %d", h.calls)
	}
	v := h.recovery(t)
	if v.State != store.RecoveryOK || v.LastSuccessAt != base.Unix() {
		t.Fatalf("首轮成功未落 ok: %+v", v)
	}
	if h.candidates(t) != 1 {
		t.Fatalf("ok 且在 TTL 内应进候选")
	}
	if h.guard.Blocked(store.GuardKey{SourceID: h.f.srcID, Family: "registry", Scope: "quay.io"}) {
		t.Fatal("成功提交后不应残留屏蔽")
	}
	if h.invalidations != 1 {
		t.Fatalf("成功提交应失效一次缓存，实际 %d", h.invalidations)
	}

	// 越过 TTL。
	h.f.now = base.Add(store.RecoveryDefaultTTL + 2*time.Second)
	r.Once(context.Background())

	if !obs.saw {
		t.Fatal("Once 内部未观察到 expired 物化")
	}
	if obs.nextAt != h.f.now.Unix() {
		t.Fatalf("物化后 next_probe_at 应为本轮 now=%d，实际 %d", h.f.now.Unix(), obs.nextAt)
	}
	if h.calls != 2 {
		t.Fatalf("物化后同一轮应重探，实际探测 %d 次", h.calls)
	}
	v = h.recovery(t)
	if v.State != store.RecoveryOK {
		t.Fatalf("重探成功应恢复 ok，实际 %s", v.State)
	}
	if v.LastSuccessAt != h.f.now.Unix() {
		t.Fatalf("last_success_at 未更新：%d", v.LastSuccessAt)
	}
	if v.NextProbeAt != h.f.now.Unix()+int64(store.RecoveryDefaultTTL/time.Second) {
		t.Fatalf("next_probe_at 未按新成功时间重排：%d", v.NextProbeAt)
	}
	if h.invalidations != 2 {
		t.Fatalf("两次成功提交应各失效一次缓存，实际 %d", h.invalidations)
	}
}

// 失败恢复：expired 后重探失败 → failed，候选仍为 0；再成功可回到 ok。
func TestC6ClosedLoopExpiredThenFailedThenOK(t *testing.T) {
	h := newC6Harness(t, yesOutcome)
	base := h.f.now
	r := h.runner(nil, map[string]string{"quay.io": "prometheus/node-exporter"},
		func() time.Time { return h.f.now })

	r.Once(context.Background())

	// 第二次探测改为确定性 miss。
	h.probe = func(context.Context, probe.RegistryHostProbe) probe.RegistryHostOutcome {
		h.calls++
		return unknownOutcome()
	}
	h.f.now = base.Add(store.RecoveryDefaultTTL + 2*time.Second)
	r.Once(context.Background())

	if h.calls != 2 {
		t.Fatalf("应重探一次，实际 %d", h.calls)
	}
	v := h.recovery(t)
	if v.State != store.RecoveryFailed {
		t.Fatalf("重探失败应进入 failed，实际 %s", v.State)
	}
	if v.RecFailures != 1 {
		t.Fatalf("rec_failures 应为 1，实际 %d", v.RecFailures)
	}
	if h.candidates(t) != 0 {
		t.Fatal("failed 不应进入候选")
	}

	// 退避过后再成功，应恢复 ok。
	h.probe = func(context.Context, probe.RegistryHostProbe) probe.RegistryHostOutcome {
		h.calls++
		return yesOutcome()
	}
	h.f.now = h.f.now.Add(2 * time.Minute)
	r.Once(context.Background())
	v = h.recovery(t)
	if v.State != store.RecoveryOK || v.LastSuccessAt != h.f.now.Unix() {
		t.Fatalf("恢复失败: %+v", v)
	}
	if h.candidates(t) != 1 {
		t.Fatal("恢复 ok 后应重新进候选")
	}
}

// TTL 缩短：原 next_probe_at 还在未来，但成功证据已过期；物化必须把
// next_probe_at 拉到 now，使同一轮扫描就能重探。
func TestC6TTLShortenPullsProbeIntoCurrentRound(t *testing.T) {
	h := newC6Harness(t, yesOutcome)
	base := h.f.now
	r := h.runner(nil, map[string]string{"quay.io": "prometheus/node-exporter"},
		func() time.Time { return h.f.now })

	r.Once(context.Background())
	if h.recovery(t).NextProbeAt != base.Unix()+int64(24*time.Hour/time.Second) {
		t.Fatalf("首轮 next_probe_at 异常: %d", h.recovery(t).NextProbeAt)
	}

	// 配置缩短到 1h，时间推进 2h：距原 next_probe_at（24h）还有 22h。
	h.f.st.SetRecoveryTTL(time.Hour)
	h.f.now = base.Add(2 * time.Hour)

	r.Once(context.Background())
	if h.calls != 2 {
		t.Fatalf("TTL 缩短后应在同一轮重探，实际探测 %d 次", h.calls)
	}
	v := h.recovery(t)
	if v.State != store.RecoveryOK || v.LastSuccessAt != h.f.now.Unix() {
		t.Fatalf("重探结果未写回: %+v", v)
	}
}

// TTL 延长：已物化的 expired 不会自动回 ok。
func TestC6TTLExtendKeepsExpired(t *testing.T) {
	h := newC6Harness(t, yesOutcome)
	base := h.f.now

	// 先成功，再直接物化（跳过 Runner，确保停在 expired）。
	r := h.runner(nil, map[string]string{"quay.io": "prometheus/node-exporter"},
		func() time.Time { return h.f.now })
	r.Once(context.Background())

	now := base.Add(store.RecoveryDefaultTTL + 2*time.Second)
	if n, err := h.f.st.MaterializeExpired(now); err != nil || n != 1 {
		t.Fatalf("物化失败 n=%d err=%v", n, err)
	}
	if h.recovery(t).State != store.RecoveryExpired {
		t.Fatal("应处于 expired")
	}

	// 延长 TTL 后再次物化：不得把 expired 改回 ok。
	h.f.st.SetRecoveryTTL(48 * time.Hour)
	h.f.now = now.Add(time.Hour)
	// Repos 为空：即使被扫描也拿不到仓库，不会触发新探测。
	r2 := h.runner(nil, nil, func() time.Time { return h.f.now })
	r2.Once(context.Background())

	v := h.recovery(t)
	if v.State != store.RecoveryExpired {
		t.Fatalf("延长 TTL 不应恢复 ok，实际 %s", v.State)
	}
	if h.candidates(t) != 0 {
		t.Fatal("expired 不应进候选")
	}
}

// 重启一致：物化后的 expired 与 next_probe_at 已持久化，新进程仍能调度重探。
func TestC6RestartKeepsExpiredSchedule(t *testing.T) {
	h := newC6Harness(t, yesOutcome)
	base := h.f.now
	r := h.runner(nil, map[string]string{"quay.io": "prometheus/node-exporter"},
		func() time.Time { return h.f.now })
	r.Once(context.Background())

	now := base.Add(store.RecoveryDefaultTTL + 2*time.Second)
	if n, err := h.f.st.MaterializeExpired(now); err != nil || n != 1 {
		t.Fatalf("物化失败 n=%d err=%v", n, err)
	}
	v := h.recovery(t)
	if v.State != store.RecoveryExpired || v.NextProbeAt != now.Unix() {
		t.Fatalf("物化结果未持久化: %+v", v)
	}

	// 模拟进程重启：关闭并重新打开同一个库，重建凭据存储与 Runner。
	h.f.st.Close()
	st2, err := store.Open(filepath.Join(h.f.dir, "hp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st2.Close() })
	cs2, err := secret.NewCredStore(filepath.Join(h.f.dir, "registry_creds.enc"), h.f.key, st2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs2.Reconcile(); err != nil {
		t.Fatal(err)
	}

	calls := 0
	r2 := NewHostProbeRunner(HostProbeOptions{
		Store: st2, Creds: cs2,
		Repos:       map[string]string{"quay.io": "prometheus/node-exporter"},
		Concurrency: 1,
		Guard:       store.NewRecoveryGuard(),
		Invalidate:  func() {},
		Now:         func() time.Time { return now },
		Probe: func(context.Context, probe.RegistryHostProbe) probe.RegistryHostOutcome {
			calls++
			return yesOutcome()
		},
	})
	r2.Once(context.Background())

	if calls != 1 {
		t.Fatalf("重启后应重探一次，实际 %d", calls)
	}
	v2, err := st2.RecoveryState(h.f.srcID, "registry", "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if v2.State != store.RecoveryOK || v2.LastSuccessAt != now.Unix() {
		t.Fatalf("重启后未恢复 ok: %+v", v2)
	}
}
