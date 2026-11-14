package v2auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

func TestParseChallenge(t *testing.T) {
	got := ParseChallenge(`Bearer realm="https://auth.example/token",service="registry.example",scope="repository:a/b:pull,push"`)
	if got.Realm != "https://auth.example/token" || got.Service != "registry.example" {
		t.Fatalf("解析结果不对: %+v", got)
	}
	// scope 里的逗号不能被当成分隔符。
	if got.Scope != "repository:a/b:pull,push" {
		t.Fatalf("scope 被截断: %q", got.Scope)
	}
	if ParseChallenge(`Basic realm="x"`).Realm != "" {
		t.Fatal("非 Bearer 不应解析出 Realm")
	}
}

func TestRealmAllowed(t *testing.T) {
	a := New(nil, map[string][]string{
		"quay.io": {"https://quay.io"},
		"gcr.io":  {"https://*.googleapis.com"},
		"lscr.io": {"https://ghcr.io/"},
	})
	u := func(s string) *url.URL { p, _ := url.Parse(s); return p }

	if err := a.RealmAllowed("quay.io", u("https://quay.io/v2/auth")); err != nil {
		t.Fatalf("白名单内 Realm 被拒: %v", err)
	}
	if err := a.RealmAllowed("lscr.io", u("https://ghcr.io/v2/token")); err != nil {
		t.Fatalf("lscr.io 的实际 Realm 应放行: %v", err)
	}
	if err := a.RealmAllowed("gcr.io", u("https://storage.googleapis.com/v2/token")); err != nil {
		t.Fatalf("通配域名应放行: %v", err)
	}
	cases := []struct {
		name string
		host string
		url  string
	}{
		{"未配置的 Host", "unknown.io", "https://unknown.io/token"},
		{"别的域名", "quay.io", "https://evil.example/token"},
		{"同域但换协议", "quay.io", "http://quay.io/token"},
		{"子域仿冒", "quay.io", "https://quay.io.evil.example/token"},
		{"带 userinfo", "quay.io", "https://u:p@quay.io/token"},
	}
	for _, c := range cases {
		if err := a.RealmAllowed(c.host, u(c.url)); !errors.Is(err, ErrRealmDenied) {
			t.Fatalf("%s 应拒绝，得到 %v", c.name, err)
		}
	}
}

func TestRealmAllowedArtifactRegistryDynamic(t *testing.T) {
	a := New(nil, nil)
	u := func(s string) *url.URL { p, _ := url.Parse(s); return p }

	for _, host := range []string{"us-docker.pkg.dev", "europe-west1-docker.pkg.dev"} {
		if err := a.RealmAllowed(host, u("https://"+host+"/v2/token")); err != nil {
			t.Fatalf("%s 的同 Host Realm 应放行: %v", host, err)
		}
		if err := a.RealmAllowed(host, u("https://evil.example/token")); !errors.Is(err, ErrRealmDenied) {
			t.Fatalf("%s 的外部 Realm 应拒绝，得到 %v", host, err)
		}
		if err := a.RealmAllowed(host, u("http://"+host+"/token")); !errors.Is(err, ErrRealmDenied) {
			t.Fatalf("%s 的非 https Realm 应拒绝，得到 %v", host, err)
		}
	}
	// 不同 Artifact Registry Host 不能互相放行；lscr.io 的 Realm 修正
	// 不改变这条边界。
	if err := a.RealmAllowed("us-docker.pkg.dev", u("https://foo-docker.pkg.dev/token")); !errors.Is(err, ErrRealmDenied) {
		t.Fatalf("不同 Artifact Registry Host 不应互相放行，得到 %v", err)
	}
}

