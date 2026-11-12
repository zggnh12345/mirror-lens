package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
)

// 首字节与超时同时就绪时必须唯一胜出，且优先采用已到达的数据。
func TestWaitWithinTiePrefersValue(t *testing.T) {
	for i := 0; i < 300; i++ {
		ch := make(chan int, 1)
		ch <- 42
		var fired atomic.Int32
		v, ok := waitWithin(ch, time.Nanosecond, func() { fired.Add(1) })
		if !ok || v != 42 {
			t.Fatalf("第 %d 轮：数据已就绪却判超时 ok=%v v=%d", i, ok, v)
		}
		if n := fired.Load(); n != 0 {
			t.Fatalf("第 %d 轮：数据胜出却调用超时回调 %d 次", i, n)
		}
	}
}

func TestWaitWithinTimeoutCallsOnce(t *testing.T) {
	ch := make(chan int, 1)
	var fired atomic.Int32
	start := time.Now()
	if _, ok := waitWithin(ch, 20*time.Millisecond, func() { fired.Add(1) }); ok {
		t.Fatal("空通道应超时")
	}
	if time.Since(start) > time.Second {
		t.Fatal("超时后应立即返回")
	}
	if n := fired.Load(); n != 1 {
		t.Fatalf("超时回调应恰好一次: %d", n)
	}
	// 超时之后才到达的数据不得反向改写结果（值被丢弃、不 panic）。
	ch <- 7
}

// 成功接管后 body 必须能继续读：超时信号不能再取消 ctx。
func TestTrySwitchKeepsBodyAfterFirstByte(t *testing.T) {
	const total = int64(1 << 20)
	const sent = int64(256 << 10)
	payload := make([]byte, total-sent)
	for i := range payload {
		payload[i] = byte(i)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", sent, total-1, total))
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(payload[:1])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// 首字节已发出，远超预算；若成功路径误取消 ctx，后面的读取会失败。
		time.Sleep(80 * time.Millisecond)
		w.Write(payload[1:])
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	g := &Gateway{
		cli:          srv.Client(),
		par:          ParOptions{SwitchMinMBps: 1},
		switchBudget: 40 * time.Millisecond,
		speedProbe:   func(int64, string, int) []int64 { return []int64{128 << 10, 120 << 10, 140 << 10} },
		parTargets: func(*adapter.Plan) []fetchTarget {
			return []fetchTarget{{u: u, c: &scheduler.Candidate{ID: 2, Name: "b", Preferred: true}}}
		},
	}
	plan := &adapter.Plan{Kind: "docker", Upstream: u}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, _, first, _, err := g.trySwitch(ctx, plan, nil, hdr("ETag", `"v1"`), sent, total, map[int64]bool{})
	if err != nil {
		t.Fatalf("首字节在预算内，应换源成功: %v", err)
	}
	defer resp.Body.Close()
	if len(first) != 1 || first[0] != payload[0] {
		t.Fatalf("首字节不符: %v", first)
	}
	rest, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		t.Fatalf("成功接管后 body 被中断: %v", rerr)
	}
	if len(rest) != len(payload)-1 {
		t.Fatalf("剩余长度不符: got=%d want=%d", len(rest), len(payload)-1)
	}
	if rest[0] != payload[1] || rest[len(rest)-1] != payload[len(payload)-1] {
		t.Fatal("剩余内容不符")
	}
}

// 首字节一直不来时必须超时放弃，且不把半开的响应交给调用方。
func TestTrySwitchTimeoutBeforeFirstByte(t *testing.T) {
	const total = int64(1 << 20)
	const sent = int64(256 << 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", sent, total-1, total))
		w.WriteHeader(http.StatusPartialContent)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	g := &Gateway{
		cli:          srv.Client(),
		par:          ParOptions{SwitchMinMBps: 1},
		switchBudget: 30 * time.Millisecond,
		speedProbe:   func(int64, string, int) []int64 { return []int64{128 << 10, 120 << 10, 140 << 10} },
		parTargets: func(*adapter.Plan) []fetchTarget {
			return []fetchTarget{{u: u, c: &scheduler.Candidate{ID: 2, Name: "b", Preferred: true}}}
		},
	}
	plan := &adapter.Plan{Kind: "docker", Upstream: u}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, _, _, _, err := g.trySwitch(ctx, plan, nil, hdr("ETag", `"v1"`), sent, total, map[int64]bool{})
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "没有可换") {
		t.Fatalf("首字节超时应放弃换源: resp=%v err=%v", resp, err)
	}
}
