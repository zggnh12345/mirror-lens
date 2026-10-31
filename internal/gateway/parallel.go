package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 并行分片下载（阶段 7）。
//
// 允许并行的唯一前提是「有可信的整份摘要」——目前只有 Docker 按 digest 拉 blob。
// 分片来自不同源，一旦某个源给的是另一个版本，每一片都能单独下成功，拼出来的
// 文件却是错的。只有最后的 SHA-256 能发现这件事，所以没有摘要就一律单流。
//
// 三条硬规则：
//  1. 只在写任何字节之前判断能不能并行，写出去之后不再改主意。
//  2. 每片都校验 Content-Range 的起点、长度和总长；任何一项对不上就换官方源重取，
//     仍不行就断开连接（客户端看到的是截断，不是一份"成功"的坏文件）。
//  3. 拼完必须整份哈希等于 ExpectSHA 才允许发布；不等就丢弃，一次都不写索引。

// parSource 是一台通过预检的源。
type parSource struct {
	t     fetchTarget
	name  string
	ctype string
}

// tryParallel 尝试分片并行填充。
//
// 返回 true 表示这次响应已经由它接管（可能已经写出字节）；返回 false 表示
// 什么都没写，调用方继续走原来的单流路径。
func (g *Gateway) tryParallel(w http.ResponseWriter, r *http.Request, plan *adapter.Plan) bool {
	if !g.par.Enabled || g.par.Workers < 2 || g.par.ChunkSize <= 0 {
		return false
	}
	// 没有可信摘要就没有拼装的资格；客户端自己带 Range 的（续传）也不掺和。
	if plan.ExpectSHA == "" || !plan.Stream || !plan.Cacheable || r.Header.Get("Range") != "" {
		return false
	}
	if _, ok := g.cache.ReadPartial(plan.CacheKey); ok {
		return false
	}
	targets := g.targets(plan, nil)
	if g.parTargets != nil {
		targets = g.parTargets(plan)
	}
	if len(targets) < 2 {
		g.decline("只有官方源可用")
		return false
	}

	ctx := r.Context()
	srcs, total := g.parPreflight(ctx, plan, targets)
	if len(srcs) < 2 {
		g.decline(fmt.Sprintf("%d 台源里只有 %d 台支持分片", len(targets), len(srcs)))
		return false
	}
	if total < g.par.MinSize {
		g.decline(fmt.Sprintf("文件 %d 字节小于分片下限 %d", total, g.par.MinSize))
		return false
	}
	if total > g.limitFor(plan) {
		g.decline(fmt.Sprintf("文件 %d 字节超过单文件上限 %d", total, g.limitFor(plan)))
		return false
	}

	chunk := g.par.ChunkSize
	if chunk > total/2 {
		chunk = total / 2
	}
	if chunk < 256<<10 {
		chunk = 256 << 10
	}
	n := int((total + chunk - 1) / chunk)

	// 先开 .part，再写响应头：开不了文件还能干净地退回单流。
	if err := g.cache.ResetPartial(plan.CacheKey); err != nil {
		log.Printf("并行填充：建 .part 失败，退回单流: %v", err)
		g.parFalls.Add(1)
		return false
	}
	part, err := g.cache.OpenPartial(plan.CacheKey)
	if err != nil {
		log.Printf("并行填充：打开 .part 失败，退回单流: %v", err)
		g.parFalls.Add(1)
		return false
	}
	drop := func() {
		part.Close()
		g.cache.DropPartial(plan.CacheKey)
	}

	firstLen := chunk
	if n == 1 {
		firstLen = total
	}
	resp0, err := g.rangeReq(ctx, plan, srcs[0].t.u, 0, firstLen-1)
	if err != nil {
		drop()
		g.parFalls.Add(1)
		return false
	}
	if start, tot, perr := parseContentRange(resp0.Header.Get("Content-Range")); resp0.StatusCode != http.StatusPartialContent ||
		perr != nil || start != 0 || tot != total {
		resp0.Body.Close()
		drop()
		g.parFalls.Add(1)
		return false
	}

	// 从这里开始就是承诺了：响应头一旦写出，失败只能断开连接。
	h := w.Header()
	ctype := resp0.Header.Get("Content-Type")
	if ctype == "" {
		ctype = srcs[0].ctype
	}
	if ctype != "" {
		h.Set("Content-Type", ctype)
	}
	copyHeader(h, resp0.Header, "Content-Disposition", "Content-Language")
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Length", strconv.FormatInt(total, 10))
	h.Set("X-ML-Cache", "MISS")
	h.Set("X-ML-Parallel", fmt.Sprintf("%d 段 / %d 源 / %d 并发", n, len(srcs), g.par.Workers))
	h.Set("X-ML-Source", strings.Join(parNames(srcs), ","))
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	start := time.Now()
	g.parFills.Add(1)
	g.parBytes.Add(total)

	// 剩下的段先并发跑起来，边跑边把第 0 段发给客户端。
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	dones := make([]chan error, n)
	// 每台源在这次下载里实际供了几段、失败了几段。预检通过只说明支持 Range，
	// 不代表它真的把数据给全了；按实际结果记账才不会把坏源记成好源。
	stats := make([]parStat, len(srcs))

	sem := make(chan struct{}, g.par.Workers)
	var wg sync.WaitGroup
	// 中途放弃时必须先 cancel 再等所有分片协程退出，最后才能删 .part：
	// 反过来会让协程往已关闭（或已被下一次请求重建）的文件里写。
	abort := func() {
		cancel()
		wg.Wait()
		drop()
	}
	for i := 1; i < n; i++ {
		off := int64(i) * chunk
		length := chunk
		if off+length > total {
			length = total - off
		}
		dones[i] = make(chan error, 1)
		src := srcs[i%len(srcs)]
		wg.Add(1)
		go func(i int, off, length int64, src parSource) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				dones[i] <- ctx.Err()
				return
			}
			dones[i] <- g.parChunk(ctx, plan, src, i%len(srcs), off, length, total, part, stats)
		}(i, off, length, src)
	}

	// 第 0 段：边收边发给客户端，同时写进 .part。
	buf := make([]byte, 64<<10)
	var got int64
	for got < firstLen {
		nr, rerr := resp0.Body.Read(buf)
		if nr > 0 {
			if _, werr := w.Write(buf[:nr]); werr != nil {
				resp0.Body.Close()
				abort()
				return true // 客户端走了，不落缓存（分片是乱序的，留下也没法续）
			}
			if _, werr := part.WriteAt(buf[:nr], got); werr != nil {
				resp0.Body.Close()
				abort()
				log.Printf("并行填充：写 .part 失败，断开: %v", werr)
				return true
			}
			got += int64(nr)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			clientGone := r.Context().Err() != nil
			resp0.Body.Close()
			abort()
			if !clientGone {
				atomic.AddInt32(&stats[0].fail, 1)
			}
			log.Printf("并行填充：第 0 段中断 %s: %v", srcs[0].name, rerr)
			return true
		}
	}
	resp0.Body.Close()
	if got != firstLen {
		clientGone := r.Context().Err() != nil
		abort()
		if !clientGone {
			atomic.AddInt32(&stats[0].fail, 1)
		}
		log.Printf("并行填充：第 0 段长度不符，期望 %d 实得 %d", firstLen, got)
		return true
	}
	atomic.AddInt32(&stats[0].ok, 1)

	// 其余段按顺序补给客户端：谁先到不影响顺序，客户端看到的永远是从头开始的字节流。
	for i := 1; i < n; i++ {
		if err := <-dones[i]; err != nil {
			abort()
			log.Printf("并行填充：第 %d 段失败，断开连接: %v", i, err)
			return true
		}
		off := int64(i) * chunk
		length := chunk
		if off+length > total {
			length = total - off
		}
		if _, werr := io.CopyN(w, io.NewSectionReader(part, off, length), length); werr != nil {
			abort()
			return true
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	wg.Wait()

	// 整份校验：只有它能把「每片都成功、拼起来是错的」这种情况拦下来。
	if _, err := part.Seek(0, io.SeekStart); err != nil {
		drop()
		log.Printf("并行填充：回读 .part 失败: %v", err)
		return true
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, part); err != nil {
		drop()
		log.Printf("并行填充：哈希失败: %v", err)
		return true
	}
	sha := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(sha, plan.ExpectSHA) {
		g.verifyFail.Add(1)
		drop()
		// 拼起来是错的，但这只证明「组合」错了，不证明每台源都给错了内容。
		// 只有一台源时归因明确，照实记失败；多台时谁都可能，宁可不动评分，
		// 否则正常源会跟着被熔断，越修越差。
		if len(srcs) == 1 {
			g.reportSource(plan.Kind, srcs[0].t, false, time.Since(start))
		}
		log.Printf("并行填充：整份校验失败，不入缓存：期望 %s 实得 %s (%s)", plan.ExpectSHA, sha, parNames(srcs))
		return true
	}

	part.Close()
	partPath, _ := g.cache.PartialPaths(plan.CacheKey)
	unlock := g.cache.LockBlob(sha)
	if _, err := g.cache.Commit(partPath, sha); err != nil {
		unlock()
		g.cache.DropPartial(plan.CacheKey)
		log.Printf("并行填充：落盘失败: %v", err)
		return true
	}
	e := store.CacheEntry{
		Key:          plan.CacheKey,
		SHA256:       sha,
		Size:         total,
		ContentType:  ctype,
		ETag:         resp0.Header.Get("ETag"),
		LastModified: resp0.Header.Get("Last-Modified"),
		Upstream:     plan.Upstream.String(),
		CreatedAt:    g.now().Unix(),
		TTL:          int64(plan.TTL.Seconds()),
		Verify:       store.VerifyVerified,
	}
	err = g.st.CachePut(e)
	unlock()
	if err != nil {
		log.Printf("并行填充：写索引失败 %s: %v", plan.CacheKey, err)
	}
	g.cache.DropPartial(plan.CacheKey)
	g.st.RecordTraffic(store.TrafficFill, store.KindOfKey(plan.CacheKey), total)
	// 按每台源实际供的段数记账：一段没供上的记失败，供过且没出错的才记成功。
	for i := range srcs {
		switch {
		case atomic.LoadInt32(&stats[i].fail) > 0:
			g.reportSource(plan.Kind, srcs[i].t, false, time.Since(start),
				scheduler.Hint{Code: int(atomic.LoadInt32(&stats[i].code)), Mode: "parallel"})
		case atomic.LoadInt32(&stats[i].ok) > 0:
			// 并行分片成功拿到的是 206，不是 200；不记的话这条成功事件
			// 在 429 率的分母里就不存在了。
			g.reportSource(plan.Kind, srcs[i].t, true, time.Since(start),
				scheduler.Hint{Code: http.StatusPartialContent, Mode: "parallel"})
		}
	}
	log.Printf("并行填充完成：%s %d 字节，%d 段 × %d 源，用时 %s", plan.CacheKey, total, n, len(srcs), time.Since(start).Round(time.Millisecond))
	return true
}

// parStat 记录一台源在一次并行下载里实际供成、供败的段数。
type parStat struct {
	ok, fail int32
	code     int32 // 最后一次非 206 的状态码，用来区分「限流」和「坏了」
}

// preflightStatusIsSourceFailure 判断预检的 HTTP 状态是否该计入源故障。
//
// reportSource(false) 不只是记一笔：它会推进熔断计数、拉长冷却，还会写
// source_events。所以只有「这台源现在真的不可用」才配触发它。认证、权限、
// 资源不存在、协议能力缺失、Range 不满足都属于请求与源不匹配，换台源或换个
// 对象就好，不代表源坏了。
func preflightStatusIsSourceFailure(status int) bool {
	switch status {
	case http.StatusRequestTimeout, // 408 临时超时
		http.StatusTooManyRequests: // 429 限流：现在别再来，冷却正是要的效果
		return true
	case http.StatusNotImplemented, // 501 没实现这个能力
		http.StatusHTTPVersionNotSupported,       // 505 协议版本不兼容
		http.StatusVariantAlsoNegotiates,         // 506 内容协商配置错，换请求头可解
		http.StatusLoopDetected,                  // 508 上游自己成环，配置问题不是负载
		http.StatusNotExtended,                   // 510 要求扩展策略，能力缺失
		http.StatusNetworkAuthenticationRequired: // 511 需要先过认证门户
		return false
	}
	// 425（Too Early）故意不记：它是「请求来早了，重放一遍」的提示，
	// 既不说明源过载，也不说明源坏了，重试即可。
	// 其余 5xx 算服务端异常（500/502/503/504，以及 507 存储满这类）。
	return status >= 500 && status <= 599
}

// parPreflight 用 1 字节的 Range 请求确认每台源支持分片、且说的是同一个总长。
// 官方源排在最前：总长以它为准，镜像报的只能用来核对，不能用来定义事实。
func (g *Gateway) parPreflight(ctx context.Context, plan *adapter.Plan, targets []fetchTarget) ([]parSource, int64) {
	ordered := make([]fetchTarget, 0, len(targets))
	for _, t := range targets {
		if t.u.String() == plan.Upstream.String() {
			ordered = append(ordered, t)
		}
	}
	for _, t := range targets {
		if t.u.String() != plan.Upstream.String() {
			ordered = append(ordered, t)
		}
	}
	var srcs []parSource
	total := int64(-1)
	for _, t := range ordered {
		if len(srcs) >= g.par.Workers {
			break
		}
		st := time.Now()
		resp, err := g.rangeReq(ctx, plan, t.u, 0, 0)
		if err != nil {
			// 连不上、超时才算源故障；本次任务被取消则中性。
			if ctx.Err() == nil {
				g.reportSource(plan.Kind, t, false, time.Since(st))
			}
			continue
		}
		start, tot, perr := parseContentRange(resp.Header.Get("Content-Range"))
		ctype := resp.Header.Get("Content-Type")
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		if resp.StatusCode != http.StatusPartialContent {
			// 不是 206：可能是不支持 Range（200）、认证/权限、对象不存在、
			// 限流或服务端异常。只有最后一类才记源故障。
			if ctx.Err() == nil && preflightStatusIsSourceFailure(resp.StatusCode) {
				// 429 的 Retry-After 交给调度器：源自己说了等多久，比固定退避准。
				// 解析不出来就是 0，退回本地退避。
				ra := time.Duration(0)
				if resp.StatusCode == http.StatusTooManyRequests {
					ra = retryAfterHeader(resp.Header)
					if ra > 0 {
						log.Printf("并行填充：%s 限流，Retry-After=%s", t.u.Host, ra)
					}
				}
				g.reportSource(plan.Kind, t, false, time.Since(st), scheduler.Hint{RetryAfter: ra, Code: resp.StatusCode, Mode: "chunk"})
			}
			log.Printf("并行填充：%s 不参与分片（HTTP %d，Content-Range=%q）",
				t.u.Host, resp.StatusCode, resp.Header.Get("Content-Range"))
			continue
		}
		if perr != nil || start != 0 || tot <= 0 {
			// 回了 206 却给不出合法的 Content-Range：这不是「没有 Range 能力」，
			// 是协议响应本身有问题，按源故障记。
			if ctx.Err() == nil {
				g.reportSource(plan.Kind, t, false, time.Since(st), scheduler.Hint{Code: resp.StatusCode, Mode: "chunk"})
			}
			log.Printf("并行填充：%s 返回异常 Range 响应（Content-Range=%q）",
				t.u.Host, resp.Header.Get("Content-Range"))
			continue
		}
		if total < 0 {
			total = tot
		} else if tot != total {
			// 两台源说的总长不一样：不是同一个对象，拼起来必错。
			// 版本不一致是镜像缓存滞后，不是源故障，同样不降权。
			log.Printf("并行填充：%s 报的总长 %d 与 %d 不符，排除", t.u.Host, tot, total)
			continue
		}
		// 预检只证明「支持 Range」，不代表这次真的传成功，不记成功样本。
		srcs = append(srcs, parSource{t: t, name: parName(t), ctype: ctype})
	}
	return srcs, total
}

// decline 记一次「本来可以并行但条件不满足」，并把原因写进日志。
// 这类日志只在请求带可信摘要时出现，量很小，值得留。
func (g *Gateway) decline(reason string) {
	g.parFalls.Add(1)
	log.Printf("并行填充：退回单流（%s）", reason)
}

func (g *Gateway) reportSource(kind string, t fetchTarget, ok bool, d time.Duration, hints ...scheduler.Hint) {
	if t.c != nil {
		g.sched.Report(kind, *t.c, ok, d, hints...)
	}
}

// parChunk 取一段写进 .part；先找分到的源，不行再回官方源重取一次。
// idx 是这台源在 srcs 里的下标，成败都记到 stats[idx]：只有真的供出这段字节才算成功。
func (g *Gateway) parChunk(ctx context.Context, plan *adapter.Plan, src parSource, idx int, off, length, total int64, part *os.File, stats []parStat) error {
	// 占一个该源的并发名额：最快的源同时被所有任务要，也会被打成最慢的源。
	// 名额满了就在这里等，等待不是源站故障，不记健康。
	if src.t.c != nil {
		release, aerr := g.sched.Acquire(ctx, src.t.c.ID)
		if aerr != nil {
			return aerr
		}
		defer release()
	}
	urls := []*url.URL{src.t.u}
	if src.t.u.String() != plan.Upstream.String() {
		urls = append(urls, plan.Upstream)
	}
	var last error
	for _, u := range urls {
		// 只有分到的那台源才归因；回落到官方源不算官方源的功劳，也不算它的错。
		blame := u.String() == src.t.u.String()
		resp, err := g.rangeReq(ctx, plan, u, off, off+length-1)
		if err != nil {
			if blame && ctx.Err() == nil {
				atomic.AddInt32(&stats[idx].fail, 1)
			}
			last = err
			continue
		}
		start, tot, perr := parseContentRange(resp.Header.Get("Content-Range"))
		if resp.StatusCode != http.StatusPartialContent || perr != nil || start != off || tot != total {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if blame && ctx.Err() == nil {
				atomic.AddInt32(&stats[idx].fail, 1)
			}
			if blame {
				atomic.StoreInt32(&stats[idx].code, int32(resp.StatusCode))
			}
			last = fmt.Errorf("分段响应对不上：%s 返回 %d %q", u.Host, resp.StatusCode, resp.Header.Get("Content-Range"))
			continue
		}
		n, cerr := io.Copy(io.NewOffsetWriter(part, off), io.LimitReader(resp.Body, length))
		resp.Body.Close()
		if cerr != nil {
			if blame && ctx.Err() == nil {
				atomic.AddInt32(&stats[idx].fail, 1)
			}
			last = cerr
			continue
		}
		if n != length {
			if blame && ctx.Err() == nil {
				atomic.AddInt32(&stats[idx].fail, 1)
			}
			last = fmt.Errorf("%s 少给了 %d 字节", u.Host, length-n)
			continue
		}
		if blame {
			atomic.AddInt32(&stats[idx].ok, 1)
		}
		return nil
	}
	if last == nil {
		last = fmt.Errorf("没有可用源")
	}
	return last
}

// rangeReq 发一个带 Range 的上游请求。认证仍由 cli 里的 bearerTransport 按主机处理。
func (g *Gateway) rangeReq(ctx context.Context, plan *adapter.Plan, u *url.URL, from, to int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "mirror-lens/"+Version+" (+https://github.com/zggnh12345/mirror-lens)")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", from, to))
	for k, vs := range plan.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return g.cli.Do(req)
}

func parName(t fetchTarget) string {
	if t.c != nil {
		return t.c.Name
	}
	return "官方"
}

func parNames(srcs []parSource) []string {
	out := make([]string, 0, len(srcs))
	for _, s := range srcs {
		out = append(out, s.name)
	}
	return out
}

// ParallelStats 给面板用。
func (g *Gateway) ParallelStats() map[string]int64 {
	if g == nil {
		return nil
	}
	return map[string]int64{
		"fills":     g.parFills.Load(),
		"bytes":     g.parBytes.Load(),
		"fallbacks": g.parFalls.Load(),
	}
}
