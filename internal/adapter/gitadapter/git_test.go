package gitadapter

import (
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
)

func plan(t *testing.T, method, target, proto string) (*adapter.Plan, error) {
	t.Helper()
	r := httptest.NewRequest(method, "http://gw"+target, nil)
	if proto != "" {
		r.Header.Set("Git-Protocol", proto)
	}
	return New(30 * time.Second).Plan(r)
}

func TestPlanDefaultsToHTTPS(t *testing.T) {
	p, err := plan(t, "GET", "/git/github.com/zggnh12345/mirror-lens.git/info/refs?service=git-upload-pack", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Upstream.String(); got != "https://github.com/zggnh12345/mirror-lens.git/info/refs?service=git-upload-pack" {
		t.Fatalf("上游拼错了: %s", got)
	}
	if !p.Cacheable {
		t.Fatal("info/refs 应该可缓存")
	}
}

func TestPlanExplicitHTTPAndPort(t *testing.T) {
	p, err := plan(t, "GET", "/git/http/git.internal:8081/team/repo.git/info/refs?service=git-upload-pack", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Upstream.String(); got != "http://git.internal:8081/team/repo.git/info/refs?service=git-upload-pack" {
		t.Fatalf("上游拼错了: %s", got)
	}
}

func TestPlanReceivePackRejected(t *testing.T) {
	_, err := plan(t, "GET", "/git/github.com/a/b.git/info/refs?service=git-receive-pack", "")
	if !errors.Is(err, adapter.ErrReadOnly) {
		t.Fatalf("push 应被拒，实际 %v", err)
	}
}

func TestPlanDumbHTTPRejected(t *testing.T) {
	_, err := plan(t, "GET", "/git/github.com/a/b.git/objects/ab/cdef", "")
	if err == nil {
		t.Fatal("dumb 协议应被拒")
	}
}

func TestPlanRejectsTraversal(t *testing.T) {
	for _, target := range []string{
		"/git/github.com/a/../b.git/info/refs?service=git-upload-pack",
		"/git/github.com//b.git/info/refs?service=git-upload-pack",
		"/git/github.com",
		"/git/",
	} {
		if _, err := plan(t, "GET", target, ""); err == nil {
			t.Fatalf("%s 应被拒", target)
		}
	}
}

func TestPlanCacheKeySeparatesProtocol(t *testing.T) {
	a, err := plan(t, "GET", "/git/github.com/a/b.git/info/refs?service=git-upload-pack", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := plan(t, "GET", "/git/github.com/a/b.git/info/refs?service=git-upload-pack", "version=2")
	if err != nil {
		t.Fatal(err)
	}
	if a.CacheKey == b.CacheKey {
		t.Fatalf("v1/v2 的 info/refs 不能共用一个缓存键: %s", a.CacheKey)
	}
	if b.Header.Get("Git-Protocol") != "version=2" {
		t.Fatal("Git-Protocol 没传给上游")
	}
}

func TestPlanPostNeverCached(t *testing.T) {
	p, err := plan(t, "POST", "/git/github.com/a/b.git/git-upload-pack", "version=2")
	if err != nil {
		t.Fatal(err)
	}
	if p.Cacheable {
		t.Fatal("upload-pack 不该缓存")
	}
}

func TestPlanZeroTTLDisablesCache(t *testing.T) {
	r := httptest.NewRequest("GET", "http://gw/git/github.com/a/b.git/info/refs?service=git-upload-pack", nil)
	p, err := New(0).Plan(r)
	if err != nil {
		t.Fatal(err)
	}
	if p.Cacheable {
		t.Fatal("TTL=0 时不该缓存")
	}
}

func TestPlanRejectsWriteMethods(t *testing.T) {
	r := httptest.NewRequest("PUT", "http://gw/git/github.com/a/b.git/info/refs", nil)
	if _, err := New(time.Minute).Plan(r); err == nil {
		t.Fatal("PUT 应被拒")
	}
}
