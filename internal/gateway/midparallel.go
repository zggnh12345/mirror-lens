package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/logging"
	"github.com/zggnh12345/mirror-lens/internal/metrics"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// midChunks 把「还差 rest 字节」切成每段 chunk 的段表。
// 末尾不足一段的按实际长度算，返回段数。
func midChunks(rest, chunk int64) int {
	if chunk <= 0 || rest <= 0 {
		return 0
	}
	return int((rest + chunk - 1) / chunk)
}

// midFill 在单流已经发出去 sent 字节之后接入分片：从 sent 起用多台源
// 并行填剩余部分，边填边按顺序续给客户端，整份校验通过才入缓存。
//
// 返回 true 表示已经接手：无论后面成功还是失败，调用方都不该再动这条响应；
// 返回 false 表示还没写新字节，调用方可以继续原来的单流连接。
func (g *Gateway) midFill(out io.Writer, plan *adapter.Plan, part *os.File,
	sent, total int64, flusher http.Flusher, old *http.Response, killConn func()) (taken, ok bool) {
	// 不能异常终止连接的客户端（HTTP/2、writer 链里没有 Hijacker）不接管：
	// 边填边发之后校验失败却只能正常收尾，等于把损坏内容当完整响应交付。
	if killConn == nil {
		log.Printf("中途并行跳过：当前连接无法异常终止（HTTP/2 或不支持 Hijack）")
		return false, false
	}
	// 没有 .part 就没法拼装（超大文件已转透传），客户端自带 Range 也不掺和。
	if part == nil || total <= 0 || sent <= 0 || sent >= total {
		log.Printf("中途并行跳过：part=%v sent=%d total=%d", part != nil, sent, total)
		return false, false
	}
	if !g.par.Enabled || g.par.Workers < 2 || g.par.ChunkSize <= 0 {
		log.Printf("中途并行跳过：分片未启用 workers=%d chunk=%d", g.par.Workers, g.par.ChunkSize)
		return false, false
	}
	if plan.ExpectSHA == "" || !plan.Stream || !plan.Cacheable || g.lowDisk() {
		log.Printf("中途并行跳过：sha=%q stream=%v cacheable=%v lowdisk=%v",
			plan.ExpectSHA, plan.Stream, plan.Cacheable, g.lowDisk())
		return false, false
	}
	// 中途接入的前提是「已知整份长度」：不然段表落不了地。
	if plan.Upstream == nil {
		return false, false
	}

	if old == nil || old.Request == nil {
		return false, false
	}
	// requestCtx 只表示客户端请求本身；并行任务另用 taskCtx。
	// 两者必须分开：任务失败会 cancel(taskCtx)，若这里复用同一变量，
	// 下面的 defer 会把「我们主动中止」误判成「客户端已断开」而跳过断连。
	requestCtx := old.Request.Context()
	// wrote 一旦置位，响应就无法撤回：此后的任何失败都必须让连接异常结束，
	// 不能正常收尾，否则客户端会把损坏内容当成完整响应。
	var wrote, clientGone bool
	delivered := sent
	defer func() {
		if !wrote || ok || clientGone || requestCtx.Err() != nil {
			return
		}
		metrics.MidfillAborted()
		log.Printf("中途并行失败：已交付 %d/%d 字节，将中止连接", delivered, total)
		if killConn != nil {
			killConn()
		}
	}()
	targets := g.parallelTargetsN(plan, 20)
	if g.parTargets != nil {
		targets = g.parTargets(plan)
	}
	if len(targets) < 1 {
		log.Printf("中途并行跳过：没有候选源")
		return false, false
	}
	srcs, tot := g.parPreflight(requestCtx, plan, targets, 6*time.Second)
	// 总长必须和正在发的这份一致，否则拼起来必错。
	// 只剩一台也能接：对同一台开多连接分段，比单流干等强。
	if len(srcs) < 1 || (tot > 0 && tot != total) {
		log.Printf("中途并行：没有能分片的源（%d 台），继续单流", len(srcs))
		return false, false
	}
	if len(srcs) == 1 {
		log.Printf("中途并行：只有 %s 支持分片，单源多连接", srcs[0].name)
	}
	preferMirrors(srcs, plan.Upstream.String())

	rest := total - sent
	chunk := g.par.ChunkSize
	if chunk > rest/2 {
		chunk = rest / 2
	}
	if chunk < 256<<10 {
		chunk = 256 << 10
	}
	n := midChunks(rest, chunk)
	for n > 256 {
		chunk *= 2
		n = midChunks(rest, chunk)
	}
	if n <= 0 {
		return false, false
	}

	// 第 0 段直接从 sent 开始：拿到首块才算接手，之前失败都还能回单流。
	firstLen := chunk
	if n == 1 {
		firstLen = rest
	}
	// 同源接管不新开名额（Transfer），异源才 +1（handover，非阻塞）。
	hm := holderFrom(requestCtx)
	cur := hm.get()
	same := cur != nil && srcs[0].t.c != nil && cur.SourceID == srcs[0].t.c.ID
	var l0 *scheduler.Lease
	if !same {
		var lerr error
		l0, lerr = g.leaseFor(requestCtx, srcs[0].t, scheduler.KindHandover)
		if lerr != nil {
			log.Printf("中途并行：名额已满（%v），继续单流", lerr)
			return false, false
		}
	}

	resp0, err := g.rangeReq(requestCtx, plan, srcs[0].t.u, sent, sent+firstLen-1)
	if err != nil {
		releaseLease(l0)
		return false, false
	}
	if start, tot2, perr := parseContentRange(resp0.Header.Get("Content-Range")); resp0.StatusCode != http.StatusPartialContent ||
		perr != nil || start != sent || tot2 != total {
		resp0.Body.Close()
		releaseLease(l0)
		return false, false
	}
	if same {
		// 首块校验通过后再交接：失败还能继续单流，不丢名额。
		tok, terr := cur.Transfer(cur.Token())
		if terr != nil {
			resp0.Body.Close()
			return false, false
		}
		retire, aerr := cur.Activate()
		if aerr != nil {
			cur.Release(tok)
			resp0.Body.Close()
			return false, false
		}
		resp0.Body = newLeaseBody(resp0.Body, cur, tok, retire)
	} else if resp0, err = wrapLease(resp0, l0); err != nil {
		resp0.Body.Close()
		releaseLease(l0)
		return false, false
	} else {
		hm.set(l0)
	}
	defer resp0.Body.Close()

	start := time.Now()
	stats := make([]parStat, len(srcs))
	var wg sync.WaitGroup
	taskCtx, cancel := context.WithCancel(requestCtx)
	defer cancel()

	// 先派后面的段，第 0 段留给自己直通客户端。
	dones := make([]chan struct{}, n)
	for i := range dones {
		dones[i] = make(chan struct{})
	}
	var next atomic.Int64
	next.Store(1)
	busy := make([]atomic.Int32, len(srcs))
	// 第 0 段由主协程从 resp0 直读，也要占源 0 的并发名额；
	// 否则 workers 会把正在供数的源当成空闲源继续派活。
	busy[0].Add(1)
	defer busy[0].Add(-1)
	pick := func() int {
		best, bestBusy, bestFail := -1, int32(1<<30), int32(1<<30)
		for j := range srcs {
			if j > 0 && srcs[j].t.u.String() == plan.Upstream.String() {
				continue
			}
			bz, fl := busy[j].Load(), atomic.LoadInt32(&stats[j].fail)
			if best < 0 || bz < bestBusy || (bz == bestBusy && fl < bestFail) {
				best, bestBusy, bestFail = j, bz, fl
			}
		}
		if best < 0 {
			return 0
		}
		return best
	}
	abort := func() {
		cancel()
		wg.Wait()
		part.Close()
		g.cache.DropPartial(plan.CacheKey)
	}
	var done atomic.Bool
	defer func() {
		if done.Load() {
			return
		}
		// 走到这里说明中途失败：连接已经写了新字节，只能断开。
		abort()
	}()
	for wk := 0; wk < g.par.Workers; wk++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= n || taskCtx.Err() != nil {
					return
				}
				off := sent + int64(i)*chunk
				length := chunk
				if off+length > total {
					length = total - off
				}
				si := pick()
				busy[si].Add(1)
				err := g.parChunk(taskCtx, plan, srcs[si], srcs, si, off, length, total, part, stats, nil)
				busy[si].Add(-1)
				if err != nil {
					cancel() // 一段补不齐，整份就别再白下载了
					return
				}
				close(dones[i])
			}
		}()
	}

	log.Printf("中途并行：从 %d 字节处接入，%d 段 × %d 源（%s）", sent, n, len(srcs), parNames(srcs))

	// 第 0 段：边写客户端边落到 part 的 sent 偏移。
	pw := io.NewOffsetWriter(part, sent)
	var got int64
	buf := make([]byte, 64<<10)
	for got < firstLen {
		nr, rerr := resp0.Body.Read(buf)
		if nr > 0 {
			wn := int64(nr)
			if avail := firstLen - got; avail < wn {
				wn = avail
			}
			if _, werr := out.Write(buf[:wn]); werr != nil {
				clientGone = true // 客户端走了
				return true, false
			}
			delivered += wn
			if !wrote {
				wrote = true
				metrics.MidfillStarted()
			}
			if _, werr := pw.Write(buf[:wn]); werr != nil {
				return true, false
			}
			got += wn
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return true, false
		}
	}
	if got != firstLen {
		atomic.AddInt32(&stats[0].fail, 1)
		return true, false
	}
	atomic.AddInt32(&stats[0].ok, 1)

	// 其余段严格按顺序续给客户端：每段下完就立刻发，段和段之间不抢跑。
	for i := 1; i < n; i++ {
		select {
		case <-dones[i]:
		case <-taskCtx.Done():
			return true, false
		}
		off := sent + int64(i)*chunk
		length := chunk
		if off+length > total {
			length = total - off
		}
		nw, werr := io.CopyN(out, io.NewSectionReader(part, off, length), length)
		delivered += nw
		if werr != nil {
			return true, false
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	wg.Wait()
	if taskCtx.Err() != nil {
		return true, false
	}

	// 整份回读校验：前缀是单流写的，后半是分片写的，只有整份哈希能兜住。
	if _, err := part.Seek(0, io.SeekStart); err != nil {
		return true, false
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, part); err != nil {
		return true, false
	}
	sha := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(sha, plan.ExpectSHA) {
		g.verifyFail.Add(1)
		metrics.MidfillVerifyFailed()
		var failSrc *fetchTarget
		if len(srcs) == 1 { // 单源失败可归因；多源混跑不猜是哪台写坏的
			failSrc = &srcs[0].t
		}
		g.recordIntegrityFailure(plan, sha, failSrc)
		log.Printf("中途并行：整份校验失败，不入缓存：期望 %s 实得 %s", plan.ExpectSHA, sha)
		return true, false
	}

	part.Close()
	done.Store(true)
	partPath, _ := g.cache.PartialPaths(plan.CacheKey)
	unlock := g.cache.LockBlob(sha)
	if _, err := g.cache.Commit(partPath, sha); err != nil {
		unlock()
		g.cache.DropPartial(plan.CacheKey)
		log.Printf("中途并行：落盘失败: %v", logging.SafeErr(err))
		return true, false
	}
	e := store.CacheEntry{
		Key:          plan.CacheKey,
		SHA256:       sha,
		Size:         total,
		ContentType:  srcs[0].ctype,
		ETag:         old.Header.Get("ETag"),
		LastModified: old.Header.Get("Last-Modified"),
		Upstream:     plan.Upstream.String(),
		CreatedAt:    g.now().Unix(),
		TTL:          int64(plan.TTL.Seconds()),
		Verify:       store.VerifyVerified,
	}
	err = g.st.CachePut(e)
	unlock()
	if err != nil {
		log.Printf("中途并行：写索引失败 %s: %v", logging.SanitizeKey(plan.CacheKey), logging.SafeErr(err))
	} else {
		g.recordBlobProvenance(e.SHA256, plan)
	}
	g.cache.DropPartial(plan.CacheKey)
	g.st.RecordTraffic(store.TrafficFill, store.KindOfKey(plan.CacheKey), total)

	for i := range srcs {
		switch {
		case atomic.LoadInt32(&stats[i].fail) > 0:
			g.reportSource(plan.Kind, srcs[i].t, false, time.Since(start),
				scheduler.Hint{Code: int(atomic.LoadInt32(&stats[i].code)), Mode: "midparallel"})
		case atomic.LoadInt32(&stats[i].ok) > 0:
			g.reportSource(plan.Kind, srcs[i].t, true, time.Since(start),
				scheduler.Hint{Code: http.StatusPartialContent, Mode: "midparallel"})
		}
	}
	log.Printf("中途并行完成：%s %d 字节（%d 起），%d 段 × %d 源，用时 %s",
		logging.SanitizeKey(plan.CacheKey), total, sent, n, len(srcs), time.Since(start).Round(time.Millisecond))
	return true, true
}
