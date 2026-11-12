package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
)

var errOverrun = errors.New("上游返回的字节超过声明长度")

// speedSample 是一段真实时间内的字节数：不能假设每次采样都正好 1 秒。
type speedSample struct {
	b int64
	d time.Duration
}

// streamWatch 在边下边发的同时按真实时间采样，判断这台源是不是已经慢到值得换。
// 它不做预测：只看刚过去的一段真实字节数与真实时长。
type streamWatch struct {
	window    time.Duration
	limitKB   int64
	minFile   int64
	maxSent   int64
	maxSwitch int
	gap       time.Duration

	winStart time.Time
	winBytes int64
	blocked  time.Duration // 窗口内写客户端/落盘的耗时，不算源站吞吐
	samples  []speedSample
	lastAt   time.Time

	switches   int
	lastSwitch time.Time
	giveUp     bool
}

func newStreamWatch(o ParOptions) *streamWatch {
	w := &streamWatch{
		window:    o.WatchWindow,
		limitKB:   o.WatchMBps * 1024,
		minFile:   o.SwitchMinFileMiB << 20,
		maxSent:   o.SwitchMaxSentMiB << 20,
		maxSwitch: o.SwitchMax,
		gap:       o.SwitchGap,
	}
	if w.window <= 0 {
		w.window = 10 * time.Second
	}
	if w.limitKB <= 0 {
		w.limitKB = 3 * 1024
	}
	if w.maxSwitch <= 0 {
		w.maxSwitch = 0
	}
	w.winStart = time.Now()
	w.lastAt = w.winStart
	return w
}

// add 记一段刚收到的上游字节。窗口按真实时间切分。
func (w *streamWatch) add(n int64) {
	if n <= 0 {
		return
	}
	now := time.Now()
	w.winBytes += n
	w.lastAt = now
	w.roll(now)
}

// addWrite 记一次写客户端/落盘的阻塞时间，从窗口时长里扣掉：
// 下游慢不是源站慢，不能据此换源。
func (w *streamWatch) addWrite(d time.Duration) {
	if d > 0 {
		w.blocked += d
	}
}

// tick 由定时器驱动：没有数据到达时也推进窗口，源站卡死才能被判慢。
func (w *streamWatch) tick() { w.roll(time.Now()) }

func (w *streamWatch) roll(now time.Time) {
	el := now.Sub(w.winStart)
	if el < time.Second {
		return
	}
	d := el - w.blocked
	if d < 0 {
		d = 0
	}
	if d > 0 || w.winBytes > 0 {
		w.samples = append(w.samples, speedSample{b: w.winBytes, d: d})
		if max := int(w.window / time.Second); max > 0 && len(w.samples) > max {
			w.samples = w.samples[len(w.samples)-max:]
		}
	}
	w.winBytes = 0
	w.blocked = 0
	w.winStart = now
}

// windowStats 汇总最近一个窗口的字节与真实时长（不够一个窗口就返回已攒部分）。
func (w *streamWatch) windowStats() (int64, time.Duration) {
	var sum int64
	var dur time.Duration
	for i := len(w.samples) - 1; i >= 0; i-- {
		sum += w.samples[i].b
		dur += w.samples[i].d
		if dur >= w.window {
			break
		}
	}
	// 还没落样本的当前一段也算进去，避免刚启动时窗口永远不前进。
	if d := time.Since(w.winStart) - w.blocked; d > 0 && (w.winBytes > 0 || d >= time.Second) {
		sum += w.winBytes
		dur += d
	}
	return sum, dur
}

// slow 判断是否该换源。窗口必须覆盖满真实时长，并且这次请求还有资格换。
func (w *streamWatch) slow(sent, total int64) bool {
	if w.giveUp || w.maxSwitch == 0 || total < w.minFile || sent <= 0 || sent > w.maxSent {
		return false
	}
	if w.switches >= w.maxSwitch {
		return false
	}
	if !w.lastSwitch.IsZero() && time.Since(w.lastSwitch) < w.gap {
		return false
	}
	sum, dur := w.windowStats()
	if dur < w.window {
		return false
	}
	avgKB := rateKB(sum, dur)
	return avgKB < w.limitKB
}

// rateKB 用真实时长（纳秒精度）算平均吞吐，不截断整秒。
func rateKB(b int64, d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64(float64(b) / d.Seconds() / 1024)
}

// sample 返回当前窗口的平均速度（KB/s）与覆盖的整秒数（仅供存储展示）。
// 速度按真实时长计算：3.9 秒就是 3.9 秒，不会被当成 3 秒而高估。
func (w *streamWatch) sample() (int64, int) {
	sum, dur := w.windowStats()
	if dur <= 0 {
		return 0, 0
	}
	if w.window > 0 && dur > w.window {
		dur = w.window
	}
	sec := int((dur + time.Second - 1) / time.Second)
	if sec <= 0 {
		return 0, 0
	}
	return rateKB(sum, dur), sec
}

// setGiveUp 标记本次请求不再尝试换源（没有可换的源，或新源首字节超时）。
func (w *streamWatch) setGiveUp() { w.giveUp = true }