// 用实际 /v2/ 响应里的 WWW-Authenticate 值校验 Realm，而不是只验证
// 静态 map 自己和自己一致。这些字符串来自 2026-11-14 的匿名探测。
func TestRealChallengeRealms(t *testing.T) {
	a := New(nil, nil)
	cases := []struct {
		host      string
		challenge string
	}{
		{"gcr.io", `Bearer realm="https://gcr.io/v2/token",service=gcr.io`},
		{"registry.k8s.io", `Bearer realm="https://registry.k8s.io/token",service="registry.k8s.io"`},
		{"us-docker.pkg.dev", `Bearer realm="https://us-docker.pkg.dev/v2/token"`},
		{"quay.io", `Bearer realm="https://quay.io/v2/auth",service="quay.io"`},
		{"registry.gitlab.com", `Bearer realm="https://gitlab.com/jwt/auth",service="container_registry"`},
		{"public.ecr.aws", `Bearer realm="https://public.ecr.aws/token/",service="public.ecr.aws",scope="aws"`},
		{"cgr.dev", `Bearer realm="https://cgr.dev/token",service="cgr.dev"`},
		{"lscr.io", `Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:user/image:pull"`},
	}
	for _, c := range cases {
		ch := ParseChallenge(c.challenge)
		realm, err := url.Parse(ch.Realm)
		if err != nil {
			t.Fatalf("%s: Realm 解析失败: %v", c.host, err)
		}
		if err := a.RealmAllowed(c.host, realm); err != nil {
			t.Errorf("%s 的实际 Challenge 被拒: %v", c.host, err)
		}
	}
}

// MCR 的匿名 /v2/ 直接 200，没有 Bearer Challenge；不能因此去请求 token。
func TestMCRNoBearerChallenge(t *testing.T) {
	var tokenHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			atomic.AddInt32(&tokenHits, 1)
			w.Write([]byte(`{"token":"unexpected"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	a := New(srv.Client(), nil)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/", nil)
	resp, err := a.Do(context.Background(), "mcr.microsoft.com", req, CredentialSnapshot{}, "")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || atomic.LoadInt32(&tokenHits) != 0 {
		t.Fatalf("MCR 无 Challenge 应直接 200 且不请求 token，status=%d token=%d", resp.StatusCode, tokenHits)
	}
}

func TestDoFetchesTokenPerRequest(t *testing.T) {
	var tokenHits, manifestHits int32
	var lastAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			atomic.AddInt32(&tokenHits, 1)
			u, p, ok := r.BasicAuth()
			lastAuth = u + ":" + p
			if !ok || u != "robot" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.URL.Query().Get("scope") != "repository:library/alpine:pull" {
				t.Errorf("scope 未透传: %q", r.URL.Query().Get("scope"))
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"token":"tok"}`))
		case "/v2/":
			w.Header().Set("Www-Authenticate",
				`Bearer realm="`+srvURL(r)+`/token",service="test",scope="repository:library/alpine:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
		case "/v2/library/alpine/manifests/latest":
			atomic.AddInt32(&manifestHits, 1)
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.Header().Set("Www-Authenticate",
					`Bearer realm="`+srvURL(r)+`/token",service="test",scope="repository:library/alpine:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := New(srv.Client(), map[string][]string{"quay.io": {srv.URL}})
	snap := CredentialSnapshot{Host: "quay.io", Generation: 3, Digest: "d", Credentials: Credentials{Username: "robot", Password: "pw"}}
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/library/alpine/manifests/latest", nil)
		resp, err := a.Do(context.Background(), "quay.io", req, snap, "repository:library/alpine:pull")
		if err != nil {
			t.Fatalf("第 %d 次请求: %v", i+1, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("第 %d 次请求状态 %d", i+1, resp.StatusCode)
		}
	}
	if tokenHits != 2 {
		t.Fatalf("token 端点应被调用 2 次（每请求一次），实际 %d", tokenHits)
	}
	// 每次 Do 是「先 401，再带 Token 重试」两次 manifest 请求。
	if manifestHits != 4 || lastAuth != "robot:pw" {
		t.Fatalf("manifest 命中 %d，token 请求凭据 %q", manifestHits, lastAuth)
	}
}

// Realm 指向外部地址时必须拒绝，且不得去请求它。
func TestDoRejectsForeignRealm(t *testing.T) {
	var evilHits int32
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&evilHits, 1)
		w.Write([]byte(`{"token":"stolen"}`))
	}))
	defer evil.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Www-Authenticate", `Bearer realm="`+evil.URL+`/token",service="x"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	a := New(srv.Client(), map[string][]string{"quay.io": {srv.URL}})
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/library/alpine/manifests/latest", nil)
	_, err := a.Do(context.Background(), "quay.io", req, CredentialSnapshot{}, "")
	if !errors.Is(err, ErrRealmDenied) {
		t.Fatalf("外部 Realm 应被拒，得到 %v", err)
	}
	if evilHits != 0 {
		t.Fatalf("不应请求外部 Realm，实际 %d 次", evilHits)
	}
}

