package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 3.5.17 B19 私有认证完整矩阵（第五轮补测）。
//
// 冻结边界（1.6 §B.7 / 11.x）：
//  1. 权威 Host 私有时不产生跨站候选调度；
//  2. 副本 Host 私有时不进入跨站候选；
//  3. Authority 凭据不得向副本 Host 索取/发送；
//  4. 副本凭据不得向 Authority 索取/发送；
//  5. 私有请求不写共享缓存；
//  6. 显式前缀指向私有 Host 时走既定「缓存前分流」（BYPASS，且第二次仍 BYPASS）。
//
// 这里的「凭据只按自己 Host 解析」用记录型 CredentialSource 直接断言：
// 记录被提问过的 Host 列表，任何跨 Host 提问都算泄漏路径。

type recordCreds struct {
	mu    sync.Mutex
	asked []string
	allow map[string]bool
}

func (r *recordCreds) CredentialView(host string) (CredentialView, error) {
	r.mu.Lock()
	r.asked = append(r.asked, host)
	r.mu.Unlock()
	if r.allow[host] {
		return CredentialView{Host: host, HasCredential: true, Username: "u-" + host, Password: "p"}, nil
	}
	return CredentialView{}, nil
}

func (r *recordCreds) setPassword(string) {}

func (r *recordCreds) askedHosts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.asked))
	copy(out, r.asked)
	return out
}

func (r *recordCreds) sawHost(host string) bool {
	for _, h := range r.askedHosts() {
		if strings.EqualFold(h, host) {
			return true
		}
	}
	return false
}

func newB19Fixture(t *testing.T) *replicaFixtureT {
	t.Helper()
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	return newReplicaFixture(t, body, body)
}

// 1 + 3：权威私有 → 零候选；凭据只向权威 Host 提问，绝不向副本 Host 提问。
func TestB19PrivateAuthorityNoCandidatesAndHostScopedCreds(t *testing.T) {
	f := newB19Fixture(t)
	rc := &recordCreds{allow: map[string]bool{"registry.k8s.io": true}}
	f.gw.privAuth = PrivateAuthOptions{
		Enabled: true, RequireToken: true,
		Hosts: map[string]bool{"registry.k8s.io": true},
		Creds: rc,
	}
	ts, _, err := f.gw.replicaTargets(context.Background(), f.plan, time.Now())
	if err != nil {
		t.Fatalf("候选解析: %v", err)
	}
	if len(ts) != 0 {
		t.Fatalf("权威 Host 私有时不得产生跨站候选，实际 %d 个", len(ts))
	}
	if _, handled, err := f.gw.crossRegistryManifest(f.ctx(), f.plan, nil); handled {
		t.Fatalf("权威私有时跨站路径不得形成结论（handled=%v err=%v）", handled, err)
	}
	if f.replica.Load() != 0 {
		t.Fatalf("权威私有时不得访问副本上游，实际 %d 次", f.replica.Load())
	}
	if rc.sawHost("quay.io") {
		t.Fatalf("权威凭据被向副本 Host 提问：%v", rc.askedHosts())
	}
}

// 2 + 4：副本私有 → 零候选；凭据不向权威 Host 提问；副本上游零访问。
func TestB19PrivateReplicaNoCandidatesAndHostScopedCreds(t *testing.T) {
	f := newB19Fixture(t)
	rc := &recordCreds{allow: map[string]bool{"quay.io": true}}
	f.gw.privAuth = PrivateAuthOptions{
		Enabled: true, RequireToken: true,
		Hosts: map[string]bool{"quay.io": true},
		Creds: rc,
	}
	ts, _, err := f.gw.replicaTargets(context.Background(), f.plan, time.Now())
	if err != nil {
		t.Fatalf("候选解析: %v", err)
	}
	for _, c := range ts {
		if c.sourceID == f.srcID {
			t.Fatal("私有副本 Host 不得进入跨站候选")
		}
	}
	if f.replica.Load() != 0 {
		t.Fatalf("私有副本不得被访问，实际 %d 次", f.replica.Load())
	}
	if rc.sawHost("registry.k8s.io") {
		t.Fatalf("副本凭据被向权威 Host 提问：%v", rc.askedHosts())
	}
}

// 5 + 6：显式前缀指向私有 Host 时，缓存查询前分流（BYPASS），两次都不写共享缓存。
// 用带 Registry adapter 的夹具：显式前缀 /v2/gcr.io/... 经真实 ParsedPath 进入私有分流。
func TestB19PrivatePrefixBypassesSharedCache(t *testing.T) {
	f := newRegFixtureWith(t, false, nil)
	// 让 gcr.io 的对外请求落到本地受控上游（不改产品逻辑，只在测试客户端上重写）。
	lo := f.srv.URL
	f.gw.cli = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.EqualFold(r.URL.Host, "gcr.io") {
			u := *r.URL
			u.Scheme, u.Host = "http", strings.TrimPrefix(lo, "http://")
			r2 := r.Clone(r.Context())
			r2.URL = &u
			return http.DefaultTransport.RoundTrip(r2)
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	rc := &recordCreds{allow: map[string]bool{"gcr.io": true}}
	f.gw.privAuth = PrivateAuthOptions{
		Enabled: true, RequireToken: true,
		Hosts: map[string]bool{"gcr.io": true},
		Creds: rc,
	}
	target := "/v2/gcr.io/library/alpine/manifests/latest"
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest(http.MethodGet, "http://gw"+target, nil)
		r.SetBasicAuth("x", f.token)
		w := httptest.NewRecorder()
		f.gw.ServeRegistry(w, r)
		if got := w.Header().Get("X-ML-Cache"); got != "BYPASS" {
			t.Fatalf("第 %d 次：私有前缀必须 BYPASS，实际 %q（status=%d body=%s）", i+1, got, w.Code, w.Body.String())
		}
	}
	entries, err := f.st.CacheList(100)
	if err != nil {
		t.Fatalf("CacheList: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("私有请求不得写共享缓存，实际 %d 条", len(entries))
	}
	if rc.sawHost("quay.io") {
		t.Fatalf("私有凭据被问到无关 Host：%v", rc.askedHosts())
	}
}
