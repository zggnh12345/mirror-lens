package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
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

// afterTimeout 是超时分支的最终裁决原语：此时数据已就绪就必须胜出。
// 它不经过 select 的双就绪随机路径，因此可以确定性断言。
func TestAfterTimeoutReadyDataWins(t *testing.T) {
	ch := make(chan int, 1)
	ch <- 42
	if v, ok := afterTimeout(ch); !ok || v != 42 {
		t.Fatalf("数据已就绪却判超时: ok=%v v=%d", ok, v)
	}
}

func TestAfterTimeoutEmptyMeansTimeout(t *testing.T) {
	ch := make(chan int, 1)
	if _, ok := afterTimeout(ch); ok {
		t.Fatal("空通道不得判数据胜出")
	}
}

// 自定义 RoundTripper：无视 ctx，延迟返回一个可计数的 Body。
type lateRT struct {
	closed *atomic.Int32
	delay  time.Duration
	body   []byte
}

func (r lateRT) RoundTrip(req *http.Request) (*http.Response, error) {
	time.Sleep(r.delay)
	return &http.Response{
		StatusCode:    http.StatusPartialContent,
		Header:        http.Header{"Etag": {`"v1"`}, "Content-Range": {"bytes 262144-1048575/1048576"}},
		Body:          &countCloser{closed: r.closed, data: r.body},
		ContentLength: int64(len(r.body)),
		Request:       req,
	}, nil
}

type countCloser struct {
	closed *atomic.Int32
	data   []byte
}

func (c *countCloser) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.data)
	c.data = c.data[n:]
	return n, nil
}

func (c *countCloser) Close() error {
	c.closed.Add(1)
	return nil
}

// Do 超时先胜出，迟到响应由清理协程关闭，且只关一次。
func TestTrySwitchLateResponseClosedOnce(t *testing.T) {
	var closed atomic.Int32
	g := &Gateway{
		cli:          &http.Client{Transport: lateRT{closed: &closed, delay: 80 * time.Millisecond, body: []byte("late")}},
		par:          ParOptions{SwitchMinMBps: 1},
		switchBudget: 20 * time.Millisecond,
		speedProbe:   func(int64, string, int) []int64 { return []int64{128 << 10, 120 << 10, 140 << 10} },
		parTargets: func(*adapter.Plan) []fetchTarget {
			u, _ := url.Parse("http://127.0.0.1:1/never")
			return []fetchTarget{{u: u, c: &scheduler.Candidate{ID: 2, Name: "b", Preferred: true}}}
		},
	}
	plan := &adapter.Plan{Kind: "docker", Upstream: fbURL(t, "http://127.0.0.1:1/never")}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, _, _, _, err := g.trySwitch(ctx, plan, nil, hdr("ETag", `"v1"`), 256<<10, 1<<20, map[int64]bool{}); err == nil {
		t.Fatal("Do 超时应放弃换源")
	}
	deadline := time.Now().Add(2 * time.Second)
	for closed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := closed.Load(); n != 1 {
		t.Fatalf("迟到响应 Body 必须恰好关闭一次: %d", n)
	}
}

// 首个 Read 同时返回 n>0 和错误时，错误必须留给下一次 Read。
type errFirstBody struct{ step int }

func (b *errFirstBody) Read(p []byte) (int, error) {
	b.step++
	if b.step == 1 {
		p[0] = 'X'
		return 1, io.ErrUnexpectedEOF
	}
	return 0, io.EOF
}

func (b *errFirstBody) Close() error { return nil }

type staticRT struct{ resp *http.Response }

func (r staticRT) RoundTrip(*http.Request) (*http.Response, error) { return r.resp, nil }

