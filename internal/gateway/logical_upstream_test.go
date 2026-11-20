package gateway

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestCandidateRewriteUsesLogicalUpstream 锁定 P2-2 修复。
//
// 流式换源会把 Plan.Upstream 改写成本次实际供数源；候选推导若继续拿它当
// Rewrite 基址，registry:<host> 的 host 校验就会失败（base.Host 已是镜像站），
// 候选被全部跳过，换源退化成「只剩官方源」。
//
// 因此候选推导必须使用 LogicalUpstream（创建计划时的官方地址）。
func TestCandidateRewriteUsesLogicalUpstream(t *testing.T) {
	f := newMOFixture(t)
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer mirror.Close()
	id := f.addRegistrySource(t, mirror, "native")

	req := httptest.NewRequest(http.MethodGet, "http://gw/v2/"+f.host+"/library/alpine/manifests/latest", nil)
	plan, err := f.ad.Plan(req)
	if err != nil {
		t.Fatal(err)
	}
	if plan.LogicalUpstream == nil {
		t.Fatal("创建计划时应记录 LogicalUpstream（官方基址）")
	}

	// 模拟流式路径：Upstream 已被改写成本次供数源，LogicalUpstream 保持官方。
	switched, err := url.Parse("https://mirror-in-use.example")
	if err != nil {
		t.Fatal(err)
	}
	switched.Path = plan.Upstream.Path
	plan.Upstream = switched

	targets := f.gw.targets(plan, nil)
	found := false
	for _, tg := range targets {
		if tg.c != nil && tg.c.ID == id {
			found = true
			if tg.u.Host != mustHostOf(t, mirror.URL) {
				t.Fatalf("候选目标 host=%q want %q", tg.u.Host, mustHostOf(t, mirror.URL))
			}
			break
		}
	}
	if !found {
		t.Fatalf("换源后候选被跳过：Rewrite 未使用 LogicalUpstream（targets=%d）", len(targets))
	}
}

func mustHostOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		t.Fatalf("解析地址失败: %q %v", raw, err)
	}
	return u.Host
}