func (w *streamWatch) afterSwitch() {
	w.switches++
	w.lastSwitch = time.Now()
	w.samples = nil
	w.winBytes = 0
	w.blocked = 0
	w.winStart = time.Now()
}

// verifierMatch 判断两份响应头能否证明是同一份字节。
// 只接受强 ETag 精确相等。Last-Modified 只有秒级精度，同秒内资源仍可能变化，
// 因此不再当作字节一致性证据；两边都没有强 ETag 时一律返回不可比，
// 由调用方决定是否允许用整份摘要兜底。
func verifierMatch(oldH, newH http.Header) (bool, bool) {
	oe, ne := strongETag(oldH), strongETag(newH)
	if oe != "" && ne != "" {
		return oe == ne, true
	}
	return false, false
}

// hasStrongVerifier 表示这份响应带了可用于字节级比较的强 ETag。
func hasStrongVerifier(h http.Header) bool {
	return strongETag(h) != ""
}

// switchSpeedOK 判断样本够不够快、能不能作为中途换源目标。
// 少于 3 条不下结论：单条样本在慢启动/瞬时拥塞下偏差太大。
func switchSpeedOK(sp []int64, minKBps int64) bool {
	if len(sp) < 3 {
		return false
	}
	return medianInt64(sp) >= minKBps
}

