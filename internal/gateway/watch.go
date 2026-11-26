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
	"github.com/zggnh12345/mirror-lens/internal/config"
	"github.com/zggnh12345/mirror-lens/internal/logging"
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
	maxSent   int64 // 绝对已发上限（配置项），与大文件下的 0.8×total 取 min
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
		maxSent:   config.NormalizeMaxSentMiB(o.SwitchMaxSentMiB) << 20,
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
	if w.giveUp || w.maxSwitch == 0 || total < w.minFile || sent <= 0 {
		return false
	}
	// 进度闸门：已发字节不能超过 min(绝对上限, 0.8×total)。
	// 0.8×total 用整数运算，避免 total×8 溢出；maxSent 已在构造时归一化，
	// <=0 或 >150MiB 都按 150MiB 硬上限处理。
	allow := total/10*8 + (total%10)*8/10 // floor(0.8×total)
	if w.maxSent > 0 && allow > w.maxSent {
		allow = w.maxSent
	}
	if sent > allow {
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

// isFullSHA256 只认完整的 64 位十六进制 SHA-256。
// 一侧有强 ETag、另一侧没有时，只有完整摘要才足以兜底；短摘要/占位值不放行。
func isFullSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// switchSpeedOK 判断样本够不够快、能不能作为中途换源目标。
// 少于 3 条不下结论：单条样本在慢启动/瞬时拥塞下偏差太大。
func switchSpeedOK(sp []int64, minKBps int64) bool {
	if len(sp) < 3 {
		return false
	}
	return medianInt64(sp) >= minKBps
}

// dynamicThresholdBps 返回候选 i 的动态准入门槛 T_i = max(F, P_i/1.3, C×1.2)，单位 B/s。
//   - F：绝对速度下限（g.par.SwitchFloorMiBPerSec × 1024²，MiB/s→B/s），默认 1MiB/s
//   - P_i：排除当前源与候选 i 自身后，其他合格候选样本中位数的中位数；无同伴取 0
//   - C：当前供数源实时速度（B/s）
//
// P_i 用「/1.3」而非「×1.3」：后者会让候选被自己所在群体中位数淘汰。
func (g *Gateway) dynamicThresholdBps(pBps, cBps int64) int64 {
	fBps := g.par.SwitchFloorMiBPerSec * 1024 * 1024
	pi := pBps * 10 / 13 // P_i / 1.3，整数运算避免浮点
	cx := cBps * 12 / 10 // C × 1.2
	t := fBps
	if pi > t {
		t = pi
	}
	if cx > t {
		t = cx
	}
	return t
}

// peerMedianBps 计算候选 i 的同伴基线 P_i（B/s）：
// 排除当前源、候选 i 自身，以及本次请求中已经尝试过的候选（excluded）。
// 已尝试意味着 lease/连接/状态码等已证明其本轮不可用，不能再用历史高速样本
// 抬高其他候选的门槛。历史样本是 b/ms（十进制 kB/s），这里统一 ×1000 转 B/s。
func (g *Gateway) peerMedianBps(plan *adapter.Plan, cur *fetchTarget, excludeID int64, excluded map[int64]bool) int64 {
	cands := g.switchCandidates(plan)
	var peers []int64
	for _, t := range cands {
		if t.c == nil || !t.c.Preferred || t.c.ID == excludeID || excluded[t.c.ID] {
			continue
		}
		if cur != nil && cur.c != nil && cur.c.ID == t.c.ID {
			continue
		}
		sp := g.recentProxySpeeds(t.c.ID, plan.Kind, 5)
		if len(sp) < 3 {
			continue
		}
		peers = append(peers, medianInt64(sp)*1000)
	}
	if len(peers) == 0 {
		return 0
	}
	return medianInt64(peers)
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

// recentProxySpeeds 查某台源最近的实测吞吐（b/ms，即十进制 kB/s），测试可注入。
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
//
// 3.5.23 F1/F2：中途接管的统一资格过滤落点。候选必须已验证支持 Range
// （RangeCap == 1），RangeCap 为 0（不支持）或 -1（未检测）一律排除。
// peerMedianBps 与 trySwitchWithSpeed 都经由此函数，因此测速同伴集合与
// 实际接管候选集合天然一致；候选失败后 excluded 重算也基于同一过滤集合。
//
// 过滤只能写在这里，不得下沉到 targets()/parTargets 注入层或 fetchTarget
// 构造层：g.parTargets 是换源/并行填充/中途并行三路共享注入点，下沉会污染
// 并行下载路径（并行有自己的 rangeKnownOff 门槛）。
func (g *Gateway) switchCandidates(plan *adapter.Plan) []fetchTarget {
	var cands []fetchTarget
	if g.parTargets != nil {
		cands = g.parTargets(plan)
	} else {
		cands = g.targets(plan, nil)
	}
	// 新建切片，不复用 cands 底层数组：g.targets 可能返回缓存派生的列表，
	// 原地过滤会污染共享缓存。
	out := make([]fetchTarget, 0, len(cands))
	for _, t := range cands {
		if t.c == nil || t.c.RangeCap != 1 {
			continue
		}
		out = append(out, t)
	}
	return out
}

// switchAcceptable 判断新响应能否接在 sent 之后。
// verified 为假表示没有可比验证器、靠 ExpectSHA 兜底。
func switchAcceptable(curHdr, newHdr http.Header, status int, clen, sent, total int64, expectSHA string) (verified, ok bool) {
	if total < 0 || sent <= 0 {
		return false, false
	}
	switch status {
	case http.StatusPartialContent:
		start, end, tot, err := parseContentRange(newHdr.Get("Content-Range"))
		if err != nil || start != sent || end != total-1 || tot != total {
			return false, false
		}
		if clen >= 0 && clen != total-sent {
			return false, false
		}
	case http.StatusOK:
		// 3.5.23 F3：中途接管只接受 HTTP 206。候选返回 200 意味着它忽略了
		// Range 从 0 重发，回放 sent 字节依赖历史速度估算且旧源已关闭、无回退，
		// 是已定位的客户端失败路径（见 3.5.22 §4.8 / 3.5.23 §一）。长度即使
		// 一致也一律拒绝。此限制只作用于换源接管，不影响初始源/普通故障转移。
		return false, false
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
	// 一侧有强 ETag、另一侧没有：不能用 ETag 证明同版本。
	// 只有完整 64 位 SHA-256 摘要才允许兜底（verified=false）；短摘要/占位值拒绝。
	if hasStrongVerifier(curHdr) != hasStrongVerifier(newHdr) {
		return false, isFullSHA256(expectSHA)
	}
	// 双方都没有强验证器：只有可信摘要能兜底。
	return false, expectSHA != ""
}

func skipReason(t fetchTarget, tried map[int64]bool, cur *fetchTarget) string {
	switch {
	case t.c == nil:
		return "官方目标，不是星标源"
	case !t.c.Preferred:
		return "未勾选星标"
	case tried[t.c.ID]:
		return "本轮已试过"
	case cur != nil && cur.c != nil && cur.c.ID == t.c.ID:
		return "就是当前源"
	}
	return "未知"
}

// trySwitch 是 trySwitchWithSpeed 的兼容包装：当前源速度未知（C=0）。
// 老测试与不需要当前速度的调用方用它；真实换源路径传 C。
func (g *Gateway) trySwitch(ctx context.Context, plan *adapter.Plan, cur *fetchTarget, curHdr http.Header, sent, total int64, tried map[int64]bool) (*http.Response, *fetchTarget, []byte, bool, error) {
	return g.trySwitchWithSpeed(ctx, plan, cur, curHdr, sent, total, 0, tried)
}

// trySwitchWithSpeed 在旧连接不动的前提下，向下一台有快样本的星标发一次请求。
// 只有 1 秒内读到第一块数据、并且能证明与旧源是同一版本，才算成功。
// verified 为假表示没有可比验证器，只能靠 ExpectSHA 事后兜底。
// curBps 是当前供数源实时速度（B/s），用于动态门槛 C×1.2。
func (g *Gateway) trySwitchWithSpeed(ctx context.Context, plan *adapter.Plan, cur *fetchTarget, curHdr http.Header, sent, total, curBps int64, tried map[int64]bool) (*http.Response, *fetchTarget, []byte, bool, error) {
	if sent <= 0 || total < 0 {
		return nil, nil, nil, false, errors.New("没有可用的换源条件")
	}
	cands := g.switchCandidates(plan)
	// 一旦开始尝试某候选，本轮就不再把它当作 P_i 的同伴；失败后下一个候选
	// 立即用剩余有效同伴重算，避免被已证明不可用的高速源抬高门槛。
	excluded := map[int64]bool{}
	log.Printf("换源候选：n=%d official_only=%v sched=%q par_targets=%v",
		len(cands), plan.OfficialOnly, plan.Sched(), g.parTargets != nil)
	for _, t := range cands {
		if t.c == nil || !t.c.Preferred || tried[t.c.ID] || (cur != nil && cur.c != nil && cur.c.ID == t.c.ID) {
			name := "official"
			if t.c != nil {
				name = t.c.Name
			}
			log.Printf("换源拒绝 %s reason=not_eligible detail=%s", name, skipReason(t, tried, cur))
			continue
		}
		// 只换到有真实快样本的源：没样本时赌它快，代价是几十秒空档，不划算。
		// 取最近 5 条的中位数：生产样本 CV>1.4，最新一条的峰值会让慢源插队。
		sp := g.recentProxySpeeds(t.c.ID, plan.Kind, 5)
		if len(sp) < 3 {
			log.Printf("换源拒绝 %s reason=speed_rejected detail=insufficient_samples kind=%q n=%d sp=%v",
				t.c.Name, plan.Kind, len(sp), sp)
			continue
		}
		pBps := g.peerMedianBps(plan, cur, t.c.ID, excluded)
		thresholdBps := g.dynamicThresholdBps(pBps, curBps)
		sBps := medianInt64(sp) * 1000
		if sBps < thresholdBps {
			log.Printf("换源拒绝 %s reason=speed_rejected detail=below_threshold kind=%q s_bps=%d p_bps=%d c_bps=%d threshold_bps=%d sp=%v",
				t.c.Name, plan.Kind, sBps, pBps, curBps, thresholdBps, sp)
			continue
		}
		tried[t.c.ID] = true
		excluded[t.c.ID] = true

		// 交接用预留名额：拿不到就放弃本次换源，绝不排队（I6）。
		l, lerr := g.leaseFor(ctx, t, scheduler.KindHandover)
		if lerr != nil {
			log.Printf("换源拒绝 %s reason=lease_rejected err=%v", t.c.Name, logging.SafeErr(lerr))
			continue
		}

		// 首字节预算只约束「等到首字节」：读到数据后绝不能再取消 ctx，
		// 否则 body 立刻断。超时信号只投递，由 waitWithin 唯一裁决胜负。
		budget := g.switchTimeout()
		start := time.Now()
		rctx, cancel := context.WithCancel(ctx)
		unbind := func() {}
		if l != nil {
			// Reclaim("stall") 只能取消登记过的函数；不绑定的话看门狗会在
			// 底层 Body.Close 上等一个永远不返回的 Read。
			unbind = l.BindCancel(cancel)
			if rctx.Err() != nil {
				// 名额在绑定前已失效，登记即取消：不允许继续拨号。
				log.Printf("换源拒绝 %s reason=lease_rejected detail=expired_before_bind", t.c.Name)
				unbind()
				cancel()
				releaseLease(l)
				continue
			}
		}
		rctx, err := g.withRegistryPolicy(rctx, plan, sourceIDFor(t), logicalHostFor(t, plan), t.u)
		if err != nil {
			log.Printf("换源拒绝 %s reason=request_failed stage=policy err=%v", t.c.Name, logging.SafeErr(err))
			unbind()
			cancel()
			releaseLease(l)
			continue
		}
		req, err := http.NewRequestWithContext(rctx, http.MethodGet, t.u.String(), nil)
		if err != nil {
			log.Printf("换源拒绝 %s reason=request_failed stage=new_request err=%v", t.c.Name, logging.SafeErr(err))
			unbind()
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
			log.Printf("换源拒绝 %s reason=request_failed stage=first_byte_budget", t.c.Name)
			unbind()
			releaseLease(l)
			continue
		}
		resp, err := d.resp, d.err
		if err != nil {
			log.Printf("换源拒绝 %s reason=request_failed stage=do err=%v", t.c.Name, logging.SafeErr(err))
			unbind()
			cancel()
			releaseLease(l)
			continue
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			// 先取消：Close 可能因为要读剩余数据而阻塞，届时 Reclaim 已
			// 找不到取消函数，等于把名额锁死。
			log.Printf("换源拒绝 %s reason=request_failed stage=bad_status status=%d", t.c.Name, resp.StatusCode)
			cancel()
			unbind()
			resp.Body.Close()
			releaseLease(l)
			continue
		}
		verified, vok := switchAcceptable(curHdr, resp.Header, resp.StatusCode, resp.ContentLength, sent, total, plan.ExpectSHA)
		if !vok {
			log.Printf("换源拒绝 %s reason=verifier_rejected status=%d", t.c.Name, resp.StatusCode)
			cancel()
			unbind()
			resp.Body.Close()
			releaseLease(l)
			continue
		}
		// HTTP 200 = 候选忽略 Range 从 0 重发，必须回放 sent 字节。
		// 回放预算独立设定（不继承首字节预算）：按候选样本速度估算，超预算直接拒绝，
		// 避免为了省几十秒却卡在丢弃前缀上更久。回放过程本身由请求 ctx 取消。
		if resp.StatusCode == http.StatusOK && !g.replayCostOK(sent, sBps) {
			log.Printf("换源拒绝 %s reason=replay_over_budget sent=%d s_bps=%d budget=%v",
				t.c.Name, sent, sBps, g.replayTimeout())
			cancel()
			unbind()
			resp.Body.Close()
			releaseLease(l)
			continue
		}

		remaining := budget - time.Since(start)
		if remaining <= 0 {
			log.Printf("换源拒绝 %s reason=first_byte_timeout detail=budget_exhausted", t.c.Name)
			cancel()
			unbind()
			resp.Body.Close()
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
			log.Printf("换源拒绝 %s reason=first_byte_timeout detail=no_first_byte n=%d", t.c.Name, rr.n)
			cancel()
			unbind()
			resp.Body.Close()
			releaseLease(l)
			continue
		}
		wrapped, werr := wrapLease(resp, l)
		if werr != nil {
			log.Printf("换源拒绝 %s reason=lease_rejected detail=wrap_failed err=%v", t.c.Name, logging.SafeErr(werr))
			cancel()
			unbind()
			resp.Body.Close()
			releaseLease(l)
			continue
		}
		resp = wrapped
		// 换源接管走的是读循环直用路径，不会经过 stream.go 里那次
		// *leaseBody 断言；这里补上同一道空闲看门狗，避免调用方既不读
		// 也不关时名额长期占用。必须在 pendingErrBody 包装之前做，
		// 否则断言拿到的是 *pendingErrBody，看门狗不会启动。
		if lb, ok := resp.Body.(*leaseBody); ok && l != nil {
			go lb.watch(90 * time.Second)
		}
		if rr.err != nil {
			// Read 允许同时返回 n>0 和非 nil error；字节已作为 first 交付，
			// 错误必须留给下一次 Read，不能被吞掉当成正常 EOF。
			resp.Body = &pendingErrBody{ReadCloser: resp.Body, err: rr.err}
		}
		// 成功接管的 body 关闭时才取消 rctx：既不泄漏 context，也保证
		// 迟到的超时信号永远不能打断已经开始的读取。
		resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel, unbind: unbind}
		holderFrom(ctx).set(l)
		log.Printf("换源成功 %s reason=switch_success sent=%d verified=%v s_bps=%d p_bps=%d c_bps=%d threshold_bps=%d",
			t.c.Name, sent, verified, sBps, pBps, curBps, thresholdBps)
		return resp, &t, buf[:rr.n], verified, nil
	}
	return nil, nil, nil, false, fmt.Errorf("没有可换的星标源")
}

// discardN 把新源从 0 开始的那段字节读掉，只留下 sent 之后的内容。
// 回放过程可被 ctx 取消（客户端断开/预算回收），避免卡在丢弃前缀上。
func discardN(ctx context.Context, r io.Reader, n int64) error {
	if n <= 0 {
		return nil
	}
	buf := make([]byte, 32<<10)
	var done int64
	for done < n {
		if err := ctx.Err(); err != nil {
			return err
		}
		want := int64(len(buf))
		if remain := n - done; remain < want {
			want = remain
		}
		got, err := r.Read(buf[:want])
		if got > 0 {
			done += int64(got)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// discardWithDeadline 在独立 deadline 内丢弃 n 字节。io.Reader 不认 context，
// 超时/取消时必须 Close 底层 Body 才能让阻塞中的 Read 退出。
func discardWithDeadline(ctx context.Context, body io.ReadCloser, n int64, d time.Duration) error {
	replayCtx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	stop := context.AfterFunc(replayCtx, func() { _ = body.Close() })
	defer stop()
	return discardN(replayCtx, body, n)
}

// switchFirstByteBudget 是默认的首字节等待预算。
const switchFirstByteBudget = 3 * time.Second

// switchReplayBudget 是 HTTP 200 候选回放已发前缀的独立预算，不继承首字节预算。
// 数值为保守初值，需按文档 §3.2 在真实环境校准；测试可通过 Gateway.switchReplayBudget 注入。
const switchReplayBudget = 5 * time.Second

func (g *Gateway) switchTimeout() time.Duration {
	if g.switchBudget > 0 {
		return g.switchBudget
	}
	return switchFirstByteBudget
}

func (g *Gateway) replayTimeout() time.Duration {
	if g.switchReplayBudget > 0 {
		return g.switchReplayBudget
	}
	return switchReplayBudget
}

// replayCostOK 估算 HTTP 200 回放 sent 字节的成本（按候选样本速度 sBps），
// 超过独立回放预算则拒绝该候选。样本速度单位为 B/s。
func (g *Gateway) replayCostOK(sent, sBps int64) bool {
	if sent <= 0 {
		return true
	}
	if sBps <= 0 {
		return false // 速度未知，回放时间无法上界，保守拒绝
	}
	budget := g.replayTimeout()
	// 预估回放秒数 = sent / sBps。用浮点只做阈值比较，不参与计费；
	// 避免整数交叉相乘时 sent×1e9 溢出（大文件已发可到 GiB 级）。
	seconds := float64(sent) / float64(sBps)
	return seconds <= budget.Seconds()
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
		if v, ok := afterTimeout(ch); ok {
			return v, true
		}
		onTimeout()
		var zero T
		return zero, false
	}
}

// afterTimeout 是超时胜出前的最后一次裁决：此刻 channel 有值则数据胜出。
// 独立成函数，才能脱离 select 的随机性做确定性测试。
func afterTimeout[T any](ch <-chan T) (T, bool) {
	select {
	case v := <-ch:
		return v, true
	default:
		var zero T
		return zero, false
	}
}

// cancelOnClose 在 body 关闭后取消请求 context，避免成功路径长期持有
// 一个永不取消的 context。
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
	unbind func()
}

func (c *cancelOnClose) Close() error {
	// 正常完成：先关 Body 让底层连接尽量复用，再解绑并取消 Context。
	err := c.ReadCloser.Close()
	if c.unbind != nil {
		c.unbind()
	}
	c.cancel()
	return err
}

// pendingErrBody 把「一批字节 + 同一次 Read 返回的错误」拆成两次返回：
// 字节已经由 trySwitch 作为 first 交付，错误在下一次 Read 交给流处理。
type pendingErrBody struct {
	io.ReadCloser
	err error
}

func (b *pendingErrBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		// io.Reader 约定：零长度读不消费任何状态，也不能吞掉待交付的错误。
		return 0, nil
	}
	if b.err != nil {
		err := b.err
		b.err = nil
		return 0, err
	}
	return b.ReadCloser.Read(p)
}
