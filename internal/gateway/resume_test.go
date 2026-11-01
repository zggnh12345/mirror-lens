package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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

// 续传点必须对应真实存在的半截文件：只剩元信息时必须从头下，
// 按 Have 往后拼会得到中间少一段、却以「下载完成」入缓存的文件。
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

// 策略中途变了（超上限不再缓存）：上游仍只回 206 后半段，
// 但补发前缀与落不落盘无关，客户端必须拿到整份。
// 防的是「if cacheable { 补发前缀 }」：跳过补发会把后半段当整份发，
// 而且 Content-Length 还对得上。
func TestResumeReplaysPrefixWhenCacheNoLongerAllowed(t *testing.T) {
	f := newHTTPFixture(t, 0)
	target := f.srv.URL + "/f.bin"

	// 第一次：允许缓存，传到一半断开，留下 .part 和续传点。
	f.up.mu.Lock()
	f.up.truncateOnce = true
	f.up.mu.Unlock()
	w1 := f.req(t, target, nil)
	if got := w1.Body.Len(); got == 0 || got >= len(f.up.body) {
		t.Fatalf("第一次应只收到半份，实际 %d 字节", got)
	}
	if n := len(partialFiles(t, f.dir)); n != 1 {
		t.Fatalf("第一次应留下 1 个 .part，实际 %d", n)
	}

	// 第二次：上限调到比文件小，这次不允许缓存。
	gw := New(f.st, Options{
		Cache:    f.cs,
		RawBase:  f.srv.URL,
		MaxBytes: int64(len(f.up.body)) / 2,
		Guard:    safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	enc := url.PathEscape(target)
	r := httptest.NewRequest(http.MethodGet, "http://gw/http/"+enc, nil)
	r.Header.Set("Authorization", "Bearer "+f.token)
	w := httptest.NewRecorder()
	gw.ServeHTTPProxy(w, r)

	if !bytes.Equal(w.Body.Bytes(), f.up.body) {
		t.Fatalf("续传必须给出整份 %d 字节，实际 %d 字节（后半段被当成整份发了）",
			len(f.up.body), w.Body.Len())
	}
	if cl := w.Header().Get("Content-Length"); cl != strconv.Itoa(len(f.up.body)) {
		t.Fatalf("Content-Length 应声明 %d，实际 %q", len(f.up.body), cl)
	}
	// 已经不缓存了，半成品别留着过期。
	if n := len(partialFiles(t, f.dir)); n != 0 {
		t.Fatalf("不缓存的响应不该留下 .part，实际 %d", n)
	}
}
