package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
)

// 3.5.22 第三阶段单元测试：动态门槛 T_i=max(F,P_i/1.3,C×1.2)、进度闸门、
// HTTP 200 回放预算。内部统一 B/s；历史样本 b/ms 在进入门槛前 ×1000。

func TestDynamicThresholdBps(t *testing.T) {
	const mib = int64(1 << 20)
	g := &Gateway{par: ParOptions{SwitchFloorMiBPerSec: 1}} // F = 1048576 B/s
	cases := []struct {
		name    string
		p, c    int64
		wantBps int64
		explain string
	}{
		{"无同伴无当前速度退化为 F", 0, 0, mib, "max(1048576, 0, 0)"},
		{"F 主导", 100, 200, mib, "max(1048576, 76, 240)"},
		{"C×1.2 主导：当前 2.5MiB/s", 0, 2500 * 1024, 3000 * 1024, "max(1048576, 0, 3072000)"},
		{"P/1.3 主导：同伴 2.6MiB/s", 2600 * 1024, 100 * 1024, 2000 * 1024, "max(1048576, 2048000, 122880)"},
		{"当前源更快时拒绝弱候选", 0, 2500 * 1024, 3000 * 1024, "1700KiB/s < C×1.2=3072000B/s"},
	}
	for _, c := range cases {
		got := g.dynamicThresholdBps(c.p, c.c)
		if got != c.wantBps {
			t.Fatalf("%s: got %d want %d (%s)", c.name, got, c.wantBps, c.explain)
		}
	}
}

// 候选样本 1000 b/ms（1.0MB/s 十进制）、同伴 1700 b/ms（1.7MB/s）
// → P_i=1_700_000 B/s，P_i/1.3≈1_307_692 B/s，候选 1_000_000 B/s 应被拒绝。
func TestPeerMedianRejectsCandidateByP(t *testing.T) {
	g := &Gateway{
		par: ParOptions{SwitchFloorMiBPerSec: 1},
		parTargets: func(*adapter.Plan) []fetchTarget {
			return []fetchTarget{
				{u: nil, c: &scheduler.Candidate{ID: 1, Name: "cur", Preferred: true}},
				{u: nil, c: &scheduler.Candidate{ID: 2, Name: "cand", Preferred: true}},
				{u: nil, c: &scheduler.Candidate{ID: 3, Name: "peer", Preferred: true}},
			}
		},
		speedProbe: func(id int64, _ string, _ int) []int64 {
			switch id {
			case 2:
				return []int64{1000, 1000, 1000} // b/ms = 1.0MB/s 十进制
			case 3:
				return []int64{1700, 1700, 1700} // b/ms = 1.7MB/s 十进制
			}
			return nil
		},
	}
	plan := &adapter.Plan{Kind: "docker"}
	cur := &fetchTarget{c: &scheduler.Candidate{ID: 1}}
	p := g.peerMedianBps(plan, cur, 2, nil)
	if p != 1700*1000 {
		t.Fatalf("P_i 应为 %d B/s，实际 %d", 1700*1000, p)
	}
	th := g.dynamicThresholdBps(p, 0) // max(1048576, 1700000/1.3≈1307692, 0)
	if th != 1700*1000*10/13 {
		t.Fatalf("T_i 应 ≈%d B/s，实际 %d", 1700*1000*10/13, th)
	}
	if 1000*1000 >= th {
		t.Fatalf("候选 1000000 B/s 应被 T_i=%d 拒绝", th)
	}
}

