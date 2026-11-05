package gateway

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

const (
	opTotal = 2 << 20   // 2MB
	opChunk = 256 << 10 // 8 段
)

// opHarness 起一个只支持精确分段的假官方：truncate 返回 true 时只写半段就断。
// onSegment 仅对「长度恰好等于 chunk」的请求生效，用来区分分段请求和补齐请求。
func opHarness(t *testing.T, truncate func(off int64, attempt int, exactSeg bool) bool) (*Gateway, *adapter.Plan, []byte) {
	t.Helper()
	data := make([]byte, opTotal)
	for i := range data {
		data[i] = byte(i*17 + i>>7)
	}

	var mu sync.Mutex
	hits := map[int64]int{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var from, to int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &from, &to); err != nil {
			http.Error(w, "need range", 400)
			return
		}
		mu.Lock()
		hits[from]++
		n := hits[from]
		mu.Unlock()

		exact := to-from+1 == opChunk
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, opTotal))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusPartialContent)
		if truncate != nil && truncate(from, n, exact) {
			half := (to - from + 1) / 2
			w.Write(data[from : from+half])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return // 只写一半：客户端拿到 ErrUnexpectedEOF
		}
		w.Write(data[from : to+1])
	}))
	t.Cleanup(srv.Close)

	u, _ := url.Parse(srv.URL + "/o/r/releases/download/v1/big.bin")
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	g := New(st, Options{Cache: cs, Guard: safehttp.New(safehttp.Options{AllowLoopback: true})})
	g.par = ParOptions{Enabled: true, MinSize: 1 << 20, ChunkSize: opChunk, Workers: 4}

	plan := &adapter.Plan{
		Kind:         "release",
		Upstream:     u,
		CacheKey:     "release:o/r/v1/big.bin",
		Stream:       true,
		OfficialOnly: true,
		NoStore:      true,
	}
	return g, plan, data
}

func opRun(t *testing.T, g *Gateway, plan *adapter.Plan) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://gw/gh/o/r/releases/download/v1/big.bin", nil)
	ok := g.tryOfficialParallel(rec, req, plan)
	return rec, ok
}

// 某一段第一次被截断：靠重试把这一段重新拿回来，客户端应当收到完整的文件。
func TestOfficialParallelRetriesTruncatedSegment(t *testing.T) {
	g, plan, data := opHarness(t, func(off int64, attempt int, exact bool) bool {
		return off == opChunk && attempt == 1 && exact
	})
	rec, ok := opRun(t, g, plan)
	if !ok {
		t.Fatal("应走官方并行")
	}
	if rec.Body.Len() != opTotal {
		t.Fatalf("应输出 %d 字节，实际 %d", opTotal, rec.Body.Len())
	}
	if !bytes.Equal(rec.Body.Bytes(), data) {
		t.Fatal("重试后内容错位")
	}
}

// 某一段始终失败：不能把客户端掐断，必须从断点用单流补齐剩余字节。
func TestOfficialParallelFallsBackToSingleStream(t *testing.T) {
	var midOff = int64(3) * opChunk
	g, plan, data := opHarness(t, func(off int64, attempt int, exact bool) bool {
		return off == midOff && exact // 只有分段请求失败，补齐请求（更长 Range）不受影响
	})
	rec, ok := opRun(t, g, plan)
	if !ok {
		t.Fatal("应走官方并行")
	}
	if rec.Body.Len() != opTotal {
		t.Fatalf("补齐后应输出 %d 字节，实际 %d（客户端会报 curl 18）", opTotal, rec.Body.Len())
	}
	if !bytes.Equal(rec.Body.Bytes(), data) {
		t.Fatal("补齐后内容错位")
	}
	if rec.Header().Get("Content-Length") != fmt.Sprint(opTotal) {
		t.Fatalf("Content-Length 应为 %d，实际 %s", opTotal, rec.Header().Get("Content-Length"))
	}
}

// 第一段就失败：一个字节都还没写，应当干脆退回单流（返回 false），
// 让调用方用普通路径重试，而不是发一个必然不完整的响应。
func TestOfficialParallelFirstSegmentFailureFallsBack(t *testing.T) {
	g, plan, _ := opHarness(t, func(off int64, attempt int, exact bool) bool {
		return off == 0 && exact
	})
	rec, ok := opRun(t, g, plan)
	if ok {
		t.Fatal("首段失败应退回单流，不该自己发响应")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("退回前不应写出任何字节，实际 %d", rec.Body.Len())
	}
}

// 补齐路径本身也会断：按已写进度续传，最终仍然要凑满。
func TestOfficialParallelSingleStreamResumes(t *testing.T) {
	var midOff = int64(2) * opChunk
	var calls int32
	g, plan, data := opHarness(t, func(off int64, attempt int, exact bool) bool {
		if off == midOff && exact {
			return true // 分段永久失败 → 触发补齐
		}
		if off == midOff && !exact {
			// 补齐的第一次单流请求也断一次，之后恢复
			return atomic.AddInt32(&calls, 1) == 1
		}
		return false
	})
	rec, ok := opRun(t, g, plan)
	if !ok {
		t.Fatal("应走官方并行")
	}
	if rec.Body.Len() != opTotal || !bytes.Equal(rec.Body.Bytes(), data) {
		t.Fatalf("续传后应得到完整文件，实际 %d 字节", rec.Body.Len())
	}
}
