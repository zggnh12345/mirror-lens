package gateway

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

func relayGatewayMulti(t *testing.T, urls []string) (*Gateway, *adapter.Plan) {
	t.Helper()
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
	list := make([]TrustedRelay, 0, len(urls))
	for i, u := range urls {
		list = append(list, TrustedRelay{ID: int64(i + 1), Name: fmt.Sprintf("站%d", i+1), URL: u})
	}
	g.SetTrusted(list)
	up, _ := url.Parse("https://github.com/o/r/releases/download/v1/big.bin")
	return g, &adapter.Plan{
		Kind:         "release",
		Upstream:     up,
		Stream:       true,
		OfficialOnly: true,
		NoStore:      true,
		Header:       http.Header{"Authorization": {"token test"}},
	}
}

// 胜者交出响应头后，正文还要读大文件。以前胜出即 cancel 共享 ctx，
// 胜者自己的 body 会被掐断——这里必须完整读到 20MB。
func TestTrustedWinnerBodySurvivesRace(t *testing.T) {
	data := relayData(20 << 20)
	var canceled int32
	winner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		for i := 0; i < len(data); i += 256 << 10 {
			end := i + 256<<10
			if end > len(data) {
				end = len(data)
			}
			if _, err := w.Write(data[i:end]); err != nil {
				atomic.StoreInt32(&canceled, 1)
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(2 * time.Millisecond)
		}
	}))
	defer winner.Close()
	loser := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer loser.Close()

	g, plan := relayGatewayMulti(t, []string{winner.URL, loser.URL})
	rec := relayRun(t, g, plan)
	if atomic.LoadInt32(&canceled) != 0 {
		t.Fatal("胜者的请求被取消了：正文读取会断在缓冲区大小附近")
	}
	if rec.Body.Len() != len(data) {
		t.Fatalf("应收到 %d 字节，实际 %d", len(data), rec.Body.Len())
	}
	if !bytes.Equal(rec.Body.Bytes(), data) {
		t.Fatal("内容不一致")
	}
}

// 连续竞速不能留下成串的 goroutine。
func TestTrustedRacesDoNotLeakGoroutines(t *testing.T) {
	data := relayData(64 << 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	}))
	defer srv.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer slow.Close()

	g, plan := relayGatewayMulti(t, []string{srv.URL, slow.URL})
	for i := 0; i < 20; i++ {
		relayRun(t, g, plan)
	}
	time.Sleep(200 * time.Millisecond)
	runtime.GC()
	base := runtime.NumGoroutine()

	for i := 0; i < 1000; i++ {
		relayRun(t, g, plan)
	}
	time.Sleep(700 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()
	if after > base+25 {
		t.Fatalf("goroutine 没回落：基線 %d，1000 次后 %d", base, after)
	}
}

// 429/204/408/416 这些都不是下载成功，必须交给官方回退。
func TestTrustedStatusMustBeOK(t *testing.T) {
	for _, code := range []int{429, 204, 408, 416, 502, 301} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		g, plan := relayGatewayMulti(t, []string{srv.URL})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "http://gw/x", nil)
		if g.tryTrusted(rec, req, plan) {
			t.Fatalf("HTTP %d 不该被当成中转成功", code)
		}
		srv.Close()
	}
}

func relayTruncServer(t *testing.T, data []byte, first int, resume func(w http.ResponseWriter, r *http.Request, from int)) *httptest.Server {
	t.Helper()
	var hits int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 || r.Header.Get("Range") == "" {
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.WriteHeader(http.StatusOK)
			w.Write(data[:first])
			w.(http.Flusher).Flush()
			return
		}
		resume(w, r, rangeStart(r.Header.Get("Range")))
	}))
}

