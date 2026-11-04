package gateway

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

func privGateway(t *testing.T, priv PrivateOptions) *Gateway {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	return New(st, Options{
		Cache:   cs,
		Guard:   safehttp.New(safehttp.Options{AllowLoopback: true}),
		Private: priv,
	})
}

func privPlan(t *testing.T, raw string) *adapter.Plan {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return &adapter.Plan{
		Kind:         "release",
		Method:       "GET",
		Upstream:     u,
		CacheKey:     "release:o/r/v1/private.bin",
		Stream:       true,
		OfficialOnly: true,
		NoStore:      true,
		Header:       http.Header{"Authorization": {"token s3cret"}},
	}
}

// 白名单里配了自建中转，PAT 必须原样到它那里，客户端拿到完整内容。
func TestTrustedRelayGetsPAT(t *testing.T) {
	var gotAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte("private-bytes"))
	}))
	defer srv.Close()

	g := privGateway(t, PrivateOptions{
		Trusted: []TrustedRelay{{Name: "my-relay", URL: srv.URL + "/{url}"}},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://gw/gh/o/r/releases/download/v1/private.bin", nil)
	if !g.tryTrusted(rec, req, privPlan(t, "https://github.com/o/r/releases/download/v1/private.bin")) {
		t.Fatal("中转正常时应由它服务")
	}
	if v, _ := gotAuth.Load().(string); v != "token s3cret" {
		t.Fatalf("中转应收到原 PAT，实际 %q", v)
	}
	if rec.Body.String() != "private-bytes" {
		t.Fatalf("内容不一致: %q", rec.Body.String())
	}
	if rec.Header().Get("X-ML-Relay") != "my-relay" {
		t.Fatal("应标记实际使用的中转")
	}
}

// 中转 5xx → 不写任何响应，交给后面的官方并行回退。
func TestTrustedFailFallsBack(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "relay down", http.StatusBadGateway)
	}))
	defer srv.Close()

	g := privGateway(t, PrivateOptions{
		Trusted: []TrustedRelay{{Name: "my-relay", URL: srv.URL + "/{url}"}},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://gw/gh/o/r/releases/download/v1/private.bin", nil)
	if g.tryTrusted(rec, req, privPlan(t, "https://github.com/o/r/releases/download/v1/private.bin")) {
		t.Fatal("中转 5xx 不应被当成成功")
	}
	if rec.Body.Len() != 0 || rec.Code != 200 {
		t.Fatalf("回退前不能写响应，实际 code=%d body=%q", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatal("应只试一次中转")
	}
}

// PAT 无效（401）时回退没有意义，直接把结论给客户端。
func TestTrustedUnauthorizedReturnsDirectly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	g := privGateway(t, PrivateOptions{
		Trusted: []TrustedRelay{{Name: "my-relay", URL: srv.URL + "/{url}"}},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://gw/gh/o/r/releases/download/v1/private.bin", nil)
	if !g.tryTrusted(rec, req, privPlan(t, "https://github.com/o/r/releases/download/v1/private.bin")) {
		t.Fatal("401 应直接作为最终结果")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("应返回 401，实际 %d", rec.Code)
	}
}

// 连续失败到阈值后熔断，期间不再打中转，省掉每次的超时等待。
func TestTrustedBreaksAfterFailures(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()

	g := privGateway(t, PrivateOptions{
		Trusted:    []TrustedRelay{{Name: "my-relay", URL: srv.URL + "/{url}"}},
		BreakAfter: 2,
	})
	plan := privPlan(t, "https://github.com/o/r/releases/download/v1/private.bin")
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "http://gw/gh/o/r/releases/download/v1/private.bin", nil)
		g.tryTrusted(rec, req, plan)
	}
	if !g.privBroken("my-relay") {
		t.Fatal("连续 2 次失败后应熔断")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://gw/gh/o/r/releases/download/v1/private.bin", nil)
	g.tryTrusted(rec, req, plan)
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Fatalf("熔断期间不应再打中转，实际打了 %d 次", n)
	}
}

// 公开请求不受白名单影响，永远不会把匿名流量绕去自建站。
func TestTrustedIgnoresPublicPlan(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer srv.Close()
	g := privGateway(t, PrivateOptions{
		Trusted: []TrustedRelay{{Name: "my-relay", URL: srv.URL + "/{url}"}},
	})
	p := privPlan(t, "https://github.com/o/r/releases/download/v1/x.bin")
	p.OfficialOnly = false
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://gw/gh/o/r/releases/download/v1/x.bin", nil)
	if g.tryTrusted(rec, req, p) {
		t.Fatal("公开请求不应走私有白名单")
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("不应向中转发公开流量")
	}
}