// 失效候选被排除后必须立即重算 P_i：A 历史 12.9MB/s 但连接失败，
// B 只有 3.9MB/s；若仍把 A 当同伴，B 会被 P_i/1.3≈9.9MB/s 拒绝。
func TestPeerExclusionRecomputesAfterFailedCandidate(t *testing.T) {
	const total = int64(1 << 20)
	const sent = int64(256 << 10)
	body := bytes.Repeat([]byte("z"), int(total))

	var bHits int
	bSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHits++
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", sent, total-1, total))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body[sent:])
	}))
	defer bSrv.Close()

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL + "/blob"
	dead.Close()
	aURL, _ := url.Parse(deadURL)
	bURL, _ := url.Parse(bSrv.URL + "/blob")

	g := &Gateway{
		cli: &http.Client{Timeout: 3 * time.Second},
		par: ParOptions{SwitchFloorMiBPerSec: 1},
		parTargets: func(*adapter.Plan) []fetchTarget {
			return []fetchTarget{
				{u: aURL, c: &scheduler.Candidate{ID: 1, Name: "A", Preferred: true}},
				{u: bURL, c: &scheduler.Candidate{ID: 2, Name: "B", Preferred: true}},
			}
		},
		speedProbe: func(id int64, _ string, _ int) []int64 {
			switch id {
			case 1:
				return []int64{12914, 12914, 12914} // 12.9MB/s，但连接失败
			case 2:
				return []int64{3867, 3867, 3867} // 3.9MB/s，唯一可用
			}
			return nil
		},
	}
	plan := &adapter.Plan{Kind: "docker"}
	cur := &fetchTarget{c: &scheduler.Candidate{ID: 9, Name: "C"}}
	curHdr := http.Header{"Etag": []string{`"v1"`}}

	resp, tgt, first, verified, err := g.trySwitchWithSpeed(context.Background(), plan, cur, curHdr, sent, total, 100*1024, map[int64]bool{})
	if err != nil {
		t.Fatalf("A 失败后应继续尝试 B: %v", err)
	}
	if resp == nil || tgt == nil || tgt.c.ID != 2 {
		t.Fatalf("应由 B 接管，实际 tgt=%v", tgt)
	}
	defer resp.Body.Close()
	if !verified || len(first) == 0 {
		t.Fatalf("B 应通过校验并有首块: verified=%v first=%d", verified, len(first))
	}
	if bHits != 1 {
		t.Fatalf("B 应只命中 1 次，实际 %d", bHits)
	}
}

// 进度闸门：0.8×total 与绝对上限取 min；用整数运算避免 total×8 溢出。
func TestSwitchProgressGate(t *testing.T) {
	total := int64(200 << 20) // 200MiB
	w := newStreamWatch(ParOptions{
		WatchMBps: 3, WatchWindow: time.Second, SwitchMinFileMiB: 1,
		SwitchMaxSentMiB: 150, SwitchMax: 2,
	})
	// 只测闸门：用足够慢的速率让判慢成立，sent 处在不同位置。
	slow := func(sent int64) bool {
		w.giveUp, w.switches, w.lastSwitch = false, 0, time.Time{}
		w.samples = nil
		for i := 0; i < 2; i++ {
			w.add(1 << 20)
			time.Sleep(1100 * time.Millisecond)
		}
		return w.slow(sent, total)
	}
	if !slow(100 << 20) { // 50% < 80%：允许
		t.Fatal("sent=100MiB(50%) 应允许换源")
	}
	if slow(170 << 20) { // 85% > 80%：拒绝
		t.Fatal("sent=170MiB(85%) 应拒绝换源")
	}
	if slow(155 << 20) { // 77.5% 但 > 150MiB 绝对上限：拒绝
		t.Fatal("sent=155MiB 超过 150MiB 绝对上限应拒绝")
	}
}

// HTTP 200 回放预算：按候选速度估算，超预算拒绝；预算为 0 走默认 5s。
func TestReplayBudget(t *testing.T) {
	g := &Gateway{switchReplayBudget: time.Second}
	if !g.replayCostOK(512<<10, 1<<20) { // 512KiB @1MiB/s = 0.5s，放行
		t.Fatal("0.5s 回放应放行")
	}
	if g.replayCostOK(2<<20, 1<<20) { // 2MiB @1MiB/s = 2s > 1s，拒绝
		t.Fatal("2s 回放应超预算拒绝")
	}
	if g.replayCostOK(1<<20, 0) { // 速度未知：保守拒绝
		t.Fatal("速度未知应拒绝")
	}
}

