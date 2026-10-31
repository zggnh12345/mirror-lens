package gateway

import (
	"bytes"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

func partialFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, "cache", "partial", "*.part"))
	return m
}

func partialMetas(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, "cache", "partial", "*.json"))
	return m
}

// 续传点必须对应一份真实存在的半截文件。数据文件被清掉、只剩元信息时，
// 下一次请求必须从头下，不能按元信息里的 Have 往后拼——那样拼出来的文件
// 长度对得上、中间少一段，而且会以「下载完成」的样子入缓存。
func TestResumeRequiresExistingPartFile(t *testing.T) {
	f := newHTTPFixture(t, 0)
	target := f.srv.URL + "/f.bin"

	f.up.mu.Lock()
	f.up.truncateOnce = true
	f.up.mu.Unlock()

	w1 := f.req(t, target, nil)
	if w1.Body.Len() >= len(f.up.body) {
		t.Fatalf("第一次应该是半截，实际 %d 字节", w1.Body.Len())
	}
	if len(f.partials(t)) != 1 {
		t.Fatal("断流后应留下续传点")
	}
	// 模拟半截文件被清理（janitor、手工删、或盘上丢了），元信息还在。
	parts := partialFiles(t, f.dir)
	if len(parts) != 1 {
		t.Fatalf("应有 1 个 .part，实际 %d", len(parts))
	}
	if err := os.Remove(parts[0]); err != nil {
		t.Fatal(err)
	}

	w2 := f.req(t, target, nil)
	if got := w2.Header().Get("X-ML-Cache"); got == "RESUME" {
		t.Fatalf("没有半截文件还去续传，X-ML-Cache=%q", got)
	}
	if !bytes.Equal(w2.Body.Bytes(), f.up.body) {
		t.Fatalf("重下后内容不完整：%d/%d 字节", w2.Body.Len(), len(f.up.body))
	}
	f.up.mu.Lock()
	lr := f.up.lastRange
	f.up.mu.Unlock()
	if lr != "" {
		t.Fatalf("不该带 Range 回源，实际 %q", lr)
	}
	if !f.cs.Has(sha(f.up.body)) {
		t.Fatal("重下完成后应落盘")
	}
}

// 半截文件比元信息声明的短（写到一半崩了、或被人截断）：长度对不上就不能续。
func TestResumeRejectsShortPartFile(t *testing.T) {
	f := newHTTPFixture(t, 0)
	target := f.srv.URL + "/f.bin"

	f.up.mu.Lock()
	f.up.truncateOnce = true
	f.up.mu.Unlock()
	f.req(t, target, nil)

	parts := partialFiles(t, f.dir)
	if len(parts) != 1 {
		t.Fatalf("应有 1 个 .part，实际 %d", len(parts))
	}
	fi, err := os.Stat(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(parts[0], fi.Size()/2); err != nil {
		t.Fatal(err)
	}

	w2 := f.req(t, target, nil)
	if got := w2.Header().Get("X-ML-Cache"); got == "RESUME" {
		t.Fatalf("半截文件长度不符还去续传，X-ML-Cache=%q", got)
	}
	if !bytes.Equal(w2.Body.Bytes(), f.up.body) {
		t.Fatalf("重下后内容不完整：%d/%d 字节", w2.Body.Len(), len(f.up.body))
	}
	if !f.cs.Has(sha(f.up.body)) {
		t.Fatal("重下完成后应落盘")
	}
}

// 这一轮根本没往盘上写（不可缓存、写盘失败转透传），就不该凭空造一个续传点。
func TestKeepPartialWithoutFileWritesNoMeta(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	g := New(st, Options{Cache: cs, Guard: safehttp.New(safehttp.Options{AllowLoopback: true})})

	u, _ := url.Parse("https://up.example/f.bin")
	plan := &adapter.Plan{Kind: "release", Upstream: u, CacheKey: "release:o/r/f.bin"}
	resp := &http.Response{Header: http.Header{"Etag": {`"v1"`}}}

	// part == nil：这次没有 .part 文件可续。
	g.keepPartial(plan, resp, 1000, 500, nil)

	if metas := partialMetas(t, dir); len(metas) != 0 {
		t.Fatalf("没有半截文件却写了续传点: %v", metas)
	}
	if _, ok := cs.ReadPartial(plan.CacheKey); ok {
		t.Fatal("ReadPartial 不该认出续传点")
	}
}

// 元信息还在、文件也还在，但这一轮没写盘：不能拿客户端的已发字节数去覆盖
// 元信息（那会把 Have 写成比文件实际长度更大的值）。
func TestKeepPartialNilKeepsExistingMeta(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	g := New(st, Options{Cache: cs, Guard: safehttp.New(safehttp.Options{AllowLoopback: true})})

	u, _ := url.Parse("https://up.example/f.bin")
	plan := &adapter.Plan{Kind: "release", Upstream: u, CacheKey: "release:o/r/f.bin"}
	resp := &http.Response{Header: http.Header{"Etag": {`"v1"`}}}

	if err := cs.WritePartial(plan.CacheKey, cache.PartialMeta{
		URL: u.String(), ETag: `"v1"`, Size: 1000, Have: 400, Updated: 1,
	}); err != nil {
		t.Fatal(err)
	}
	part, _ := cs.PartialPaths(plan.CacheKey)
	if err := os.WriteFile(part, bytes.Repeat([]byte("x"), 400), 0o644); err != nil {
		t.Fatal(err)
	}

	g.keepPartial(plan, resp, 1000, 0, nil)

	m, ok := cs.ReadPartial(plan.CacheKey)
	if !ok || m.Have != 400 {
		t.Fatalf("已有续传点被改坏了: %+v ok=%v", m, ok)
	}
}
