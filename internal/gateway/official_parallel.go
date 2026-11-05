package gateway

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
)

// 私有内容只能打官方（带着 PAT，不能外发给镜像站），但官方单连接
// 往往跑不满带宽。这里对同一个官方源开多个 Range 并发下载，按序从
// 内存窗口发给客户端：不落盘、不缓存，窗口上限 = workers × chunk。
//
// 和 tryParallel 的区别：那边靠「多源 + 可信摘要」防拼错；这里只有一个源，
// 版本天然一致，不需要摘要，代价是内存里同时最多压 workers 段。
func (g *Gateway) tryOfficialParallel(w http.ResponseWriter, r *http.Request, plan *adapter.Plan) bool {
	if !g.par.Enabled || g.par.Workers < 2 || g.par.ChunkSize <= 0 {
		return false
	}
	if !plan.OfficialOnly || !plan.Stream || r.Header.Get("Range") != "" {
		return false
	}
	// 回退路径要限并发：中转全挂时，N 个客户端同时各开 4 路会把官方打限流。
	if g.privSem != nil {
		select {
		case g.privSem <- struct{}{}:
			defer func() { <-g.privSem }()
		case <-r.Context().Done():
			return false
		}
	}
	ctx := r.Context()

	// 1 字节预检：确认支持 Range、拿到总长。还没写任何字节，失败可直接退回单流。
	resp, err := g.rangeReq(ctx, plan, plan.Upstream, 0, 0)
	if err != nil {
		return false
	}
	start, total, perr := parseContentRange(resp.Header.Get("Content-Range"))
	ctype := resp.Header.Get("Content-Type")
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || perr != nil || start != 0 || total <= 0 {
		return false
	}
	if total < g.par.MinSize || total > g.limitFor(plan) {
		g.decline(fmt.Sprintf("私有文件 %d 字节不在分片区间", total))
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
	for n > 256 {
		chunk *= 2
		n = int((total + chunk - 1) / chunk)
	}
	workers := g.par.Workers
	if workers > n {
		workers = n
	}

	type segResult struct {
		b   []byte
		err error
	}
	segs := make([]chan segResult, n)
	for i := range segs {
		segs[i] = make(chan segResult, 1)
	}
	// 信号量同时限制「在途段数」和「内存峰值」：输出端读走一段才还一个名额。
	sem := make(chan struct{}, workers)
	var next atomic.Int64
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(ctx)
	// cancel 必须先于 Wait：否则 worker 卡在 sem 上等名额，永远等不到取消。
	defer func() {
		cancel()
		wg.Wait()
	}()

	for k := 0; k < workers; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= n || ctx.Err() != nil {
					return
				}
				off := int64(i) * chunk
				ln := chunk
				if off+ln > total {
					ln = total - off
				}
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				b, err := g.fetchSeg(ctx, plan, off, ln)
				if err != nil {
					// 释放名额再报错，避免输出端在 <-sem 上卡死。
					<-sem
					segs[i] <- segResult{err: err}
					return
				}
				segs[i] <- segResult{b: b}
			}
		}()
	}

	// 先拿到第一段再发响应头：这一段失败时一个字节都没写出去，可以无损退回
	// 单流；后面的段万一失败，就从断点用单流补齐——声明了 Content-Length 就
	// 必须凑够，半路把客户端掐断会变成 curl 18 / http2 RST。
	first := <-segs[0]
	<-sem
	if first.err != nil {
		g.decline(fmt.Sprintf("私有文件首段失败，退回单流: %v", first.err))
		return false
	}

	h := w.Header()
	if ctype != "" {
		h.Set("Content-Type", ctype)
	}
	copyHeader(h, resp.Header, "Content-Disposition", "Content-Language")
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Length", strconv.FormatInt(total, 10))
	h.Set("X-ML-Cache", "BYPASS")
	h.Set("X-ML-Parallel", fmt.Sprintf("%d 段 / 1 源 / %d 并发", n, workers))
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	write := func(b []byte) bool {
		if _, err := w.Write(b); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}

	g.parFills.Add(1)
	g.parBytes.Add(total)
	if !write(first.b) {
		return true
	}
	for i := 1; i < n; i++ {
		s := <-segs[i]
		<-sem
		if s.err != nil {
			off := int64(i) * chunk
			log.Printf("官方并行：第 %d/%d 段失败，转单流补齐剩余 %d 字节: %v", i+1, n, total-off, s.err)
			cancel()
			// 用不带取消的 ctx：其余分片 worker 已经停了，补齐不能再被它们带走。
			if err := g.copyFrom(context.WithoutCancel(r.Context()), w, plan, off, total, flusher); err != nil {
				log.Printf("官方并行：单流补齐未完成（已发到 %d/%d）: %v", off, total, err)
			}
			return true
		}
		if !write(s.b) {
			return true
		}
	}
	return true
}

// copyFrom 从 off 开始用单连接把剩余字节补齐（分段失败后的兜底）。
// 中途再断就按已写进度续，最多试 3 次。
func (g *Gateway) copyFrom(ctx context.Context, w io.Writer, plan *adapter.Plan, off, total int64, flusher http.Flusher) error {
	backoff := 300 * time.Millisecond
	for attempt := 0; attempt < 3 && off < total; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
			backoff *= 2
		}
		resp, err := g.rangeReq(ctx, plan, plan.Upstream, off, total-1)
		if err != nil {
			continue
		}
		if resp.StatusCode != http.StatusPartialContent {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			continue
		}
		buf := make([]byte, 256<<10)
		for off < total {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					resp.Body.Close()
					return werr
				}
				off += int64(n)
				if flusher != nil {
					flusher.Flush()
				}
			}
			if rerr != nil {
				break
			}
		}
		resp.Body.Close()
	}
	if off < total {
		return fmt.Errorf("还差 %d 字节", total-off)
	}
	return nil
}

// fetchSeg 取一段到内存；失败按指数退避重试（同一个源，换连接重来）。
// 官方对并发 Range 偶发截断/重置，只试一次很容易把整条流打断。
func (g *Gateway) fetchSeg(ctx context.Context, plan *adapter.Plan, off, ln int64) ([]byte, error) {
	var last error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * 200 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		resp, err := g.rangeReq(ctx, plan, plan.Upstream, off, off+ln-1)
		if err != nil {
			last = err
			continue
		}
		start, _, perr := parseContentRange(resp.Header.Get("Content-Range"))
		if resp.StatusCode != http.StatusPartialContent || perr != nil || start != off {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			last = fmt.Errorf("分段响应异常：HTTP %d，Content-Range=%q", resp.StatusCode, resp.Header.Get("Content-Range"))
			continue
		}
		buf := make([]byte, ln)
		n, rerr := io.ReadFull(resp.Body, buf)
		resp.Body.Close()
		if rerr != nil || int64(n) != ln {
			last = fmt.Errorf("分段长度不符：%d/%d", n, ln)
			continue
		}
		return buf, nil
	}
	return nil, last
}