func TestTrySwitchPropagatesFirstReadError(t *testing.T) {
	const total = int64(1 << 20)
	const sent = int64(256 << 10)
	resp := &http.Response{
		StatusCode:    http.StatusPartialContent,
		Header:        http.Header{"Etag": {`"v1"`}, "Content-Range": {"bytes 262144-1048575/1048576"}},
		Body:          &errFirstBody{},
		ContentLength: total - sent,
	}
	g := &Gateway{
		cli:          &http.Client{Transport: staticRT{resp: resp}},
		par:          ParOptions{SwitchMinMBps: 1},
		switchBudget: time.Second,
		speedProbe:   func(int64, string, int) []int64 { return []int64{128 << 10, 120 << 10, 140 << 10} },
		parTargets: func(*adapter.Plan) []fetchTarget {
			return []fetchTarget{{u: fbURL(t, "http://127.0.0.1:1/x"), c: &scheduler.Candidate{ID: 2, Name: "b", Preferred: true}}}
		},
	}
	plan := &adapter.Plan{Kind: "docker"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, _, first, _, err := g.trySwitch(ctx, plan, nil, hdr("ETag", `"v1"`), sent, total, map[int64]bool{})
	if err != nil {
		t.Fatalf("首字节已有数据，应成功接管: %v", err)
	}
	defer got.Body.Close()
	if len(first) != 1 || first[0] != 'X' {
		t.Fatalf("首字节不符: %v", first)
	}
	if _, rerr := got.Body.Read(make([]byte, 8)); rerr != io.ErrUnexpectedEOF {
		t.Fatalf("首个 Read 的错误被吞掉: %v", rerr)
	}
	if _, rerr := got.Body.Read(make([]byte, 8)); rerr != io.EOF {
		t.Fatalf("错误只应交付一次: %v", rerr)
	}
}

func fbURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// 零长度 Read 不得消费待交付的错误。
func TestPendingErrBodyZeroLengthRead(t *testing.T) {
	b := &pendingErrBody{ReadCloser: io.NopCloser(strings.NewReader("rest")), err: io.ErrUnexpectedEOF}
	if n, err := b.Read(nil); n != 0 || err != nil {
		t.Fatalf("零长度读必须返回 (0,nil): %d %v", n, err)
	}
	if _, err := b.Read(make([]byte, 8)); err != io.ErrUnexpectedEOF {
		t.Fatalf("错误被零长度读吞掉: %v", err)
	}
}

// enterRT / enterBody 给测试一个显式屏障：第二次及以后的 Read 会先关闭
// entered，再委托给真实 http body 阻塞等待。这样不必用 sleep 猜“是否已阻塞”。
type enterRT struct {
	base    http.RoundTripper
	entered chan struct{}
	once    *sync.Once
}

func (r enterRT) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.base.RoundTrip(req)
	if resp != nil && resp.Body != nil {
		resp.Body = &enterBody{ReadCloser: resp.Body, entered: r.entered, once: r.once}
	}
	return resp, err
}

type enterBody struct {
	io.ReadCloser
	entered chan struct{}
	once    *sync.Once
	reads   int
}

func (b *enterBody) Read(p []byte) (int, error) {
	b.reads++
	if b.reads > 1 {
		b.once.Do(func() { close(b.entered) })
	}
	return b.ReadCloser.Read(p)
}

