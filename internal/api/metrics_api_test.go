package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/auth"
	"github.com/zggnh12345/mirror-lens/internal/metrics"
)

func TestMetricsViewEndpoint(t *testing.T) {
	_, h := newTestServer(t)
	_, ck := login(t, h, "admin", "admin")
	rec := postJSON(h, "/api/setup", `{"user":"alex","pass":"correct-horse"}`, ck)
	if rec.Code != 200 {
		t.Fatalf("setup 应成功，得到 %d %s", rec.Code, rec.Body.String())
	}
	var newCk *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.CookieName {
			newCk = c
		}
	}
	if newCk == nil {
		t.Fatal("设置后应换发会话")
	}

	// 监控未启用：接口应返回 enabled:false，而不是报错。
	metrics.Disable()
	req := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
	req.AddCookie(newCk)
	r := httptest.NewRecorder()
	h.ServeHTTP(r, req)
	if r.Code != 200 {
		t.Fatalf("GET /api/metrics 应 200，得到 %d", r.Code)
	}
	var base map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &base); err != nil {
		t.Fatalf("解析失败 %v", err)
	}
	if base["enabled"] != false {
		t.Fatalf("未启用时应 enabled:false，得到 %v", base["enabled"])
	}

	// 启用并灌入聚合数据。
	metrics.Enable()
	defer metrics.Disable()
	metrics.SetCache(1000, 3, 500)
	metrics.HTTP("docker", "GET", 200, 0, 0, "hit")
	metrics.HTTP("docker", "GET", 200, 0, 0, "hit")
	metrics.HTTP("docker", "GET", 200, 0, 0, "miss")
	metrics.SnapshotHitRatio()
	metrics.SnapshotSceneHotness()
	metrics.SourceInflight(7, 2)
	metrics.SourceLimit(7, 8)

	r2 := httptest.NewRecorder()
	h.ServeHTTP(r2, req)
	if r2.Code != 200 {
		t.Fatalf("已启用应 200，得到 %d %s", r2.Code, r2.Body.String())
	}
	var m metrics.MetricsView
	if err := json.Unmarshal(r2.Body.Bytes(), &m); err != nil {
		t.Fatalf("解析失败 %v body=%s", err, r2.Body.String())
	}
	if !m.Enabled {
		t.Fatal("应 enabled:true")
	}
	if !m.Ratio.HasRatio || m.Ratio.Overall < 0.66 || m.Ratio.Overall > 0.67 {
		t.Fatalf("整体命中率应 ~0.667，得到 %v", m.Ratio.Overall)
	}
	if m.Heat["docker"] != 3 {
		t.Fatalf("docker 热度应为 3，得到 %d", m.Heat["docker"])
	}
	if m.Concurrency.Inflight != 2 || m.Concurrency.Limit != 8 {
		t.Fatalf("并发应为 2/8，得到 %d/%d", m.Concurrency.Inflight, m.Concurrency.Limit)
	}
	if len(m.Concurrency.Top) != 1 || m.Concurrency.Top[0].ID != 7 {
		t.Fatalf("Top 并发应为 id=7，得到 %+v", m.Concurrency.Top)
	}
	if !m.Cache.HasCache || m.Cache.Bytes != 1000 || m.Cache.Blobs != 3 || m.Cache.Free != 500 {
		t.Fatalf("缓存快照不对 %+v", m.Cache)
	}

	// 未授权访问必须 401。
	bad := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
	rb := httptest.NewRecorder()
	h.ServeHTTP(rb, bad)
	if rb.Code != 401 {
		t.Fatalf("未带会话应 401，得到 %d", rb.Code)
	}
}
