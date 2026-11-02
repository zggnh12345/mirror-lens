package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// site_name 是后加的列：写入要能读回，而且只能被「补名」逻辑填一次。
func TestSiteNameRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	id, err := st.AddSource(Source{
		Name: "docker.1ms.run", BaseURL: "https://docker.1ms.run",
		SiteName: "毫秒镜像", Status: "pending", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	list, err := st.ListSources()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].SiteName != "毫秒镜像" {
		t.Fatalf("site_name 应原样落库，实际 %+v", list)
	}

	// 已有真名不允许被覆盖。
	if err := st.SetSiteNameIfEmpty(id, "别的名字"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.ByURL("https://docker.1ms.run"); got.SiteName != "毫秒镜像" {
		t.Fatalf("真名不应被覆盖，实际 %q", got.SiteName)
	}

	// 空串和兜底「未知」可以补成真名。
	id2, err := st.AddSource(Source{Name: "x", BaseURL: "https://x.example", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSiteNameIfEmpty(id2, "未知"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSiteNameIfEmpty(id2, "真名"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.ByURL("https://x.example"); got.SiteName != "真名" {
		t.Fatalf("「未知」应能被真名顶掉，实际 %q", got.SiteName)
	}
}

// 面板状态条：一次批量查询，每个源最多 RecentN 条，时间升序，只算 tier>=2。
func TestRecentSummaries(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	id, err := st.AddSource(Source{Name: "a", BaseURL: "https://a.example", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for i := 0; i < 30; i++ {
		if err := st.AddCheck(Check{
			SourceID: id, Kind: "docker", TS: now - 100 + int64(i),
			Tier: 2, OK: i%2 == 0, LatencyMS: int64(i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// tier 1 是存活探测，不进状态条。
	if err := st.AddCheck(Check{SourceID: id, Kind: "docker", TS: now + 10, Tier: 1, OK: false}); err != nil {
		t.Fatal(err)
	}

	m, err := st.RecentSummaries(0)
	if err != nil {
		t.Fatal(err)
	}
	pts := m[id]
	if len(pts) != RecentN {
		t.Fatalf("应截到 %d 条，实际 %d", RecentN, len(pts))
	}
	// 丢掉最早的 6 条：最新一条（i=29）仍在末尾，最早的应是 i=6。
	if pts[len(pts)-1].LatencyMS != 29 {
		t.Fatalf("末尾应是最新样本，实际 latency=%d", pts[len(pts)-1].LatencyMS)
	}
	if pts[0].LatencyMS != 6 {
		t.Fatalf("应保留最近 %d 条（最早为 i=6），实际首条 latency=%d", RecentN, pts[0].LatencyMS)
	}
	for i := 1; i < len(pts); i++ {
		if pts[i-1].TS > pts[i].TS {
			t.Fatalf("桶内应按时间升序，第 %d 条乱序", i)
		}
	}
	for _, p := range pts {
		if p.TS > now {
			t.Fatalf("tier=1 的样本不应进状态条：%+v", p)
		}
	}
}

// 面板默认顺序：在线的排最前，其次慢、待检测、异常，最后归档；同层星标优先。
func TestListSourcesDefaultOrder(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	add := func(name, status string, pref bool) {
		t.Helper()
		if _, err := st.AddSource(Source{Name: name, BaseURL: "https://" + name, Status: status, Enabled: true, Preferred: pref}); err != nil {
			t.Fatal(err)
		}
	}
	add("dead.example", "dead", true)
	add("pending.example", "pending", false)
	add("slow.example", "degraded", false)
	add("ok-b.example", "usable", false)
	add("blocked.example", "blocked", false)
	add("ok-a.example", "usable", false)
	add("ok-pref.example", "usable", true)
	add("archived.example", "archived", false)

	list, err := st.ListSources()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range list {
		got = append(got, s.Name)
	}
	want := []string{"ok-pref.example", "ok-a.example", "ok-b.example", "slow.example",
		"pending.example", "dead.example", "blocked.example", "archived.example"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("默认排序不对\n got=%v\nwant=%v", got, want)
	}
}