// token_method=POST 尚未实现：必须显式失败，且一个请求都不发。
func TestTokenMethodPostRejected(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Www-Authenticate", `Bearer realm="`+srvURL(r)+`/token",service="x"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	a := New(srv.Client(), map[string][]string{"quay.io": {srv.URL}})
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/", nil)
	snap := CredentialSnapshot{Host: "quay.io", Credentials: Credentials{
		Username: "u", Password: "p", TokenMethod: "POST", TokenAuth: "basic",
	}}
	_, err := a.Do(context.Background(), "quay.io", req, snap, "")
	if !errors.Is(err, ErrTokenMethodUnsupported) {
		t.Fatalf("POST 应显式拒绝，得到 %v", err)
	}
	// 首个 /v2/ 请求本身会发出，但 token 端点不能被访问。
	if hits != 1 {
		t.Fatalf("不应继续请求 token 端点，实际 %d 次", hits)
	}
}

// token_auth=none 即使快照里残留用户名密码，也不能附带 Basic。
func TestTokenAuthNoneSendsNoBasic(t *testing.T) {
	var tokenAuth string
	var seen bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokenAuth, seen = r.Header.Get("Authorization"), true
			w.Write([]byte(`{"token":"t"}`))
			return
		}
		w.Header().Set("Www-Authenticate", `Bearer realm="`+srvURL(r)+`/token",service="x"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	a := New(srv.Client(), map[string][]string{"quay.io": {srv.URL}})
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/", nil)
	snap := CredentialSnapshot{Host: "quay.io", Credentials: Credentials{
		Username: "u", Password: "p", TokenAuth: "none",
	}}
	resp, err := a.Do(context.Background(), "quay.io", req, snap, "")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !seen || tokenAuth != "" {
		t.Fatalf("token_auth=none 不应带 Basic，实际 %q", tokenAuth)
	}
}

// token_auth=basic 但快照没有凭据：显式报错，不去猜。
func TestTokenAuthBasicRequiresCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Www-Authenticate", `Bearer realm="`+srvURL(r)+`/token",service="x"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	a := New(srv.Client(), map[string][]string{"quay.io": {srv.URL}})
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/", nil)
	snap := CredentialSnapshot{Host: "quay.io", Credentials: Credentials{TokenAuth: "basic"}}
	if _, err := a.Do(context.Background(), "quay.io", req, snap, ""); !errors.Is(err, ErrCredentialsMissing) {
		t.Fatalf("缺凭据应报错，得到 %v", err)
	}
}

func TestTokenAuthUnknownValue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Www-Authenticate", `Bearer realm="`+srvURL(r)+`/token",service="x"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	a := New(srv.Client(), map[string][]string{"quay.io": {srv.URL}})
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/", nil)
	snap := CredentialSnapshot{Credentials: Credentials{Username: "u", TokenAuth: "digest"}}
	if _, err := a.Do(context.Background(), "quay.io", req, snap, ""); !errors.Is(err, ErrTokenAuthUnsupported) {
		t.Fatalf("未知认证方式应报错，得到 %v", err)
	}
}

func TestDoTokenFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Www-Authenticate", `Bearer realm="`+srvURL(r)+`/token",service="x"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	a := New(srv.Client(), map[string][]string{"quay.io": {srv.URL}})
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/", nil)
	if _, err := a.Do(context.Background(), "quay.io", req, CredentialSnapshot{}, ""); err == nil {
		t.Fatal("Token 端点 403 时应报错")
	}
}

func srvURL(r *http.Request) string { return "http://" + r.Host }
