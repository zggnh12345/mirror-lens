package gateway

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

func relayData(n int) []byte {
	d := make([]byte, n)
	for i := range d {
		d[i] = byte(i*31 + i>>5)
	}
	return d
}

func relayGateway(t *testing.T, relayURL string) (*Gateway, *adapter.Plan) {
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
	g.SetTrusted([]TrustedRelay{{ID: 1, Name: "自建站", URL: relayURL}})
	u, _ := url.Parse("https://github.com/o/r/releases/download/v1/big.bin")
	return g, &adapter.Plan{
		Kind:         "release",
		Upstream:     u,
		Stream:       true,
		OfficialOnly: true,
		NoStore:      true,
		Header:       http.Header{"Authorization": {"token test"}},
	}
}

func relayRun(t *testing.T, g *Gateway, plan *adapter.Plan) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://gw/https://github.com/o/r/releases/download/v1/big.bin", nil)
	if !g.tryTrusted(rec, req, plan) {
		t.Fatal("应走信任中转")
	}
	return rec
}

func rangeStart(h string) int {
	var from, to int64
	fmt.Sscanf(h, "bytes=%d-%d", &from, &to)
	return int(from)
}

// 中转首次只给一半就断：必须用 Range 续传，客户端最终拿到完整文件。
func TestRelayResumesAfterTruncation(t *testing.T) {
	data := relayData(1 << 20)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			from := rangeStart(r.Header.Get("Range"))
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(data)-1, len(data)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(data[from:])
			return
		}
		n := atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		if n == 1 {
			// 只写一半就返回：客户端会看到 unexpected EOF
			w.Write(data[:len(data)/2])
			w.(http.Flusher).Flush()
			return
		}
		w.Write(data)
	}))
	defer srv.Close()

	g, plan := relayGateway(t, srv.URL)
	rec := relayRun(t, g, plan)
	if rec.Body.Len() != len(data) {
		t.Fatalf("应输出 %d 字节，实际 %d", len(data), rec.Body.Len())
	}
	if !bytes.Equal(rec.Body.Bytes(), data) {
		t.Fatal("续传后内容错位")
	}
	// 中转转发要进流量账，否则面板上看不到这笔。
	rows, err := g.st.TrafficByDay(1)
	if err != nil {
		t.Fatal(err)
	}
	var bypass int64
	for _, row := range rows {
		if row.Traffic == store.TrafficBypass {
			bypass += row.Bytes
		}
	}
	if bypass != int64(len(data)) {
		t.Fatalf("流量账应记 %d 字节，实际 %d", len(data), bypass)
	}
	// 面板的「正在加速 / 刚刚完成」要能看到这条私有请求。
	snap := g.ActiveTasks()
	if len(snap.Recent) == 0 {
		t.Fatal("私有请求应登记成任务，否则面板看不到")
	}
	r := snap.Recent[0]
	if r.Mode != "relay" || !r.OK || r.Total != int64(len(data)) {
		t.Fatalf("任务记录不对: mode=%s ok=%v total=%d", r.Mode, r.OK, r.Total)
	}
	if len(r.Sources) != 1 || r.Sources[0] != "自建站" {
		t.Fatalf("应记录中转名，实际 %v", r.Sources)
	}
}

// 中转不认 Range（续传请求仍回 200 全量）：跳过已写部分后继续，不能重复写。
func TestRelayResumeWithoutRangeSupport(t *testing.T) {
	data := relayData(512 << 10)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		if n == 1 {
			w.Write(data[:len(data)/3])
			w.(http.Flusher).Flush()
			return
		}
		w.Write(data) // 忽略 Range，回全量
	}))
	defer srv.Close()

	g, plan := relayGateway(t, srv.URL)
	rec := relayRun(t, g, plan)
	if !bytes.Equal(rec.Body.Bytes(), data) {
		t.Fatalf("应输出完整文件，实际 %d 字节（Content-Length=%d）", rec.Body.Len(), len(data))
	}
}

// 续传过程又断两次：只要每次有推进就该一直补下去。
func TestRelayResumeSurvivesRepeatedBreaks(t *testing.T) {
	data := relayData(1 << 20)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		from := 0
		if r.Header.Get("Range") != "" {
			from = rangeStart(r.Header.Get("Range"))
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(data)-1, len(data)))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.WriteHeader(http.StatusOK)
		}
		n := atomic.AddInt32(&hits, 1)
		chunk := (len(data) - from) / 4
		if chunk < 1 {
			chunk = len(data) - from
		}
		if n <= 2 {
			chunk = chunk / 2 // 前两次各只给一点
		}
		w.Write(data[from : from+chunk])
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	g, plan := relayGateway(t, srv.URL)
	rec := relayRun(t, g, plan)
	if !bytes.Equal(rec.Body.Bytes(), data) {
		t.Fatalf("多次中断后应补齐，实际 %d/%d 字节", rec.Body.Len(), len(data))
	}
}

// 401 这类非 200 响应原样透传，不该触发续传逻辑。
func TestRelayPassesDeniedThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer srv.Close()

	g, plan := relayGateway(t, srv.URL)
	rec := relayRun(t, g, plan)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("应透传 401，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Bad credentials") {
		t.Fatal("响应体应原样透传")
	}
}
