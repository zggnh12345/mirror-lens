package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 冷对象并发请求只该回源一次：后来者排队，等第一个下完直接读缓存。
func TestConcurrentColdStreamCoalesced(t *testing.T) {
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

	body := bytes.Repeat([]byte("x"), 4<<20) // 4 MiB
	var hits int32
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		// 慢一点，保证三个请求真的撞在一起。
		w.Header().Set("Content-Length", itoa(len(body)))
		w.Write(body)
	}))
	defer mirror.Close()
	addSchedSource(t, st, "mirror", mirror.URL, 50, 5000)

	g := New(st, Options{
		Cache:    cs,
		RawBase:  "https://raw.githubusercontent.com",
		MaxBytes: 16 << 20,
		Guard:    safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true, MaxCandidates: 3}))
	tok := token(t, st)

	const n = 3
	var wg sync.WaitGroup
	codes := make([]int, n)
	sizes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := doGH(g, tok, "http://gw/gh/o/r/releases/download/v1/cold.bin")
			codes[i], sizes[i] = rec.Code, rec.Body.Len()
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if codes[i] != http.StatusOK || sizes[i] != len(body) {
			t.Fatalf("第 %d 个请求应拿到完整 %d 字节，得到 %d / %d 字节", i, len(body), codes[i], sizes[i])
		}
	}
	if h := atomic.LoadInt32(&hits); h != 1 {
		t.Fatalf("%d 个并发冷请求应只回源 1 次，实际 %d 次", n, h)
	}
}
