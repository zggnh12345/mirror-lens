package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
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
	avgKB := sum / int64(dur/time.Second) / 1000
	return avgKB < w.limitKB
}

// sample 返回当前窗口的平均速度（KB/s）与覆盖的真实秒数。
func (w *streamWatch) sample() (int64, int) {
	sum, dur := w.windowStats()
	sec := int(dur / time.Second)
	if sec <= 0 {
		return 0, 0
	}
	if max := int(w.window / time.Second); max > 0 && sec > max {
		sec = max
	}
	return sum / int64(sec) / 1000, sec
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

// verifierMatch 判断两份响应头能否证明是同一资源版本。
// ETag 双方都有时以 ETag 为准；否则用 Last-Modified；两者都无法比较
// 时返回 (false,false)，由调用方决定是否允许用摘要兜底。
// 这里不做 W/ 归一：强弱验证器混用一律按不一致处理，宁可少换源。
func verifierMatch(oldH, newH http.Header) (bool, bool) {
	oe, ne := oldH.Get("ETag"), newH.Get("ETag")
	if oe != "" && ne != "" {
		return oe == ne, true
	}
	ol, nl := oldH.Get("Last-Modified"), newH.Get("Last-Modified")
	if ol != "" && nl != "" {
		return ol == nl, true
	}
	return false, false
}

// hasVerifier 表示这份响应带了任一强/弱验证器。
func hasVerifier(h http.Header) bool {
	return h.Get("ETag") != "" || h.Get("Last-Modified") != ""
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
	// 没有可比的同类型验证器：只要有一侧带了验证器，就无法证明是同一版本。
	// （拿一侧的 ETag 去比另一侧的 Last-Modified 不算证据。）
	if hasVerifier(curHdr) || hasVerifier(newHdr) {
		return false, false
	}
	// 双方都没有验证器：只有可信摘要能兜底。
	return false, expectSHA != ""
}

// trySwitch 在旧连接不动的前提下，向下一台有快样本的星标发一次请求。
// 只有 1 秒内读到第一块数据、并且能证明与旧源是同一版本，才算成功。
// verified 为假表示没有可比验证器，只能靠 ExpectSHA 事后兜底。
func (g *Gateway) trySwitch(ctx context.Context, plan *adapter.Plan, cur *fetchTarget, curHdr http.Header, sent, total int64, tried map[int64]bool) (*http.Response, *fetchTarget, []byte, bool, error) {
	if sent <= 0 || total < 0 {
		return nil, nil, nil, false, errors.New("没有可用的换源条件")
	}
	for _, t := range g.switchCandidates(plan) {
		if t.c == nil || !t.c.Preferred || tried[t.c.ID] || (cur != nil && cur.c != nil && cur.c.ID == t.c.ID) {
			continue
		}
		// 只换到有真实快样本的源：没样本时赌它快，代价是几十秒空档，不划算。
		sp := g.recentProxySpeeds(t.c.ID, plan.Kind, 1)
		if len(sp) == 0 || sp[0] < g.par.SwitchMinMBps*1024 {
			continue
		}
		tried[t.c.ID] = true

		// 3 秒只限制「等到首字节」：读到数据后不能取消 ctx，否则 body 立刻断。
		rctx, cancel := context.WithCancel(ctx)
		timer := time.AfterFunc(3*time.Second, cancel)
		req, err := http.NewRequestWithContext(rctx, http.MethodGet, t.u.String(), nil)
		if err != nil {
			timer.Stop()
			cancel()
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

		resp, err := g.cli.Do(req)
		if err != nil {
			timer.Stop()
			cancel()
			continue
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			timer.Stop()
			resp.Body.Close()
			cancel()
			continue
		}
		verified, ok := switchAcceptable(curHdr, resp.Header, resp.StatusCode, resp.ContentLength, sent, total, plan.ExpectSHA)
		if !ok {
			timer.Stop()
			resp.Body.Close()
			cancel()
			continue
		}
		buf := make([]byte, 64<<10)
		n, rerr := resp.Body.Read(buf)
		timer.Stop()
		if n <= 0 {
			cancel()
			resp.Body.Close()
			if rerr == nil {
				rerr = io.ErrNoProgress
			}
			continue
		}
		log.Printf("单流换源：%s 已接手（已发 %d MB，验证器=%v）", t.c.Name, sent>>20, verified)
		return resp, &t, buf[:n], verified, nil
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
