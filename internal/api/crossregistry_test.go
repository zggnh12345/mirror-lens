package api

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// B17：默认顺序回退、两档可切、竞速必须显式开启、匿名拒绝。
func TestCrossRegistryAPIMode(t *testing.T) {
	st, h := newTestServer(t)

	if rec := get(h, "/api/cross-registry", nil, nil); rec.Code != 401 && rec.Code != 403 {
		t.Fatalf("匿名读应被拒，得到 %d", rec.Code)
	}
	if rec := putJSON(h, "/api/cross-registry", `{"mode":"race"}`, nil); rec.Code != 401 && rec.Code != 403 {
		t.Fatalf("匿名写应被拒，得到 %d", rec.Code)
	}

	ck := setupPanel(t, h)
	rec := get(h, "/api/cross-registry", nil, ck)
	if rec.Code != 200 {
		t.Fatalf("读取失败 %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Mode       string  `json:"mode"`
		Configured string  `json:"configured"`
		Selected   []int64 `json:"selected"`
		Starred    []int64 `json:"starred"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if out.Mode != "sequential" {
		t.Fatalf("默认模式必须是顺序回退，实际 %q", out.Mode)
	}
	if out.Configured != "" {
		t.Fatalf("未设置时 configured 应为空，实际 %q", out.Configured)
	}
	if len(out.Selected) != 0 {
		t.Fatalf("cross_registry_selected_sources 默认必须为空，实际 %v", out.Selected)
	}

	// 显式切到竞速：立即生效并落库。
	if rec := putJSON(h, "/api/cross-registry", `{"mode":"race"}`, ck); rec.Code != 200 {
		t.Fatalf("切竞速失败 %d %s", rec.Code, rec.Body.String())
	}
	if got := st.CrossRegistryMode(); got != "race" {
		t.Fatalf("模式未落库: %q", got)
	}
	rec = get(h, "/api/cross-registry", nil, ck)
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Mode != "race" {
		t.Fatalf("切换后应读回 race，实际 %q", out.Mode)
	}

	// 非法值拒绝。
	if rec := putJSON(h, "/api/cross-registry", `{"mode":"turbo"}`, ck); rec.Code != 400 {
		t.Fatalf("非法模式应 400，实际 %d", rec.Code)
	}
	// 审计：模式切换必须留痕。
	if n := auditCount(t, st, "cross_registry_mode"); n == 0 {
		t.Fatal("模式切换没有审计记录")
	}
}

// B20：勾选必须 ⊆ 星标；未星标拒绝；空列表合法且表示没有跨站候选。
func TestCrossRegistryAPISelection(t *testing.T) {
	st, h := newTestServer(t)
	ck := setupPanel(t, h)

	id, err := st.AddSource(store.Source{BaseURL: "https://mirror.example.org", Name: "m1", Enabled: true})
	if err != nil {
		t.Fatalf("加源: %v", err)
	}
	// 未星标 → 拒绝。
	if rec := putJSON(h, "/api/cross-registry", `{"selected":[`+itoa64(id)+`]}`, ck); rec.Code != 400 {
		t.Fatalf("未星标源应被拒，实际 %d %s", rec.Code, rec.Body.String())
	}
	starred := true
	if err := st.UpdateSource(id, nil, nil, nil, &starred); err != nil {
		t.Fatalf("加星标: %v", err)
	}
	if rec := putJSON(h, "/api/cross-registry", `{"selected":[`+itoa64(id)+`]}`, ck); rec.Code != 200 {
		t.Fatalf("星标源应可勾选，实际 %d %s", rec.Code, rec.Body.String())
	}
	if got := st.EffectiveCrossSelected(); !got[id] {
		t.Fatalf("勾选未生效: %v", got)
	}
	// 采集后撤星，旧条目读时过滤（不留幽灵资格）。
	off := false
	if err := st.UpdateSource(id, nil, nil, nil, &off); err != nil {
		t.Fatalf("撤星: %v", err)
	}
	if got := st.EffectiveCrossSelected(); got[id] {
		t.Fatal("撤星后勾选必须失效")
	}
	// 清空合法。
	if rec := putJSON(h, "/api/cross-registry", `{"selected":[]}`, ck); rec.Code != 200 {
		t.Fatalf("清空失败 %d %s", rec.Code, rec.Body.String())
	}
	if got := st.CrossSelectedSources(); len(got) != 0 {
		t.Fatalf("清空后应为空，实际 %v", got)
	}
}

func itoa64(v int64) string {
	return strconv.FormatInt(v, 10)
}

// auditCount 统计指定 action 的审计行数（耐久表，不是异步队列）。
func auditCount(t *testing.T, st *store.Store, action string) int {
	t.Helper()
	n, err := st.CountAuditByAction(action)
	if err != nil {
		t.Fatalf("审计统计: %v", err)
	}
	return n
}
