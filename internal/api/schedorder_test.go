package api

import (
	"net/http"
	"testing"
)

// 手动调度只切模式、不选源时必须落库：面板上点「手动」就是这条路径。
func TestSchedOrderSwitchToManualWithoutOrders(t *testing.T) {
	st, h := newTestServer(t)
	newCk := setupPanel(t, h)

	rec := postJSON(h, "/api/sched-order", `{"mode":"manual","orders":{}}`, newCk)
	if rec.Code != http.StatusOK {
		t.Fatalf("切手动应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if got := st.GetSchedOrder().Mode; got != "manual" {
		t.Fatalf("模式应落库为 manual，实际 %q", got)
	}

	// 再切回自动也要落库，避免面板显示和库里不一致。
	rec = postJSON(h, "/api/sched-order", `{"mode":"auto","orders":{}}`, newCk)
	if rec.Code != http.StatusOK {
		t.Fatalf("切自动应 200，实际 %d", rec.Code)
	}
	if got := st.GetSchedOrder().Mode; got != "auto" {
		t.Fatalf("模式应落库为 auto，实际 %q", got)
	}
}
