package gateway

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/unified"
)

// 3.5.17 阶段1：/v2/ 请求必须建立 RequestScope，且每次底层派发都被 P 计量。
// 用注入的 PTransport 观察提交次数，验证「计量点包裹 t.base」真的接线了。
func TestRegistryRequestMetersP(t *testing.T) {
	var commits atomic.Int32
	f := newRegFixtureWith(t, false, func(o *Options) {
		o.PTransport = &unified.PTransport{OnCommit: func() { commits.Add(1) }}
	})
	w := f.do(t, http.MethodGet, "/v2/library/alpine/manifests/latest", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("manifest 请求应 200，得到 %d %s", w.Code, w.Body.String())
	}
	if commits.Load() == 0 {
		t.Fatal("上游派发没有经过 P 计量点：RequestScope 未挂到请求 Context")
	}
}

// 3.5.17 阶段1：P 耗尽且尚未发响应头时必须是 503 + no-store + 内部错误码，
// 不能折叠成 502/401，也不能继续回退上游。
func TestRegistryBudgetExhaustedIs503(t *testing.T) {
	f := newRegFixtureWith(t, true, func(o *Options) {
		// 夹具把 P 缩到 1：匿名请求消耗掉唯一额度，Token 获取必然耗尽。
		o.ScopeConfig = unified.ScopeConfig{PMax: 1}
	})
	w := f.do(t, http.MethodGet, "/v2/library/alpine/manifests/latest", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("预算耗尽应 503，得到 %d %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control=%q，want no-store", got)
	}
	if got := w.Header().Get(unified.HeaderError); got != unified.CodeBudgetExhausted {
		t.Fatalf("%s=%q，want %s", unified.HeaderError, got, unified.CodeBudgetExhausted)
	}
}

// 同一请求只有一个 Scope：并发请求各自独立计账，不互相吃额度。
func TestRegistryScopesArePerRequest(t *testing.T) {
	f := newRegFixtureWith(t, false, func(o *Options) {
		o.ScopeConfig = unified.ScopeConfig{PMax: 2}
	})
	for i := 0; i < 3; i++ {
		w := f.do(t, http.MethodGet, "/v2/library/alpine/manifests/latest", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("第 %d 次请求应 200（每请求独立 Scope），得到 %d %s", i+1, w.Code, w.Body.String())
		}
	}
}
