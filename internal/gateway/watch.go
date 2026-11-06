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

// streamWatch 在边下边发的同时按秒采样，判断这台源是不是已经慢到值得换。
// 它不做预测：只看刚过去的一段真实字节数。
type streamWatch struct {
	window    time.Duration
	limitKB   int64
	minFile   int64
	maxSent   int64
	maxSwitch int
	gap       time.Duration

	winStart time.Time
	winBytes int64
	samples  []int64 // 每秒增量，最近窗口
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

// add 累计新写入客户端的字节，并按秒切分窗口。
func (w *streamWatch) add(n int64) {
	if n <= 0 {
		return
	}
	now := time.Now()
	w.winBytes += n
	if now.Sub(w.winStart) >= time.Second {
		w.samples = append(w.samples, w.winBytes)
		if max := int(w.window / time.Second); max > 0 && len(w.samples) > max {
			w.samples = w.samples[len(w.samples)-max:]
		}
		w.winBytes = 0
		w.winStart = now
	}
	w.lastAt = now
}

// slow 判断是否该换源。只有窗口攒满、并且这次请求还有资格换的时候才返回真。
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
	need := int(w.window / time.Second)
	if need <= 0 || len(w.samples) < need {
		return false
	}
	var sum int64
	for _, v := range w.samples[len(w.samples)-need:] {
		sum += v
	}
	avgKB := sum / int64(need) / 1000
	return avgKB < w.limitKB
}

// sample 返回当前窗口的平均速度（KB/s），用于记录实时样本。
func (w *streamWatch) sample() (int64, int) {
	need := int(w.window / time.Second)
	if need <= 0 || len(w.samples) == 0 {
		return 0, 0
	}
	if len(w.samples) < need {
		need = len(w.samples)
	}
	var sum int64
	for _, v := range w.samples[len(w.samples)-need:] {
		sum += v
	}
	return sum / int64(need) / 1000, need
}

// setGiveUp 标记本次请求不再尝试换源（没有可换的源，或新源首字节超时）。
func (w *streamWatch) setGiveUp() { w.giveUp = true }

func (w *streamWatch) afterSwitch() {
	w.switches++
	w.lastSwitch = time.Now()
	w.samples = nil
	w.winBytes = 0
	w.winStart = time.Now()
}

// trySwitch 在旧连接不动的前提下，向下一台有快样本的星标发一次请求。
// 只有 1 秒内读到第一块数据才算成功，调用方拿到首块后再决定关旧连接。
func (g *Gateway) trySwitch(ctx context.Context, plan *adapter.Plan, cur *fetchTarget, sent int64, tried map[int64]bool) (*http.Response, *fetchTarget, []byte, error) {
	if g.st == nil || sent <= 0 {
		return nil, nil, nil, errors.New("没有可用的换源条件")
	}
	for _, t := range g.targets(plan, nil) {
		if t.c == nil || !t.c.Preferred || tried[t.c.ID] || (cur != nil && cur.c != nil && cur.c.ID == t.c.ID) {
			continue
		}
		// 只换到有真实快样本的源：没样本时赌它快，代价是几十秒空档，不划算。
		sp := g.st.RecentProxySpeeds(t.c.ID, plan.Kind, 1)
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
		if resp.StatusCode == http.StatusPartialContent {
			start, _, perr := parseContentRange(resp.Header.Get("Content-Range"))
			if perr != nil || start != sent {
				timer.Stop()
				resp.Body.Close()
				cancel()
				continue
			}
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
		log.Printf("单流换源：%s 已接手（已发 %d MB）", t.c.Name, sent>>20)
		return resp, &t, buf[:n], nil
	}
	return nil, nil, nil, fmt.Errorf("没有可换的星标源")
}

// discardN 把新源从 0 开始的那段字节读掉，只留下 sent 之后的内容。
func discardN(r io.Reader, n int64) error {
	if n <= 0 {
		return nil
	}
	_, err := io.CopyN(io.Discard, r, n)
	return err
}
