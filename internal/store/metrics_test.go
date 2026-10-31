package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func addSrc(t *testing.T, st *Store) int64 {
	t.Helper()
	id, err := st.AddSource(Source{BaseURL: "https://m" + time.Now().Format("150405.000000") + ".example", Name: "m", Origin: "manual", Enabled: true, Status: "usable"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestCheckMetricsRoundTrip(t *testing.T) {
	st := openTest(t)
	id := addSrc(t, st)
	if err := st.AddCheck(Check{
		SourceID: id, Kind: "docker", Tier: 3, OK: true,
		LatencyMS: 420, SpeedKBps: 9000, DigestOK: 1, BlobOK: 1, TTFBMS: 83,
	}); err != nil {
		t.Fatal(err)
	}
	c, err := st.LastCheck(id, "docker")
	if err != nil || c == nil {
		t.Fatalf("LastCheck: %v %v", c, err)
	}
	if c.BlobOK != 1 || c.TTFBMS != 83 {
		t.Fatalf("blob_ok/ttfb_ms 没落库：%+v", c)
	}
	pts, err := st.Series(id, 10)
	if err != nil || len(pts) != 1 {
		t.Fatalf("Series: %v %v", pts, err)
	}
	if pts[0].BlobOK != 1 || pts[0].TTFBMS != 83 {
		t.Fatalf("Series 没带上新字段：%+v", pts[0])
	}
}

func TestP95(t *testing.T) {
	cases := []struct {
		name string
		in   []int64
		want int64
	}{
		{"样本不足", []int64{1, 2, 3, 4}, 0},
		{"刚好五个", []int64{5, 1, 3, 2, 4}, 5},
		{"二十个", []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}, 19},
		{"一百个", makeRange(1, 100), 95},
		{"全等", []int64{7, 7, 7, 7, 7}, 7},
	}
	for _, c := range cases {
		if got := p95(append([]int64(nil), c.in...)); got != c.want {
			t.Errorf("%s: p95=%d，期望 %d", c.name, got, c.want)
		}
	}
}

func makeRange(a, b int64) []int64 {
	out := make([]int64, 0, b-a+1)
	for i := a; i <= b; i++ {
		out = append(out, i)
	}
	return out
}

func TestListSourcesP95(t *testing.T) {
	st := openTest(t)
	id := addSrc(t, st)
	for i, ms := range []int64{10, 20, 30, 40, 50, 60} {
		if err := st.AddCheck(Check{SourceID: id, Kind: "docker", Tier: 2, OK: true, LatencyMS: ms, TS: time.Now().Unix() - int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	// 失败样本的「延迟」是超时值，不该把 P95 拉上去。
	if err := st.AddCheck(Check{SourceID: id, Kind: "docker", Tier: 2, OK: false, LatencyMS: 25000}); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListSources()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("应有 1 个源，实际 %d", len(list))
	}
	if got := list[0].P95["docker"]; got != 60 {
		t.Fatalf("docker P95 应为 60，实际 %d", got)
	}
	if got := list[0].P95["github"]; got != 0 {
		t.Fatalf("没有 github 样本时应为 0，实际 %d", got)
	}
}

// 老库升级：checks 表里没有 blob_ok / ttfb_ms，Open 之后要补上，且默认值
// 是「未校验 / 没测」，不能是 0（0 会被读成「字节不符」）。
func TestMigrateAddsMetricColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id := addSrc(t, st)
	st.Close()

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"blob_ok", "ttfb_ms"} {
		if _, err := raw.Exec(`ALTER TABLE checks DROP COLUMN ` + col); err != nil {
			t.Fatalf("drop %s: %v", col, err)
		}
	}
	if _, err := raw.Exec(`INSERT INTO checks(source_id,kind,ts,tier,ok,latency_ms) VALUES(?,?,?,?,?,?)`,
		id, "docker", time.Now().Unix(), 3, 1, 100); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	c, err := st2.LastCheck(id, "docker")
	if err != nil || c == nil {
		t.Fatalf("LastCheck: %v %v", c, err)
	}
	if c.BlobOK != -1 {
		t.Fatalf("补列后 blob_ok 默认应为 -1，实际 %d", c.BlobOK)
	}
	if c.TTFBMS != 0 {
		t.Fatalf("补列后 ttfb_ms 默认应为 0，实际 %d", c.TTFBMS)
	}
}
