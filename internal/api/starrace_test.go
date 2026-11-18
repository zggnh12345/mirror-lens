package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

func putJSON(h http.Handler, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if testCSRF != "" {
		req.Header.Set("X-CSRF-Token", testCSRF)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// T49/T50：未登录、无 CSRF 一律拒绝；带会话才可读写（单 admin 模型，
// 没有角色分级，不写「已登录但无权限」的用例）。
func TestStarRaceAPIAuth(t *testing.T) {
	_, h := newTestServer(t)
	if rec := get(h, "/api/star-race", nil, nil); rec.Code != 401 && rec.Code != 403 {
		t.Fatalf("匿名读应被拒，得到 %d", rec.Code)
	}
	if rec := putJSON(h, "/api/star-race/quay.io", `{"enabled":true}`, nil); rec.Code != 401 && rec.Code != 403 {
		t.Fatalf("匿名写应被拒，得到 %d", rec.Code)
	}
}

func TestStarRaceAPIReadWrite(t *testing.T) {
	st, h := newTestServer(t)
	ck := setupPanel(t, h)

	rec := get(h, "/api/star-race", nil, ck)
	if rec.Code != 200 {
		t.Fatalf("读取失败 %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Hosts []raceHostView `json:"hosts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(out.Hosts) != 0 {
		t.Fatalf("默认不应列出任何 Host：%v", out.Hosts)
	}

	rec = putJSON(h, "/api/star-race/quay.io", `{"enabled":true}`, ck)
	if rec.Code != 200 {
		t.Fatalf("写失败 %d %s", rec.Code, rec.Body.String())
	}
	if !st.StarRaceEnabled("quay.io") {
		t.Fatal("写入后应开启")
	}
	// 别名必须收敛到同一个键。
	if !st.StarRaceEnabled("registry-1.docker.io") == false {
		t.Fatal("quay.io 的开关不应影响 docker hub")
	}

	rec = get(h, "/api/star-race", nil, ck)
	var out2 struct {
		Hosts []raceHostView `json:"hosts"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out2)
	if len(out2.Hosts) != 1 || out2.Hosts[0].Host != "quay.io" || !out2.Hosts[0].HostSwitch {
		t.Fatalf("应只列出 quay.io 且开关为开：%+v", out2.Hosts)
	}
	if out2.Hosts[0].Note == "" {
		t.Fatal("必须给出就绪说明，不能只有一个布尔")
	}

	if rec := putJSON(h, "/api/star-race/quay.io", `{"enabled":false}`, ck); rec.Code != 200 {
		t.Fatalf("关闭失败 %d", rec.Code)
	}
	if st.StarRaceEnabled("quay.io") {
		t.Fatal("关闭后不应仍为开")
	}
}

// 三个概念必须分开：开关开但星标源不足时 ready=false 且 eligible<2。
func TestStarRaceThreeConceptsSeparate(t *testing.T) {
	st, h := newTestServer(t)
	ck := setupPanel(t, h)

	id, err := st.AddSource(store.Source{
		BaseURL: "https://mirror.example.org", Name: "示例", Origin: "manual", Enabled: true,
	})
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	if err := st.UpdateSource(id, nil, nil, nil, apiBoolPtr(true)); err != nil {
		t.Fatalf("星标: %v", err)
	}
	if rec := putJSON(h, "/api/star-race/quay.io", `{"enabled":true}`, ck); rec.Code != 200 {
		t.Fatalf("写失败 %d", rec.Code)
	}
	rec := get(h, "/api/star-race", nil, ck)
	var out struct {
		Hosts []raceHostView `json:"hosts"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	var v *raceHostView
	for i := range out.Hosts {
		if out.Hosts[i].Host == "quay.io" {
			v = &out.Hosts[i]
		}
	}
	if v == nil {
		t.Fatalf("应列出 quay.io 行：%+v", out.Hosts)
	}
	if !v.HostSwitch {
		t.Fatal("host_switch 应为 true")
	}
	if v.Ready {
		t.Fatal("只有一个源不可能就绪")
	}
	if v.EligibleSources >= 2 {
		t.Fatalf("可竞速源数不应 ≥2：%d", v.EligibleSources)
	}
}

// T49：星标变更写管理审计；探测路径不得自动星标。
func TestStarRaceAuditOnStarChange(t *testing.T) {
	st, h := newTestServer(t)
	ck := setupPanel(t, h)

	id, err := st.AddSource(store.Source{
		BaseURL: "https://mirror.example.org", Name: "示例", Origin: "manual", Enabled: true,
	})
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	patch := func(body string) {
		req := httptest.NewRequest(http.MethodPatch, "/api/sources/"+itoa(id), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", testCSRF)
		req.AddCookie(ck)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("PATCH 失败 %d %s", rec.Code, rec.Body.String())
		}
	}
	patch(`{"preferred":true}`)
	patch(`{"preferred":true}`) // 值没变不应再记一条

	var stars []store.AuditEvent
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		evs, _ := st.ListAudit(50)
		stars = stars[:0]
		for _, e := range evs {
			if e.Action == "source_star" && e.Target == itoa(id) {
				stars = append(stars, e)
			}
		}
		if len(stars) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(stars) != 1 {
		t.Fatalf("星标变更应只记一条审计，实得 %d", len(stars))
	}
	if !strings.Contains(stars[0].Detail, "race_star=true") {
		t.Fatalf("审计 detail 应写明星标状态：%q", stars[0].Detail)
	}
}

func apiBoolPtr(b bool) *bool { return &b }