func medianInt64(v []int64) int64 {
	c := append([]int64(nil), v...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	n := len(c)
	if n%2 == 1 {
		return c[n/2]
	}
	return (c[n/2-1] + c[n/2]) / 2
}

// recentProxySpeeds 查某台源最近的实测吞吐（KB/s），测试可注入。
func (g *Gateway) recentProxySpeeds(id int64, kind string, n int) []int64 {
	if g.speedProbe != nil {
		return g.speedProbe(id, kind, n)
	}
	if g.st == nil {
		return nil
	}
	return g.st.RecentProxySpeeds(id, kind, n)
}

// switchCandidates 给换源挑候选源；测试可注入固定列表。
func (g *Gateway) switchCandidates(plan *adapter.Plan) []fetchTarget {
	if g.parTargets != nil {
		return g.parTargets(plan)
	}
	return g.targets(plan, nil)
}

// switchAcceptable 判断新响应能否接在 sent 之后。
// verified 为假表示没有可比验证器、靠 ExpectSHA 兜底。
func switchAcceptable(curHdr, newHdr http.Header, status int, clen, sent, total int64, expectSHA string) (verified, ok bool) {
	if total < 0 || sent <= 0 {
		return false, false
	}
	switch status {
	case http.StatusPartialContent:
		start, tot, err := parseContentRange(newHdr.Get("Content-Range"))
		if err != nil || start != sent || tot != total {
			return false, false
		}
		if clen >= 0 && clen != total-sent {
			return false, false
		}
	case http.StatusOK:
		// 忽略 Range 时必须先证明长度一致，否则丢前缀就是拿未知版本拼接。
		if clen != total {
			return false, false
		}
	default:
		return false, false
	}
	match, comparable := verifierMatch(curHdr, newHdr)
	if comparable {
		if !match {
			// 验证器不一致：明确拒绝，不做「差不多」的猜测。
			return false, false
		}
		return true, true
	}
	// 没有可比的强 ETag：一侧有、另一侧没有，同样无法证明是同一版本。
	if hasStrongVerifier(curHdr) != hasStrongVerifier(newHdr) {
		return false, false
	}
	// 双方都没有强验证器：只有可信摘要能兜底。
	return false, expectSHA != ""
}

// trySwitch 在旧连接不动的前提下，向下一台有快样本的星标发一次请求。
// 只有 1 秒内读到第一块数据、并且能证明与旧源是同一版本，才算成功。
// verified 为假表示没有可比验证器，只能靠 ExpectSHA 事后兜底。
func (g *Gateway) trySwitch(ctx context.Context, plan *adapter.Plan, cur *fetchTarget, curHdr http.Header, sent, total int64, tried map[int64]bool) (*http.Response, *fetchTarget, []byte, bool, error) {
	if sent <= 0 || total < 0 {
		return nil, nil, nil, false, errors.New("没有可用的换源条件")
	}
	cands := g.switchCandidates(plan)
	for _, t := range cands {
		if t.c == nil || !t.c.Preferred || tried[t.c.ID] || (cur != nil && cur.c != nil && cur.c.ID == t.c.ID) {
			continue
		}
		// 只换到有真实快样本的源：没样本时赌它快，代价是几十秒空档，不划算。
		// 取最近 5 条的中位数：生产样本 CV>1.4，最新一条的峰值会让慢源插队。
		sp := g.recentProxySpeeds(t.c.ID, plan.Kind, 5)
		if !switchSpeedOK(sp, g.par.SwitchMinMBps*1024) {
			continue
		}
		tried[t.c.ID] = true

		// 交接用预留名额：拿不到就放弃本次换源，绝不排队（I6）。
		l, lerr := g.leaseFor(ctx, t, scheduler.KindHandover)
		if lerr != nil {
			continue
		}

		// 首字节预算只约束「等到首字节」：读到数据后绝不能再取消 ctx，
		// 否则 body 立刻断。超时信号只投递，由 waitWithin 唯一裁决胜负。
		budget := g.switchTimeout()
		start := time.Now()
		rctx, cancel := context.WithCancel(ctx)
		req, err := http.NewRequestWithContext(rctx, http.MethodGet, t.u.String(), nil)
		if err != nil {
			cancel()
			releaseLease(l)
			continue
		}
		req.Header.Set("User-Agent", "mirror-lens/"+Version+" (+https://github.com/zggnh12345/mirror-lens)")
		req.Header.Set("Accept-Encoding", "identity")
		for k, vs := range plan.Header {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		req.Header.Set("Range", "bytes="+strconv.FormatInt(sent, 10)+"-")

		type doResult struct {
			resp *http.Response
			err  error
		}
		done := make(chan doResult, 1)
		go func() {
			resp, err := g.cli.Do(req)
			done <- doResult{resp, err}
		}()
		d, ok := waitWithin(done, budget, func() {
			cancel()
			// 超时后才返回的响应无人接收，必须关掉，避免泄漏连接。
			go func() {
				if late := <-done; late.resp != nil {
					late.resp.Body.Close()
				}
			}()
		})
		if !ok {
			releaseLease(l)
			continue
		}
		resp, err := d.resp, d.err
		if err != nil {
			cancel()
			releaseLease(l)
			continue
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			resp.Body.Close()
			cancel()
			releaseLease(l)
			continue
		}
		verified, vok := switchAcceptable(curHdr, resp.Header, resp.StatusCode, resp.ContentLength, sent, total, plan.ExpectSHA)
		if !vok {
			resp.Body.Close()
			cancel()
			releaseLease(l)
			continue
		}

		remaining := budget - time.Since(start)
		if remaining <= 0 {
			resp.Body.Close()
			cancel()
			releaseLease(l)
			continue
		}
		type readResult struct {
			n   int
			err error
		}
		firstCh := make(chan readResult, 1)
		buf := make([]byte, 64<<10)
		go func() {
			n, err := resp.Body.Read(buf)
			firstCh <- readResult{n, err}
		}()
		rr, ok := waitWithin(firstCh, remaining, func() {
			// 只取消，不在这里关闭 body：Close 由下面唯一的失败分支执行，
			// 保证任何路径上 body 只被关闭一次。cancel 会先让阻塞的 Read 返回。
			cancel()
		})
		if !ok || rr.n <= 0 {
			cancel()
			resp.Body.Close()
			releaseLease(l)
			continue
		}
		wrapped, werr := wrapLease(resp, l)
		if werr != nil {
			resp.Body.Close()
			cancel()
			releaseLease(l)
			continue
		}
		resp = wrapped
		// 换源接管走的是读循环直用路径，不会经过 stream.go 里那次
		// *leaseBody 断言；这里补上同一道空闲看门狗，避免调用方既不读
		// 也不关时名额长期占用。
		if lb, ok := resp.Body.(*leaseBody); ok && l != nil {
			go lb.watch(90 * time.Second)
		}
		// 成功接管的 body 关闭时才取消 rctx：既不泄漏 context，也保证
		// 迟到的超时信号永远不能打断已经开始的读取。
		resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
		holderFrom(ctx).set(l)
		log.Printf("单流换源：%s 已接手（已发 %d MB，验证器=%v）", t.c.Name, sent>>20, verified)
		return resp, &t, buf[:rr.n], verified, nil
	}
	return nil, nil, nil, false, fmt.Errorf("没有可换的星标源")
}

// discardN 把新源从 0 开始的那段字节读掉，只留下 sent 之后的内容。
func discardN(r io.Reader, n int64) error {
	if n <= 0 {
		return nil
	}
	_, err := io.CopyN(io.Discard, r, n)
	return err
}

// switchFirstByteBudget 是默认的首字节等待预算。
const switchFirstByteBudget = 3 * time.Second

func (g *Gateway) switchTimeout() time.Duration {
	if g.switchBudget > 0 {
		return g.switchBudget
	}
	return switchFirstByteBudget
}

// waitWithin 等待 ch 里的结果或超时。超时回调只投递信号、不直接产生副作用，
// 是否取消由调用方在 onTimeout 里决定，因此「数据先到」与「超时」只有一个胜者：
// 两者同时就绪时优先采用已经到达的数据。成功路径不会触发 onTimeout。
func waitWithin[T any](ch <-chan T, d time.Duration, onTimeout func()) (T, bool) {
	expired := make(chan struct{}, 1)
	t := time.AfterFunc(d, func() {
		select {
		case expired <- struct{}{}:
		default:
		}
	})
	defer t.Stop()
	select {
	case v := <-ch:
		return v, true
	case <-expired:
		select {
		case v := <-ch:
			return v, true
		default:
			onTimeout()
			var zero T
			return zero, false
		}
	}
}

// cancelOnClose 在 body 关闭后取消请求 context，避免成功路径长期持有
// 一个永不取消的 context。
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}