// 续传返回的 ETag 变了：两个版本，绝不能拼。
func TestRelayRejectsChangedETag(t *testing.T) {
	data := relayData(1 << 20)
	first := 256 << 10
	srv := relayTruncServer(t, data, first, func(w http.ResponseWriter, r *http.Request, from int) {
		w.Header().Set("ETag", `"v2"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(data)-1, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[from:])
	})
	defer srv.Close()
	g, plan := relayGatewayMulti(t, []string{srv.URL})
	rec := relayRun(t, g, plan)
	if rec.Body.Len() != first {
		t.Fatalf("ETag 变了应拒绝拼接，只留首段 %d 字节，实际 %d", first, rec.Body.Len())
	}
}

// Content-Range 起点对不上：说明中转给的不是我们要的区间。
func TestRelayRejectsBadContentRange(t *testing.T) {
	data := relayData(1 << 20)
	first := 256 << 10
	srv := relayTruncServer(t, data, first, func(w http.ResponseWriter, r *http.Request, from int) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from+1, len(data)-1, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[from:])
	})
	defer srv.Close()
	g, plan := relayGatewayMulti(t, []string{srv.URL})
	rec := relayRun(t, g, plan)
	if rec.Body.Len() != first {
		t.Fatalf("Content-Range 不合法应拒绝，实际输出 %d", rec.Body.Len())
	}
}

// 总长对不上（中转给了别的文件长度）：同样拒绝。
func TestRelayRejectsWrongTotal(t *testing.T) {
	data := relayData(1 << 20)
	first := 256 << 10
	srv := relayTruncServer(t, data, first, func(w http.ResponseWriter, r *http.Request, from int) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(data)-1, len(data)+1))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[from:])
	})
	defer srv.Close()
	g, plan := relayGatewayMulti(t, []string{srv.URL})
	rec := relayRun(t, g, plan)
	if rec.Body.Len() != first {
		t.Fatalf("总长不符应拒绝，实际输出 %d", rec.Body.Len())
	}
}

// 没有强 ETag（这里连 ETag 都没有）：不允许续传。
func TestRelayNoStrongETagNoResume(t *testing.T) {
	data := relayData(1 << 20)
	first := 256 << 10
	var resumed int32
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) > 1 {
			atomic.StoreInt32(&resumed, 1)
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		w.Write(data[:first])
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()
	g, plan := relayGatewayMulti(t, []string{srv.URL})
	rec := relayRun(t, g, plan)
	if atomic.LoadInt32(&resumed) != 0 {
		t.Fatal("没有强 ETag 时不该发续传请求")
	}
	if rec.Body.Len() != first {
		t.Fatalf("应只输出首段，实际 %d", rec.Body.Len())
	}
}

// 弱 ETag 也不能证明字节一致。
func TestRelayWeakETagNoResume(t *testing.T) {
	data := relayData(1 << 20)
	first := 256 << 10
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) > 1 {
			t.Error("弱 ETag 不该触发续传")
		}
		w.Header().Set("ETag", `W/"v1"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		w.Write(data[:first])
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()
	g, plan := relayGatewayMulti(t, []string{srv.URL})
	relayRun(t, g, plan)
}

// 客户端在续传途中断开：上游请求也要停，不能继续占着连接下载。
func TestRelayClientCancelStopsUpstream(t *testing.T) {
	data := relayData(8 << 20)
	first := 256 << 10
	var upstreamCanceled int32
	var resumed int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "" {
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.WriteHeader(http.StatusOK)
			w.Write(data[:first])
			w.(http.Flusher).Flush()
			return
		}
		atomic.StoreInt32(&resumed, 1)
		from := rangeStart(r.Header.Get("Range"))
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(data)-1, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		for i := from; i < len(data); i += 256 << 10 {
			select {
			case <-r.Context().Done():
				atomic.StoreInt32(&upstreamCanceled, 1)
				return
			case <-time.After(20 * time.Millisecond):
			}
			end := i + 256<<10
			if end > len(data) {
				end = len(data)
			}
			if _, err := w.Write(data[i:end]); err != nil {
				atomic.StoreInt32(&upstreamCanceled, 1)
				return
			}
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	g, plan := relayGatewayMulti(t, []string{srv.URL})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("GET", "http://gw/x", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan bool, 1)
	go func() { done <- g.tryTrusted(rec, req, plan) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("客户端取消后请求没退出")
	}
	if atomic.LoadInt32(&resumed) == 0 {
		t.Skip("没走到续传，跳过")
	}
	time.Sleep(300 * time.Millisecond)
	if atomic.LoadInt32(&upstreamCanceled) == 0 {
		t.Fatal("客户端断开后上游仍在下载")
	}
}

// 首段有强 ETag，续传响应却不带：服务器没有证明是同一实体，拒绝拼接。
func TestRelayResumeMissingETagRejected(t *testing.T) {
	data := relayData(1 << 20)
	first := 256 << 10
	srv := relayTruncServer(t, data, first, func(w http.ResponseWriter, r *http.Request, from int) {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(data)-1, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[from:])
	})
	defer srv.Close()
	g, plan := relayGatewayMulti(t, []string{srv.URL})
	rec := relayRun(t, g, plan)
	if rec.Body.Len() != first {
		t.Fatalf("续传缺 ETag 应拒绝拼接，只留首段 %d 字节，实际 %d", first, rec.Body.Len())
	}
}

// 206 的 Content-Length 与 Content-Range 区间不一致：响应本身是坏的。
func TestRelayResumeContentLengthMismatch(t *testing.T) {
	data := relayData(1 << 20)
	first := 256 << 10
	srv := relayTruncServer(t, data, first, func(w http.ResponseWriter, r *http.Request, from int) {
		want := len(data) - from
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(data)-1, len(data)))
		w.Header().Set("Content-Length", strconv.Itoa(want+4096))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[from:])
	})
	defer srv.Close()
	g, plan := relayGatewayMulti(t, []string{srv.URL})
	rec := relayRun(t, g, plan)
	if rec.Body.Len() != first {
		t.Fatalf("Content-Length 与区间不符应拒绝，只留首段 %d 字节，实际 %d", first, rec.Body.Len())
	}
}
