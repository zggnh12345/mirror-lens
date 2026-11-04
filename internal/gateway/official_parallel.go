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
	defer cancel()

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

	defer wg.Wait()
	g.parFills.Add(1)
	g.parBytes.Add(total)
	for i := 0; i < n; i++ {
		s := <-segs[i]
		<-sem
		if s.err != nil {
			// 已经发出去一部分，没法回头，只能断开让客户端重试。
			log.Printf("官方并行：第 %d/%d 段失败，断开: %v", i+1, n, s.err)
			return true
		}
		if _, werr := w.Write(s.b); werr != nil {
			return true
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	return true
}

// fetchSeg 取一段到内存；失败重试一次（同一个源，换连接重来）。
func (g *Gateway) fetchSeg(ctx context.Context, plan *adapter.Plan, off, ln int64) ([]byte, error) {
	var last error
	for attempt := 0; attempt < 2; attempt++ {
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
