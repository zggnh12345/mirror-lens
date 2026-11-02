package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// /api/sources 是面板唯一的数据入口：站点名和最近 N 次摘要必须一起回来，
// 否则前端要么显示不出名字，要么得为每行再发一次请求。
func TestSourcesAPIShipsSiteNameAndRecent(t *testing.T) {
	st, h := newTestServer(t)
	_, ck := login(t, h, "admin", "admin")
	postJSON(h, "/api/setup", `{"user":"alex","pass":"correct-horse"}`, ck)
	_, ck = login(t, h, "alex", "correct-horse")

	id, err := st.AddSource(store.Source{
		Name: "docker.1ms.run", BaseURL: "https://docker.1ms.run",
		SiteName: "毫秒镜像", Status: "usable", Enabled: true, DockerCap: store.CapYes,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for i := 0; i < 30; i++ {
		if err := st.AddCheck(store.Check{
			SourceID: id, Kind: "docker", TS: now - 60 + int64(i),
			Tier: 2, OK: true, LatencyMS: int64(100 + i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sources", nil)
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("/api/sources 应 200，得到 %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Sources []store.Source `json:"sources"`
		RecentN int            `json:"recent_n"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if out.RecentN != store.RecentN {
		t.Fatalf("recent_n 应由后端下发 %d，实际 %d", store.RecentN, out.RecentN)
	}
	if len(out.Sources) != 1 {
		t.Fatalf("应有 1 个源，实际 %d", len(out.Sources))
	}
	s := out.Sources[0]
	if s.SiteName != "毫秒镜像" {
		t.Fatalf("site_name 应随列表返回，实际 %q", s.SiteName)
	}
	if len(s.Recent) != store.RecentN {
		t.Fatalf("每个源应带 %d 条摘要，实际 %d", store.RecentN, len(s.Recent))
	}
	last := s.Recent[len(s.Recent)-1]
	if last.LatencyMS != 129 || !last.OK || last.Kind != "docker" {
		t.Fatalf("摘要应是最新一条且升序排列，实际 %+v", last)
	}
}
