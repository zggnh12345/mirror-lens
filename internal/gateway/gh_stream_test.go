package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 超过缓存上限的 release 资产：旧实现直接报错（客户端 502），
// 现在必须整份发出去，只是不落盘。
func TestLargeGHAssetStreamsInsteadOfFailing(t *testing.T) {
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

	body := bytes.Repeat([]byte("0123456789abcdef"), 2<<16) // 2 MiB
	var hits int32
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Length", itoa(len(body)))
		w.Write(body)
	}))
	defer mirror.Close()
	addSchedSource(t, st, "mirror", mirror.URL, 50, 5000)

	g := New(st, Options{
		Cache:    cs,
		RawBase:  "https://raw.githubusercontent.com",
		MaxBytes: 1 << 20, // 1 MiB：body 是它的两倍
		Guard:    safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true, MaxCandidates: 3}))
	tok := token(t, st)

	rec := doGH(g, tok, "http://gw/gh/o/r/releases/download/v1/big.bin")
	if rec.Code != http.StatusOK {
		t.Fatalf("大文件应正常下载，得到 %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.Len(); got != len(body) {
		t.Fatalf("应发完整 %d 字节，实际 %d", len(body), got)
	}
	if got := rec.Header().Get("X-ML-Cache"); got != "BYPASS" {
		t.Fatalf("超上限应标 BYPASS，得到 %q", got)
	}

	// 不落盘：第二次还得回源。
	rec2 := doGH(g, tok, "http://gw/gh/o/r/releases/download/v1/big.bin")
	if rec2.Code != http.StatusOK || rec2.Body.Len() != len(body) {
		t.Fatalf("第二次也应正常，得到 %d / %d 字节", rec2.Code, rec2.Body.Len())
	}
	// 不落盘：索引里不该有这条。每次请求会先发一次取头的 GET 判断大小，
	// 再走流式重发，所以上游计数是请求数的两倍——但真正传输的只有一遍。
	if e, _ := g.st.CacheGet("gh:o/r/releases/download/v1/big.bin"); e != nil {
		t.Fatalf("超上限的文件不该入缓存，却拿到 %+v", e)
	}
	if n := atomic.LoadInt32(&hits); n != 4 {
		t.Fatalf("两次请求应各探一次+流一次，上游计数 4，实际 %d", n)
	}
}

// 边下边发：上游只发了个开头就卡住，客户端这时就应该已经读到字节了。
func TestLargeGHAssetStreamsIncrementally(t *testing.T) {
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

	head := bytes.Repeat([]byte("h"), 64<<10)
	release := make(chan struct{})
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", itoa(2<<20))
		w.Write(head)
		w.(http.Flusher).Flush()
		<-release
		w.Write(bytes.Repeat([]byte("t"), 2<<20-len(head)))
	}))
	defer mirror.Close()
	addSchedSource(t, st, "mirror", mirror.URL, 50, 5000)

	g := New(st, Options{
		Cache:    cs,
		RawBase:  "https://raw.githubusercontent.com",
		MaxBytes: 1 << 20,
		Guard:    safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true, MaxCandidates: 3}))

	gw := httptest.NewServer(http.HandlerFunc(g.ServeGH))
	defer gw.Close()

	resp, err := http.Get(gw.URL + "/gh/o/r/releases/download/v1/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态 %d", resp.StatusCode)
	}

	buf := make([]byte, len(head))
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(resp.Body, buf)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("读首段失败: %v", err)
		}
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("上游还没发完就卡住了，客户端却一个字节都没拿到——没有边下边发")
	}
	close(release)
	rest, _ := io.ReadAll(resp.Body)
	if len(rest) != 2<<20-len(head) {
		t.Fatalf("剩余应 %d 字节，实际 %d", 2<<20-len(head), len(rest))
	}
}

// Range 请求直通上游：不为了切 10 个字节先把整包下下来。
func TestRangeRequestPassesThrough(t *testing.T) {
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

	body := []byte("0123456789abcdefghij")
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "a.bin", time.Unix(0, 0), bytes.NewReader(body))
	}))
	defer mirror.Close()
	addSchedSource(t, st, "mirror", mirror.URL, 50, 5000)

	g := New(st, Options{
		Cache:   cs,
		RawBase: "https://raw.githubusercontent.com",
		Guard:   safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true, MaxCandidates: 3}))

	req := httptest.NewRequest(http.MethodGet, "http://gw/gh/o/r/releases/download/v1/a.bin", nil)
	req.Header.Set("Range", "bytes=0-3")
	rec := httptest.NewRecorder()
	g.ServeGH(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("应返回 206，得到 %d", rec.Code)
	}
	if got := rec.Body.String(); got != "0123" {
		t.Fatalf("切片内容 %q", got)
	}
	if cr := rec.Header().Get("Content-Range"); !strings.HasPrefix(cr, "bytes 0-3/") {
		t.Fatalf("Content-Range 丢失: %q", cr)
	}
	if got := rec.Header().Get("X-ML-Cache"); got != "PASS" {
		t.Fatalf("Range 应直通，得到 X-ML-Cache=%q", got)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
