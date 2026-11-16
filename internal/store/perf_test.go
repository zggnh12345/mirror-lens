package store

import (
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func openPerfStore(tb testing.TB) *Store {
	tb.Helper()
	st, err := Open(filepath.Join(tb.TempDir(), "perf.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { st.Close() })
	return st
}

// seedPerfRegistry 建 n 个可调度的 foreign Registry 源，用于候选查询压测。
// 直接走 AddSource 保证 caps/策略与生产一致；recovery 只补候选所需字段。
func seedPerfRegistry(tb testing.TB, st *Store, n int) {
	tb.Helper()
	now := time.Now().Unix()
	for i := 0; i < n; i++ {
		id, err := st.AddSource(Source{
			BaseURL:      "https://perf-" + itoa(i) + ".example/",
			Name:         "perf-" + itoa(i),
			RepoHost:     "quay.io",
			DockerCap:    CapYes,
			RegistryCap:  CapYes,
			RegistryPath: PathNative,
			Status:       "usable",
			Enabled:      true,
		})
		if err != nil {
			tb.Fatal(err)
		}
		if _, err := st.db.Exec(`INSERT INTO registry_host_recovery
			(source_id,family,scope,state,gen,last_probe_at,last_success_at,next_probe_at,updated_at)
			VALUES(?, 'registry', 'quay.io', 'ok', 0, ?, ?, 0, ?)
			ON CONFLICT(source_id,family,scope) DO UPDATE SET state='ok', last_probe_at=excluded.last_probe_at,
				last_success_at=excluded.last_success_at, next_probe_at=0, updated_at=excluded.updated_at`,
			id, now, now, now); err != nil {
			tb.Fatal(err)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}

func pct(d []time.Duration, q float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	i := int(float64(len(d)-1) * q)
	return d[i]
}

func TestRegistryCandidateLatency(t *testing.T) {
	st := openPerfStore(t)
	seedPerfRegistry(t, st, 300)
	// 预热，避免把首次 prepared statement 编译算进分位数。
	for i := 0; i < 10; i++ {
		if _, err := st.RegistryCandidatesRaw("quay.io"); err != nil {
			t.Fatal(err)
		}
	}
	const rounds = 100
	ds := make([]time.Duration, 0, rounds)
	for i := 0; i < rounds; i++ {
		start := time.Now()
		out, err := st.RegistryCandidatesRaw("quay.io")
		if err != nil {
			t.Fatal(err)
		}
		ds = append(ds, time.Since(start))
		if len(out) != 300 {
			t.Fatalf("候选数=%d，期望 300", len(out))
		}
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	t.Logf("registry candidates n=300 P50=%s P95=%s P99=%s max=%s", pct(ds, .50), pct(ds, .95), pct(ds, .99), ds[len(ds)-1])
	if pct(ds, .99) > 250*time.Millisecond {
		t.Fatalf("P99 %s 超过 250ms 门禁", pct(ds, .99))
	}
}

func BenchmarkRegistryCandidates300(b *testing.B) {
	st := openPerfStore(b)
	seedPerfRegistry(b, st, 300)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.RegistryCandidatesRaw("quay.io"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDockerCandidates300(b *testing.B) {
	st := openPerfStore(b)
	seedPerfRegistry(b, st, 300)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.SourceCandidates("docker"); err != nil {
			b.Fatal(err)
		}
	}
}

func TestDockerCandidateLatency(t *testing.T) {
	st := openPerfStore(t)
	seedPerfRegistry(t, st, 300)
	for i := 0; i < 10; i++ {
		if _, err := st.SourceCandidates("docker"); err != nil {
			t.Fatal(err)
		}
	}
	const rounds = 100
	ds := make([]time.Duration, 0, rounds)
	for i := 0; i < rounds; i++ {
		start := time.Now()
		out, err := st.SourceCandidates("docker")
		if err != nil {
			t.Fatal(err)
		}
		ds = append(ds, time.Since(start))
		if len(out) != 300 {
			t.Fatalf("候选数=%d，期望 300", len(out))
		}
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	t.Logf("docker candidates n=300 P50=%s P95=%s P99=%s max=%s", pct(ds, .50), pct(ds, .95), pct(ds, .99), ds[len(ds)-1])
	if pct(ds, .99) > 250*time.Millisecond {
		t.Fatalf("P99 %s 超过 250ms 门禁", pct(ds, .99))
	}
}

// BenchmarkLegacyPerSourceSamples300 是 14a 风格的逐源取样路径，仅用于
// 量化 N+1；产品候选查询已不再走它。
func BenchmarkLegacyPerSourceSamples300(b *testing.B) {
	st := openPerfStore(b)
	seedPerfRegistry(b, st, 300)
	ids := make([]int64, 0, 300)
	for i := 0; i < 300; i++ {
		var id int64
		if err := st.db.QueryRow(`SELECT id FROM sources ORDER BY id LIMIT 1 OFFSET ?`, i).Scan(&id); err != nil {
			b.Fatal(err)
		}
		ids = append(ids, id)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, id := range ids {
			if _, _, _, _, _, err := st.sourceSamples(id, "registry:quay.io"); err != nil {
				b.Fatal(err)
			}
		}
	}
}
