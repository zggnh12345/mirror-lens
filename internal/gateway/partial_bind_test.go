package gateway

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/cache"
)

func newPartialFixture(t *testing.T) (*Gateway, *cache.Store, *adapter.Plan) {
	t.Helper()
	cs, err := cache.New(t.TempDir() + "/c")
	if err != nil {
		t.Fatal(err)
	}
	g := &Gateway{cache: cs, now: time.Now}
	u, _ := url.Parse("http://new.example/v2/blob/sha256:abc")
	plan := &adapter.Plan{
		Kind:     "docker",
		CacheKey: "docker:blob:sha256:" + strings.Repeat("a", 64),
		Upstream: u,
	}
	return g, cs, plan
}

// 换源后留下的 .part 必须绑定真正在供数的那个源。
func TestPartialBindsCurrentSource(t *testing.T) {
	g, cs, plan := newPartialFixture(t)
	partPath, _ := cs.PartialPaths(plan.CacheKey)
	f, err := os.Create(partPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(make([]byte, 400)); err != nil {
		t.Fatal(err)
	}
	resp := &http.Response{Header: hdr(
		"ETag", `"v2"`,
		"Last-Modified", "Wed, 01 Jan 2026 00:00:00 GMT",
		"Content-Type", "application/octet-stream",
	)}
	g.keepPartial(plan, resp, 1000, 400, f)

	m, ok := cs.ReadPartial(plan.CacheKey)
	if !ok {
		t.Fatal("续传元数据没写下来")
	}
	if m.URL != plan.Upstream.String() {
		t.Fatalf("URL=%s，应绑定当前源 %s", m.URL, plan.Upstream)
	}
	if m.ETag != `"v2"` || m.LastModified == "" {
		t.Fatalf("验证器没跟着当前源: etag=%q lm=%q", m.ETag, m.LastModified)
	}
}

// 靠摘要兜底的换源，断流时不能留下无法证明版本的 .part。
func TestUnverifiedPartialDropped(t *testing.T) {
	g, cs, plan := newPartialFixture(t)
	partPath, _ := cs.PartialPaths(plan.CacheKey)
	f, err := os.Create(partPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(make([]byte, 400)); err != nil {
		t.Fatal(err)
	}
	resp := &http.Response{Header: hdr("ETag", `"v2"`)}
	g.savePartialOrDrop(plan, resp, 1000, 400, f, true)

	if _, ok := cs.ReadPartial(plan.CacheKey); ok {
		t.Fatal("未验证版本的前缀不应留下续传点")
	}
	if fi, err := os.Stat(partPath); err == nil && fi.Size() > 0 {
		t.Fatalf("数据文件也应清掉，实际 %d 字节", fi.Size())
	}
}
