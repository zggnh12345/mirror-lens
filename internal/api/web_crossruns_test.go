package api

import (
	"os"
	"strings"
	"testing"
)

// 18f：Web 运行审计面板结构存在，且调用真实只读 API（不虚构数据）。
func TestWebCrossRunsAuditPanel(t *testing.T) {
	raw, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	src := string(raw)
	for _, frag := range []string{
		`id="crossRuns"`,
		`id="crossRunsBody"`,
		`loadCrossRuns()`,
		`api('/api/cross-runs?limit=30')`,
	} {
		if !strings.Contains(src, frag) {
			t.Errorf("index.html 缺少 18f 运行审计元素 %q", frag)
		}
	}
	// 面板展示的字段必须覆盖冻结稿要求的可解释维度
	for _, field := range []string{"vote_state", "authority", "cache_put", "backfill", "canceled", "nested"} {
		if !strings.Contains(src, "r."+field) {
			t.Errorf("审计面板未展示字段 %q", field)
		}
	}

	api, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatalf("read api.go: %v", err)
	}
	// 只读审计端点必须挂在登录墙后，且不带 csrf（GET 无副作用）
	if !strings.Contains(string(api), `mux.HandleFunc("/api/cross-runs", guard(s.crossRuns))`) {
		t.Error("/api/cross-runs 必须经过 guard（只读审计）")
	}
}
