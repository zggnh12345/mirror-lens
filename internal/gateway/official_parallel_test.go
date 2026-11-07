package gateway

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 私有内容不能外发凭据，只能打官方；这条路径用同源多 Range 并发提速。
func TestOfficialParallelSegments(t *testing.T) {
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

	const total = 4 << 20
	data := make([]byte, total)
	for i := range data {
		data[i] = byte(i*31 + i>>9)
	}
	var reqs, conc, maxConc int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqs, 1)
		c := atomic.AddInt32(&conc, 1)
		defer atomic.AddInt32(&conc, -1)
		for {
			old := atomic.LoadInt32(&maxConc)
			if c <= old || atomic.CompareAndSwapInt32(&maxConc, old, c) {
				break
			}
		}
		var from, to int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &from, &to); err != nil {
			http.Error(w, "need range", 400)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, total))
		w.Header().Set("Content-Type", "application/octet-stream")
		// 真实源分段请求有 RTT；CI 单核上响应快到请求来不及重叠，
		// 会让并发观测退化成 1。用固定延迟造出必然重叠的窗口。
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[from : to+1])
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL + "/o/r/releases/download/v1/big.bin")

	g := New(st, Options{Cache: cs, Guard: safehttp.New(safehttp.Options{AllowLoopback: true})})
	g.par = ParOptions{Enabled: true, MinSize: 1 << 20, ChunkSize: 512 << 10, Workers: 4}

	plan := &adapter.Plan{
		Kind:         "release",
		Upstream:     u,
		CacheKey:     "release:o/r/v1/big.bin",
		Stream:       true,
		OfficialOnly: true, // 私有：只准打官方
		NoStore:      true,
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://gw/gh/o/r/releases/download/v1/big.bin", nil)

	if !g.tryOfficialParallel(rec, req, plan) {
		t.Fatal("满足条件时应走官方并行")
	}
	if rec.Body.Len() != total {
		t.Fatalf("应输出 %d 字节，实际 %d", total, rec.Body.Len())
	}
	if !bytes.Equal(rec.Body.Bytes(), data) {
		t.Fatal("分片顺序错位，内容与原文件不一致")
	}
	if got := atomic.LoadInt32(&reqs); got < 4 {
		t.Fatalf("4MB/512KB 应至少 8 个分段请求，实际 %d", got)
	}
	if got := atomic.LoadInt32(&maxConc); got < 2 {
		t.Fatalf("应并发发分段请求，实际最大并发 %d", got)
	}
	// 私有内容不允许落盘。
	if e, _ := g.st.CacheGet(plan.CacheKey); e != nil {
		t.Fatal("私有内容不应写入缓存")
	}
}
