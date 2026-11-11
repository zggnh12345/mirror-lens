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
		"lscr.io": {"https://lscr.io/"},
	})
	u := func(s string) *url.URL { p, _ := url.Parse(s); return p }

	if err := a.RealmAllowed("quay.io", u("https://quay.io/v2/auth")); err != nil {
		t.Fatalf("白名单内 Realm 被拒: %v", err)
	}
	if err := a.RealmAllowed("lscr.io", u("https://lscr.io/v2/token")); err != nil {
		t.Fatalf("尾部斜杠应被容忍: %v", err)
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

// 每条资源请求独立换 Token：两次 Do 必须打两次 token 端点。
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
