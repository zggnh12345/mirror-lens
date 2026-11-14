package gateway

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/safehttp"
)

// D7：共享 Blob 缓存必须在 Host A / Host B 之间复用，但 Host 禁用时不能
// 通过缓存旁路；重新启用后仍应命中同一份内容。
func TestRegistrySharedBlobAcrossHostsAndDisable(t *testing.T) {
	f := newRegFixture(t, false)
	// 把两个逻辑 Host 都导到同一个测试上游，模拟“同一 digest 可从两个
	// Registry 取到”；这里只验证缓存与准入，不验证真实网络。
	f.gw.cli = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.EqualFold(r.URL.Host, "gcr.io") || strings.EqualFold(r.URL.Host, "registry.k8s.io") {
			u := *r.URL
			u.Scheme = "http"
			u.Host = strings.TrimPrefix(f.srv.URL, "http://")
			r2 := r.Clone(r.Context())
			r2.URL = &u
			r2.Host = u.Host
			return f.srv.Client().Transport.RoundTrip(r2)
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	f.gw.guard = safehttp.New(safehttp.Options{AllowLoopback: true})

	digest := f.up.blobDigest()
	targetA := "/v2/gcr.io/foo/bar/blobs/" + digest
	targetB := "/v2/registry.k8s.io/foo/bar/blobs/" + digest

	w1 := f.do(t, http.MethodGet, targetA, nil)
	if w1.Code != http.StatusOK || w1.Body.String() != string(blobBytes) {
		t.Fatalf("Host A 首次: %d %q", w1.Code, w1.Body.String())
	}
	if n := atomic.LoadInt32(&f.up.blobHits); n != 1 {
		t.Fatalf("Host A 首次应回源一次，实际 %d", n)
	}

	w2 := f.do(t, http.MethodGet, targetB, nil)
	if w2.Code != http.StatusOK || w2.Body.String() != string(blobBytes) {
		t.Fatalf("Host B 应命中共享缓存: %d %q", w2.Code, w2.Body.String())
	}
	if got := w2.Header().Get("X-ML-Cache"); got != "HIT" {
		t.Fatalf("Host B 应为 HIT，实际 %q", got)
	}
	if n := atomic.LoadInt32(&f.up.blobHits); n != 1 {
		t.Fatalf("Host B 命中缓存不应回源，实际 %d", n)
	}

	f.gw.disabledHosts = map[string]bool{"registry.k8s.io": true}
	if w := f.do(t, http.MethodGet, targetB, nil); w.Code != http.StatusNotFound {
		t.Fatalf("Host B 禁用后应 404，得到 %d %q", w.Code, w.Body.String())
	}
	if w := f.do(t, http.MethodGet, targetA, nil); w.Code != http.StatusOK {
		t.Fatalf("Host A 不应被 Host B 禁用影响，得到 %d", w.Code)
	}

	f.gw.disabledHosts = nil
	w5 := f.do(t, http.MethodGet, targetB, nil)
	if w5.Code != http.StatusOK || w5.Header().Get("X-ML-Cache") != "HIT" {
		t.Fatalf("Host B 重新启用后应命中缓存: %d %q", w5.Code, w5.Header().Get("X-ML-Cache"))
	}
	if n := atomic.LoadInt32(&f.up.blobHits); n != 1 {
		t.Fatalf("重新启用不应重新下载，实际 %d", n)
	}
}
