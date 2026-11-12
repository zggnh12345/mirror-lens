package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/probe"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// A3：真实链路端到端。这里不注入假 Probe：默认的 probe.ProbeRegistryHost
// 会真的发起 HTTP 请求，落库走真实 SQLite 与真实 RecoveryGuard。
func TestA3RealChainEndToEnd(t *testing.T) {
	// 真实探测默认拒绝内网/回环地址（SSRF 防护）；本用例的 registry 跑在
	// 本机 httptest 上，所以只在测试进程内放开，结束后恢复。
	probe.Configure(true)
	defer probe.Configure(false)

	t.Run("真实 HTTP 探测成功后写回并可入选", func(t *testing.T) {
		f := newHostProbeFixture(t, "quay.io")
		f.now = time.Now()
		if _, err := f.st.BumpHostGen("quay.io"); err != nil {
			t.Fatal(err)
		}
		srv := fakeRegistry(t, 0)
		// 源地址换成假 registry：探测走真实网络，不 mock。
		if _, err := rawExec(t, f, `UPDATE sources SET base_url=? WHERE id=?`, srv.URL, f.srcID); err != nil {
			t.Fatal(err)
		}

		r := NewHostProbeRunner(HostProbeOptions{
			Store: f.st, Creds: f.creds,
			Repos:       map[string]string{"quay.io": "library/alpine"},
			Concurrency: 1,
			Now:         func() time.Time { return f.now },
			Guard:       store.NewRecoveryGuard(),
		})
		r.Once(context.Background())

		src, err := f.st.GetSource(f.srcID)
		if err != nil {
			t.Fatal(err)
		}
		if src.RegistryCap != store.RegistryCapYes {
			t.Fatalf("真实探测应写入 cap=1: %d (%s)", src.RegistryCap, src.RegistryProbeError)
		}
		cands, err := f.st.RegistryCandidatesRaw("quay.io")
		if err != nil {
			t.Fatal(err)
		}
		if !hasSource(cands, f.srcID) {
			t.Fatalf("cap=1 后应出现在候选: %+v", cands)
		}
		failures, _, _, _ := f.st.HostProbeRetryState(f.srcID, "quay.io")
		if failures != 0 {
			t.Fatalf("成功提交不得留下退避计数: %d", failures)
		}
	})

	t.Run("跨 Host 隔离", func(t *testing.T) {
		f := newHostProbeFixture(t, "quay.io")
		f.now = time.Now()
		genQ, err := f.st.BumpHostGen("quay.io")
		if err != nil {
			t.Fatal(err)
		}
		idK, err := f.st.AddSource(store.Source{Name: "k8s", BaseURL: "https://k8s.example", Status: "usable", Enabled: true, RepoHost: "registry.k8s.io"})
		if err != nil {
			t.Fatal(err)
		}
		genK, err := f.st.BumpHostGen("registry.k8s.io")
		if err != nil {
			t.Fatal(err)
		}
		okSrv := fakeRegistry(t, 0)
		badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		t.Cleanup(badSrv.Close)
		if _, err := rawExec(t, f, `UPDATE sources SET base_url=? WHERE id=?`, okSrv.URL, f.srcID); err != nil {
			t.Fatal(err)
		}
		if _, err := rawExec(t, f, `UPDATE sources SET base_url=? WHERE id=?`, badSrv.URL, idK); err != nil {
			t.Fatal(err)
		}
		r := NewHostProbeRunner(HostProbeOptions{
			Store: f.st, Creds: f.creds,
			Repos:       map[string]string{"quay.io": "library/alpine", "registry.k8s.io": "library/alpine"},
			Concurrency: 1,
			Now:         func() time.Time { return f.now },
			Guard:       store.NewRecoveryGuard(),
			Jobs: func(context.Context) ([]HostProbeJob, error) {
				return []HostProbeJob{
					{SourceID: f.srcID, Host: "quay.io", Gen: genQ, BaseURL: okSrv.URL, Repo: "library/alpine"},
					{SourceID: idK, Host: "registry.k8s.io", Gen: genK, BaseURL: badSrv.URL, Repo: "library/alpine"},
				}, nil
			},
		})
		r.Once(context.Background())

		q, _ := f.st.GetSource(f.srcID)
		k, _ := f.st.GetSource(idK)
		if q.RegistryCap != store.RegistryCapYes {
			t.Fatalf("quay 应成功: %d", q.RegistryCap)
		}
		if k.RegistryCap == store.RegistryCapYes {
			t.Fatal("5xx 不得判成功")
		}
		// Host B 的恢复行不得被 Host A 的结论带动。
		raw, err := sql.Open("sqlite", filepath.Join(f.dir, "hp.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		var genKRow, nextK int64
		if err := raw.QueryRow(`SELECT gen, next_probe_at FROM registry_host_recovery WHERE source_id=? AND family='registry'`, idK).Scan(&genKRow, &nextK); err != nil {
			t.Fatal(err)
		}
		if genKRow != genK {
			t.Fatalf("Host B 代次被改: got=%d want=%d", genKRow, genK)
		}
		// B 的 5xx 只影响 B：按自己的恢复退避推到未来，A 的成功提交不得
		// 改动 B 的排期或失败计数。
		if nextK <= f.now.Unix() {
			t.Fatalf("Host B 应按自己的失败退避到未来: got=%d now=%d", nextK, f.now.Unix())
		}
		var recK int
		if err := raw.QueryRow(`SELECT rec_failures FROM registry_host_recovery WHERE source_id=? AND family='registry'`, idK).Scan(&recK); err != nil {
			t.Fatal(err)
		}
		if recK != 1 {
			t.Fatalf("Host B 的恢复失败计数应为 1: %d", recK)
		}
		qFail, _, _, _ := f.st.HostProbeRetryState(f.srcID, "quay.io")
		if qFail != 0 {
			t.Fatalf("Host A 成功不得留下退避: %d", qFail)
		}
	})

	t.Run("陈旧快照不写回", func(t *testing.T) {
		f := newHostProbeFixture(t, "quay.io")
		f.now = time.Now()
		gen, err := f.st.BumpHostGen("quay.io")
		if err != nil {
			t.Fatal(err)
		}
		srv := fakeRegistry(t, 0)
		if _, err := rawExec(t, f, `UPDATE sources SET base_url=? WHERE id=?`, srv.URL, f.srcID); err != nil {
			t.Fatal(err)
		}
		r := NewHostProbeRunner(HostProbeOptions{
			Store: f.st, Creds: f.creds, Concurrency: 1,
			Now:   func() time.Time { return f.now },
			Guard: store.NewRecoveryGuard(),
			Jobs: func(context.Context) ([]HostProbeJob, error) {
				return []HostProbeJob{{SourceID: f.srcID, Host: "quay.io", Gen: gen - 1, BaseURL: srv.URL, Repo: "library/alpine"}}, nil
			},
			// 真实探测，但返回前推进代次，模拟「探测期间凭据变更」。
			Probe: func(ctx context.Context, p probe.RegistryHostProbe) probe.RegistryHostOutcome {
				out := probe.ProbeRegistryHost(ctx, p)
				if _, err := f.st.BumpHostGen("quay.io"); err != nil {
					t.Error(err)
				}
				return out
			},
		})
		r.Once(context.Background())

		src, _ := f.st.GetSource(f.srcID)
		if src.RegistryCap == store.RegistryCapYes {
			t.Fatal("陈旧代次不得写成功结论")
		}
		failures, next, _, _ := f.st.HostProbeRetryState(f.srcID, "quay.io")
		if failures != 0 || next > f.now.Unix() {
			t.Fatalf("陈旧快照不得退避: failures=%d next=%d", failures, next)
		}
		var state string
		var recFailures int
		raw, err := sql.Open("sqlite", filepath.Join(f.dir, "hp.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		if err := raw.QueryRow(`SELECT state, rec_failures FROM registry_host_recovery WHERE source_id=? AND family='registry'`, f.srcID).Scan(&state, &recFailures); err != nil {
			t.Fatal(err)
		}
		if state != "initial" || recFailures != 0 {
			t.Fatalf("陈旧快照不得改恢复状态: state=%s rec_failures=%d", state, recFailures)
		}
	})

	t.Run("凭据变更不写回且不 Block", func(t *testing.T) {
		f := newHostProbeFixture(t, "quay.io")
		f.now = time.Now()
		gen, err := f.st.BumpHostGen("quay.io")
		if err != nil {
			t.Fatal(err)
		}
		srv := fakeRegistry(t, 0)
		if _, err := rawExec(t, f, `UPDATE sources SET base_url=? WHERE id=?`, srv.URL, f.srcID); err != nil {
			t.Fatal(err)
		}
		guard := store.NewRecoveryGuard()
		digest := "sha256:" + strings.Repeat("b", 64)
		r := NewHostProbeRunner(HostProbeOptions{
			Store: f.st, Creds: f.creds, Concurrency: 1,
			Now:   func() time.Time { return f.now },
			Guard: guard,
			Jobs: func(context.Context) ([]HostProbeJob, error) {
				return []HostProbeJob{{SourceID: f.srcID, Host: "quay.io", Gen: gen, BaseURL: srv.URL, Repo: "library/alpine"}}, nil
			},
			// 真实探测，返回前走真实凭据变更入口（ApplyCredChanges）。
			Probe: func(ctx context.Context, p probe.RegistryHostProbe) probe.RegistryHostOutcome {
				out := probe.ProbeRegistryHost(ctx, p)
				if err := f.st.ApplyCredChanges([]store.CredChange{{Host: "quay.io", Digest: &digest}}); err != nil {
					t.Error(err)
				}
				return out
			},
		})
		r.Once(context.Background())

		src, _ := f.st.GetSource(f.srcID)
		if src.RegistryCap == store.RegistryCapYes {
			t.Fatal("凭据变更后旧结果不得写成功结论")
		}
		key := store.GuardKey{SourceID: f.srcID, Family: "registry", Scope: "quay.io"}
		if guard.Blocked(key) {
			t.Fatal("陈旧快照不得 Block 路由缓存")
		}
		var state string
		var rec, retry int
		raw, err := sql.Open("sqlite", filepath.Join(f.dir, "hp.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		if err := raw.QueryRow(`SELECT state, rec_failures, retry_failures FROM registry_host_recovery WHERE source_id=? AND family='registry'`, f.srcID).Scan(&state, &rec, &retry); err != nil {
			t.Fatal(err)
		}
		if state != "initial" || rec != 0 || retry != 0 {
			t.Fatalf("凭据变更后应保持 initial 且无失败计数: state=%s rec=%d retry=%d", state, rec, retry)
		}
	})

	t.Run("hard-excluded 不参与扫描", func(t *testing.T) {
		f := newHostProbeFixture(t, "quay.io")
		f.now = time.Now()
		gen, err := f.st.BumpHostGen("quay.io")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rawExec(t, f, `UPDATE registry_host_recovery SET state='hard-excluded', next_probe_at=0 WHERE source_id=? AND family='registry'`, f.srcID); err != nil {
			t.Fatal(err)
		}
		jobs, err := f.st.ListDueHostProbes(f.now.Unix())
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range jobs {
			if j.SourceID == f.srcID {
				t.Fatalf("hard-excluded 源仍被调度: %+v", j)
			}
		}
		srv := fakeRegistry(t, 0)
		if _, err := rawExec(t, f, `UPDATE sources SET base_url=? WHERE id=?`, srv.URL, f.srcID); err != nil {
			t.Fatal(err)
		}
		calls := 0
		r := NewHostProbeRunner(HostProbeOptions{
			Store: f.st, Creds: f.creds, Concurrency: 1,
			Now:   func() time.Time { return f.now },
			Guard: store.NewRecoveryGuard(),
			Probe: func(ctx context.Context, p probe.RegistryHostProbe) probe.RegistryHostOutcome {
				calls++
				return probe.ProbeRegistryHost(ctx, p)
			},
		})
		r.Once(context.Background())
		if calls != 0 {
			t.Fatalf("hard-excluded 不得被探测，实际 %d 次", calls)
		}
		var next int64
		raw, err := sql.Open("sqlite", filepath.Join(f.dir, "hp.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		if err := raw.QueryRow(`SELECT next_probe_at FROM registry_host_recovery WHERE source_id=? AND family='registry'`, f.srcID).Scan(&next); err != nil {
			t.Fatal(err)
		}
		if next != 0 {
			t.Fatalf("hard-excluded 的 next_probe_at 不得被推后: %d（gen=%d）", next, gen)
		}
	})

	t.Run("失败结论不进入候选", func(t *testing.T) {
		f := newHostProbeFixture(t, "quay.io")
		f.now = time.Now()
		gen, err := f.st.BumpHostGen("quay.io")
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "not found", http.StatusNotFound)
		}))
		t.Cleanup(srv.Close)
		r := NewHostProbeRunner(HostProbeOptions{
			Store: f.st, Creds: f.creds, Concurrency: 1,
			Now:   func() time.Time { return f.now },
			Guard: store.NewRecoveryGuard(),
			Jobs: func(context.Context) ([]HostProbeJob, error) {
				return []HostProbeJob{{SourceID: f.srcID, Host: "quay.io", Gen: gen, BaseURL: srv.URL, Repo: "library/alpine"}}, nil
			},
		})
		r.Once(context.Background())
		cands, err := f.st.RegistryCandidatesRaw("quay.io")
		if err != nil {
			t.Fatal(err)
		}
		if hasSource(cands, f.srcID) {
			t.Fatalf("失败结论不得进入候选: %+v", cands)
		}
	})
}

// fakeRegistry 是最小但真实的 registry：/v2/ 200，原生 manifest 200，
// host_prefix 路径 404 UNSUPPORTED。内容是真 JSON，摘要由被测代码自算。
func fakeRegistry(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []map[string]any{{
			"mediaType": "application/vnd.oci.image.manifest.v1+json",
			"digest":    "sha256:" + strings.Repeat("a", 64),
			"size":      1,
		}},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delay > 0 {
			time.Sleep(delay)
		}
		switch {
		case r.URL.Path == "/v2/":
			w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
			w.WriteHeader(http.StatusOK)
		case strings.HasPrefix(r.URL.Path, "/v2/quay.io/"),
			strings.HasPrefix(r.URL.Path, "/v2/registry.k8s.io/"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"errors":[{"code":"UNSUPPORTED","message":"no host-prefix"}]}`))
		case strings.Contains(r.URL.Path, "/manifests/"):
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			w.WriteHeader(http.StatusOK)
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func rawExec(t *testing.T, f *hostProbeFixture, q string, args ...any) (sql.Result, error) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(f.dir, "hp.db"))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return db.Exec(q, args...)
}

func hasSource(cands []store.SourceStat, id int64) bool {
	for _, c := range cands {
		if c.ID == id {
			return true
		}
	}
	return false
}