// 换源成功持有 Lease 后，Lease 被 Reclaim 必须能取消阻塞中的 Read，
// 并在 Body 关闭后真正释放名额。
func TestLeaseReclaimCancelsBlockedSwitchRead(t *testing.T) {
	const total = int64(1 << 20)
	const sent = int64(256 << 10)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", sent, total-1, total))
		w.WriteHeader(http.StatusPartialContent)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		w.Write([]byte("X"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id := addSchedSource(t, st, "blocked", srv.URL, 10, 9000)

	entered := make(chan struct{})
	var once sync.Once
	cli := srv.Client()
	cli.Transport = enterRT{base: cli.Transport, entered: entered, once: &once}
	g := &Gateway{
		cli:          cli,
		par:          ParOptions{SwitchMinMBps: 1},
		switchBudget: time.Second,
		speedProbe:   func(int64, string, int) []int64 { return []int64{128 << 10, 120 << 10, 140 << 10} },
		parTargets: func(*adapter.Plan) []fetchTarget {
			return []fetchTarget{{u: fbURL(t, srv.URL), c: &scheduler.Candidate{ID: id, Name: "blocked", Preferred: true}}}
		},
	}
	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true, MaxCandidates: 3, MaxPerSource: 4}))
	if held := g.sched.LeaseHeld(id); held != 0 {
		t.Fatalf("起始不应持有名额: %d", held)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, _, first, _, err := g.trySwitch(ctx, &adapter.Plan{Kind: "docker"}, nil, hdr("ETag", `"v1"`), sent, total, map[int64]bool{})
	if err != nil {
		t.Fatalf("应成功接管: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("首字节不符: %v", first)
	}
	if held := g.sched.LeaseHeld(id); held != 1 {
		t.Fatalf("成功接管后应持有 1 个名额: %d", held)
	}

	readErr := make(chan error, 1)
	go func() {
		_, rerr := resp.Body.Read(make([]byte, 4096))
		readErr <- rerr
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("调用方的 Read 未进入底层")
	}
	select {
	case rerr := <-readErr:
		t.Fatalf("上游仍在挂起，Read 不应提前返回: %v", rerr)
	default:
	}
	if n := g.sched.ReclaimSource(id, "stall"); n != 1 {
		t.Fatalf("Reclaim 应命中 1 个 Lease: %d", n)
	}
	select {
	case rerr := <-readErr:
		if rerr == nil {
			t.Fatal("Reclaim 后阻塞的 Read 必须返回错误")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Reclaim 未能中断阻塞的 Read：取消函数没有绑定到 Lease")
	}
	resp.Body.Close()
	deadline := time.Now().Add(2 * time.Second)
	for g.sched.LeaseHeld(id) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if held := g.sched.LeaseHeld(id); held != 0 {
		t.Fatalf("Body 关闭后名额未释放: %d", held)
	}
}

// ctxBlockingBody 的第一段数据交付后，Read 会停在 ctx.Done 上；它只跟随
// 请求 Context，不依赖任何超时或调度时序。Reclaim 若没有经 BindCancel
// 取消这个 Context，Read 就永远不会返回。
type ctxBlockingBody struct {
	ctx     context.Context
	entered chan struct{}
	once    sync.Once
	first   []byte
}

func (b *ctxBlockingBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(b.first) > 0 {
		n := copy(p, b.first)
		b.first = b.first[n:]
		return n, nil
	}
	b.once.Do(func() { close(b.entered) })
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *ctxBlockingBody) Close() error { return nil }

type ctxBlockingRT struct {
	entered chan struct{}
	first   []byte
	sent    int64
	total   int64
}

func (r ctxBlockingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusPartialContent,
		Header: http.Header{
			"Etag":           {`"v1"`},
			"Content-Range":  {fmt.Sprintf("bytes %d-%d/%d", r.sent, r.total-1, r.total)},
			"Content-Length": {strconv.FormatInt(r.total-r.sent, 10)},
		},
		ContentLength: r.total - r.sent,
		Body:          &ctxBlockingBody{ctx: req.Context(), entered: r.entered, first: append([]byte(nil), r.first...)},
		Request:       req,
	}, nil
}

// 测试替身自身也要遵守 io.Reader：零长度读不得进入阻塞分支。
func TestCtxBlockingBodyZeroLengthRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	b := &ctxBlockingBody{ctx: ctx, entered: entered}
	if n, err := b.Read(nil); n != 0 || err != nil {
		t.Fatalf("零长度读应返回 (0,nil): %d %v", n, err)
	}
	select {
	case <-entered:
		t.Fatal("零长度读不应关闭 entered / 进入阻塞")
	default:
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// 确定性版本：阻塞点由测试 Body 显式控制，entered 关闭后 Read 只可能因
// Context 取消返回，因此 Reclaim 与阻塞状态的先后不依赖 sleep。
func TestLeaseReclaimCancelsCtxBlockingBody(t *testing.T) {
	const total = int64(1 << 20)
	const sent = int64(256 << 10)

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id := addSchedSource(t, st, "ctxblocked", "http://ctxblocked.invalid/x", 10, 9000)

	entered := make(chan struct{})
	g := &Gateway{
		cli:          &http.Client{Transport: ctxBlockingRT{entered: entered, first: []byte("X"), sent: sent, total: total}},
		par:          ParOptions{SwitchMinMBps: 1},
		switchBudget: time.Second,
		speedProbe:   func(int64, string, int) []int64 { return []int64{128 << 10, 120 << 10, 140 << 10} },
		parTargets: func(*adapter.Plan) []fetchTarget {
			return []fetchTarget{{u: &url.URL{Scheme: "http", Host: "ctxblocked.invalid", Path: "/x"}, c: &scheduler.Candidate{ID: id, Name: "ctxblocked", Preferred: true}}}
		},
	}
	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true, MaxCandidates: 3, MaxPerSource: 4}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, _, first, _, err := g.trySwitch(ctx, &adapter.Plan{Kind: "docker"}, nil, hdr("ETag", `"v1"`), sent, total, map[int64]bool{})
	if err != nil {
		t.Fatalf("应成功接管: %v", err)
	}
	if len(first) != 1 || g.sched.LeaseHeld(id) != 1 {
		t.Fatalf("首字节或名额不符: %v held=%d", first, g.sched.LeaseHeld(id))
	}

	readErr := make(chan error, 1)
	go func() {
		_, rerr := resp.Body.Read(make([]byte, 64))
		readErr <- rerr
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Read 未走到阻塞点")
	}
	select {
	case rerr := <-readErr:
		t.Fatalf("Context 未取消，Read 不应返回: %v", rerr)
	default:
	}
	if n := g.sched.ReclaimSource(id, "stall"); n != 1 {
		t.Fatalf("Reclaim 应命中 1 个 Lease: %d", n)
	}
	select {
	case rerr := <-readErr:
		if !errors.Is(rerr, context.Canceled) {
			t.Fatalf("Reclaim 后 Read 应因 Context 取消返回 context.Canceled，实际: %v", rerr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Reclaim 没有取消请求 Context：BindCancel 未生效")
	}
	resp.Body.Close()
	if held := g.sched.LeaseHeld(id); held != 0 {
		t.Fatalf("关闭后名额未释放: %d", held)
	}
}
