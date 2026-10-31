package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

func patchJSON(h http.Handler, path, body string, ck *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if ck != nil {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

// setupPanel 走一遍首次设置，返回可用的会话 cookie。
func setupPanel(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	_, ck := login(t, h, "admin", "admin")
	rec := postJSON(h, "/api/setup", `{"user":"alex","pass":"correct-horse"}`, ck)
	if rec.Code != 200 {
		t.Fatalf("设置账号失败: %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "ml_session" {
			return c
		}
	}
	t.Fatal("设置后没拿到会话")
	return nil
}

func lifecycleSourceAPI(t *testing.T, st *store.Store) int64 {
	t.Helper()
	id, err := st.AddSource(store.Source{
		Name: "s", BaseURL: "https://127.0.0.1:1", Status: "usable", Enabled: true,
		DockerCap: store.CapYes, GithubCap: store.CapNo,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// 人工只能归档/封禁/退回待检；不能直接置 usable——那等于绕过探测和校验。
func TestPatchSourceStatus(t *testing.T) {
	st, h := newTestServer(t)
	ck := setupPanel(t, h)
	id := lifecycleSourceAPI(t, st)

	rec := patchJSON(h, "/api/sources/"+itoa(id), `{"status":"archived"}`, ck)
	if rec.Code != 200 {
		t.Fatalf("归档应成功，得到 %d %s", rec.Code, rec.Body.String())
	}
	s, _ := st.GetSource(id)
	if s.Status != "archived" || s.StatusReason != "手动归档" {
		t.Fatalf("归档后状态不对：%+v", s)
	}

	rec = patchJSON(h, "/api/sources/"+itoa(id), `{"status":"usable"}`, ck)
	if rec.Code != 400 {
		t.Fatalf("手动置 usable 应被拒，得到 %d %s", rec.Code, rec.Body.String())
	}
	if s, _ = st.GetSource(id); s.Status != "archived" {
		t.Fatalf("被拒后状态不该变，实际 %s", s.Status)
	}

	rec = patchJSON(h, "/api/sources/"+itoa(id), `{"status":"blocked"}`, ck)
	if rec.Code != 200 {
		t.Fatalf("封禁应成功，得到 %d", rec.Code)
	}
	if s, _ = st.GetSource(id); s.Status != "blocked" || s.StatusReason != "手动封禁" {
		t.Fatalf("封禁后状态不对：%+v", s)
	}
}

// 列表要把状态时间和原因带出去，面板才能解释「为什么是红的」。
func TestSourcesListExposesLifecycle(t *testing.T) {
	st, h := newTestServer(t)
	ck := setupPanel(t, h)
	id := lifecycleSourceAPI(t, st)
	if err := st.SetStatusWhy(id, "dead", "连续 3 次探测失败"); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sources", nil)
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("列表应 200，得到 %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"status_reason":"连续 3 次探测失败"`) {
		t.Fatalf("列表缺少 status_reason：%s", body)
	}
	if !strings.Contains(body, `"status_at":`) {
		t.Fatalf("列表缺少 status_at：%s", body)
	}
}
