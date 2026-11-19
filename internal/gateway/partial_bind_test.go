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

// G0：resume 来源必须与实际回源来源一致。绑定别的源的 .part 一律丢弃，
// 否则会把上一个源的半截内容当成本源的续传基点。
func TestPickPartialRejectsOtherSource(t *testing.T) {
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
		"ETag", `"v1"`,
		"Content-Type", "application/octet-stream",
	)}
	g.keepPartial(plan, resp, 1000, 400, f)

	if got := g.pickPartial(plan.CacheKey, plan.Upstream); got == nil {
		t.Fatal("同源续传应被接受")
	}
	other, _ := url.Parse("http://other.example/v2/blob/sha256:abc")
	if got := g.pickPartial(plan.CacheKey, other); got != nil {
		t.Fatalf("跨源 .part 必须丢弃，实际 URL=%s", got.URL)
	}
	if _, ok := cs.ReadPartial(plan.CacheKey); ok {
		t.Fatal("跨源 .part 元数据未清理")
	}
	if _, err := os.Stat(partPath); !os.IsNotExist(err) {
		t.Fatalf("跨源 .part 数据文件未清理: %v", err)
	}
}