// 回放过程可被 ctx 取消：客户端断开后 discardN 立刻返回，不继续吞字节。
func TestDiscardNCancellable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := strings.NewReader(strings.Repeat("x", 1<<20))
	cancel() // 已取消：第一次循环即返回
	if err := discardN(ctx, r, 1<<20); err == nil {
		t.Fatal("已取消的 ctx 应让 discardN 返回错误")
	}
	// 正常路径：丢满 n 字节不报错
	if err := discardN(context.Background(), io.LimitReader(strings.NewReader(strings.Repeat("y", 4096)), 4096), 4096); err != nil {
		t.Fatalf("正常回放应成功: %v", err)
	}
}

type blockingReadCloser struct {
	closed chan struct{}
	once   sync.Once
}

func (b *blockingReadCloser) Read(p []byte) (int, error) {
	<-b.closed
	return 0, errors.New("body closed")
}

func (b *blockingReadCloser) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

// HTTP 200 回放超时必须 Close 底层 Body，否则阻塞中的 Read 永远不会退出。
func TestDiscardWithDeadlineClosesBlockingBody(t *testing.T) {
	b := &blockingReadCloser{closed: make(chan struct{})}
	start := time.Now()
	err := discardWithDeadline(context.Background(), b, 1<<20, 50*time.Millisecond)
	if err == nil {
		t.Fatal("回放超时应返回错误")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("超时后仍阻塞了 %v", d)
	}
	select {
	case <-b.closed:
	default:
		t.Fatal("超时后底层 Body 必须被 Close")
	}
}

func TestSwitchBodyWithContentLength(t *testing.T) {
	b := newSwitchBody(io.NopCloser(strings.NewReader("abcd")), 4, false, time.Second)
	got, err := io.ReadAll(b)
	if err != nil || string(got) != "abcd" {
		t.Fatalf("完整 206: got %q err=%v", got, err)
	}
	b = newSwitchBody(io.NopCloser(strings.NewReader("ab")), 4, false, time.Second)
	if _, err := io.ReadAll(b); !errors.Is(err, errSwitchIncomplete) {
		t.Fatalf("提前 EOF 应判不完整，实际 %v", err)
	}
}

func TestSwitchBodyNoContentLength(t *testing.T) {
	b := newSwitchBody(io.NopCloser(strings.NewReader("abcd")), 4, true, time.Second)
	got, err := io.ReadAll(b)
	if err != nil || string(got) != "abcd" {
		t.Fatalf("chunked 正常 EOF: got %q err=%v", got, err)
	}
	b = newSwitchBody(io.NopCloser(strings.NewReader("abcde")), 4, true, time.Second)
	if _, err := io.ReadAll(b); !errors.Is(err, errOverrun) {
		t.Fatalf("chunked 超长应拒绝，实际 %v", err)
	}
	b = newSwitchBody(&blockingReadCloser{closed: make(chan struct{})}, 0, true, 50*time.Millisecond)
	if _, err := b.Read(make([]byte, 1)); !errors.Is(err, errSwitchIncomplete) {
		t.Fatalf("chunked 读满不结束应读超时失败，实际 %v", err)
	}
}

func TestSwitchAcceptableAsymmetricVerifier(t *testing.T) {
	cur := http.Header{} // 200 源没有强 ETag
	newH := http.Header{}
	newH.Set("ETag", `"v1"`)
	newH.Set("Content-Range", "bytes 100-999/1000")
	full := strings.Repeat("a", 64)
	verified, ok := switchAcceptable(cur, newH, http.StatusPartialContent, 900, 100, 1000, full)
	if verified || !ok {
		t.Fatalf("一侧有 ETag、有完整 SHA-256 应允许但标记未验证，got verified=%v ok=%v", verified, ok)
	}
	if _, ok := switchAcceptable(cur, newH, http.StatusPartialContent, 900, 100, 1000, "abc"); ok {
		t.Fatal("一侧有 ETag、只有短摘要必须拒绝")
	}
	if _, ok := switchAcceptable(cur, newH, http.StatusPartialContent, 900, 100, 1000, ""); ok {
		t.Fatal("一侧有 ETag、无摘要必须拒绝")
	}
	cur.Set("ETag", `"v1"`)
	newH.Set("ETag", `"v2"`)
	if _, ok := switchAcceptable(cur, newH, http.StatusPartialContent, 900, 100, 1000, full); ok {
		t.Fatal("双方 ETag 不一致必须拒绝，即使有完整 SHA-256")
	}
}
