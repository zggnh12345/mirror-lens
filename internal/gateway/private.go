package gateway

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// TrustedRelay 是显式信任的自建加速端。它能看到 PAT，所以只允许
// 出现在白名单里的地址；白名单由面板维护，热更新不用重启。
type TrustedRelay struct {
	ID      int64
	Name    string
	URL     string        // 含 {url} 则替换成原地址，否则按「relay + "/" + 原地址」拼接
	Timeout time.Duration // 保留字段：等响应头的超时由客户端统一控制
}

// PrivateOptions 控制带凭据的私有请求怎么走。
type PrivateOptions struct {
	BreakAfter  int           // 连续失败几次熔断，0 = 3
	BreakFor    time.Duration // 熔断多久，0 = 60s
	FallbackSem int           // 回退官方时最多几个文件同时下载，0 = 2
}

// SetTrusted 热更新白名单：面板增删后立刻生效，不用重启进程。
func (g *Gateway) SetTrusted(list []TrustedRelay) {
	g.trusted.Store(list)
}

func (g *Gateway) trustedList() []TrustedRelay {
	if v, ok := g.trusted.Load().([]TrustedRelay); ok {
		return v
	}
	return nil
}

func (g *Gateway) privBreakAfter() int {
	if g.priv.BreakAfter > 0 {
		return g.priv.BreakAfter
	}
	return 3
}

func (g *Gateway) privBreakFor() time.Duration {
	if g.priv.BreakFor > 0 {
		return g.priv.BreakFor
	}
	return time.Minute
}

func (g *Gateway) privBroken(name string) bool {
	g.privMu.Lock()
	defer g.privMu.Unlock()
	until, ok := g.privUntil[name]
	return ok && time.Now().Before(until)
}

func (g *Gateway) privOK(name string) {
	g.privMu.Lock()
	defer g.privMu.Unlock()
	delete(g.privFails, name)
	delete(g.privUntil, name)
}

func (g *Gateway) privFail(name, reason string) {
	g.privMu.Lock()
	if g.privFails == nil {
		g.privFails = make(map[string]int)
	}
	g.privFails[name]++
	n := g.privFails[name]
	broken := n >= g.privBreakAfter()
	if broken {
		if g.privUntil == nil {
			g.privUntil = make(map[string]time.Time)
		}
		g.privUntil[name] = time.Now().Add(g.privBreakFor())
		g.privFails[name] = 0
	}
	wait := g.privBreakFor()
	g.privMu.Unlock()

	log.Printf("信任中转 %s 失败(%d)：%s", name, n, reason)
	if broken {
		log.Printf("信任中转 %s 熔断 %s，期间私有请求直接回退官方", name, wait)
	}
}

// relayURL 按配置改写目标地址。两种形态都支持：
//   - https://relay/{url}  → https://relay/https://github.com/o/r/...
//   - https://relay/       → https://relay/https://github.com/o/r/...（前缀式）
func relayURL(tmpl, raw string) (string, error) {
	if strings.Contains(tmpl, "{url}") {
		return strings.ReplaceAll(tmpl, "{url}", raw), nil
	}
	return strings.TrimRight(tmpl, "/") + "/" + raw, nil
}

// tryTrusted 让白名单里所有活着的站同时发请求，第一个交出 200 响应头的
// 胜出，其余立刻取消——和公开路径的多源竞速是同一套语义，不排队。
// 每站一个独立 context：胜出时只取消输家，胜者正文读取不受影响。
func (g *Gateway) tryTrusted(w http.ResponseWriter, r *http.Request, plan *adapter.Plan) bool {
	list := g.trustedList()
	if len(list) == 0 || !plan.OfficialOnly || r.Header.Get("Range") != "" {
		return false
	}
	alive := make([]TrustedRelay, 0, len(list))
	for _, t := range list {
		if t.URL != "" && !g.privBroken(t.Name) {
			alive = append(alive, t)
		}
	}
	if len(alive) == 0 {
		return false
	}

	type result struct {
		idx  int
		t    TrustedRelay
		resp *http.Response
		err  error
	}
	cancels := make([]context.CancelFunc, len(alive))
	ch := make(chan result, len(alive))
	for i, t := range alive {
		ctx, cancel := context.WithCancel(r.Context())
		cancels[i] = cancel
		go func(i int, t TrustedRelay, ctx context.Context) {
			resp, err := g.doTrusted(ctx, plan, t)
			if err != nil && resp != nil {
				// 没交付出去就失败：body 归本 goroutine 关闭。
				resp.Body.Close()
				resp = nil
			}
			// 通道容量等于中转数，交付不会阻塞，也不存在「交付失败」，
			// 所以交出去的 body 一律由读的一方负责关闭。每个 goroutine
			// 因此只发一次就退出，不需要 WaitGroup 兜底。
			ch <- result{idx: i, t: t, resp: resp, err: err}
		}(i, t, ctx)
	}

	var denied *http.Response
	var deniedIdx int
	var deniedT TrustedRelay
	stopOthers := func(keep int) {
		for j, c := range cancels {
			if j != keep {
				c()
			}
		}
	}
	for i := 0; i < len(alive); i++ {
		res := <-ch
		if res.err != nil {
			g.privFail(res.t.Name, res.err.Error())
			continue
		}
		code := res.resp.StatusCode
		switch {
		case code == http.StatusUnauthorized || code == http.StatusForbidden:
			// 凭据本身有问题：所有站结论一样。先留着，看有没有能成功的。
			if denied == nil {
				denied, deniedIdx, deniedT = res.resp, res.idx, res.t
			} else {
				res.resp.Body.Close()
			}
		case code == http.StatusOK:
			g.privOK(res.t.Name)
			stopOthers(res.idx)
			// 剩下的结果读完就关；它们已被取消，很快返回。
			if left := len(alive) - i - 1; left > 0 {
				go func(n int) {
					for j := 0; j < n; j++ {
						if rr := <-ch; rr.resp != nil {
							rr.resp.Body.Close()
						}
					}
				}(left)
			}
			// 胜者的 cancel 由调用方持有，正文读写结束后才释放。
			defer cancels[res.idx]()
			g.serveRelay(r.Context(), w, res.resp, res.t, plan)
			return true
		default:
			// 429 / 408 / 5xx / 404 / 204 / 3xx … 一律不算下载成功。
			g.privFail(res.t.Name, fmt.Sprintf("HTTP %d", code))
			res.resp.Body.Close()
		}
	}

	if denied != nil {
		stopOthers(deniedIdx)
		defer cancels[deniedIdx]()
		g.privOK(deniedT.Name)
		g.serveRelay(r.Context(), w, denied, deniedT, plan)
		return true
	}
	return false
}

func (g *Gateway) doTrusted(ctx context.Context, plan *adapter.Plan, t TrustedRelay) (*http.Response, error) {
	target, err := relayURL(t.URL, plan.Upstream.String())
	if err != nil {
		return nil, err
	}
	method := plan.Method
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, err
	}
	// 原样转发上游头，其中包括 Authorization: token <PAT>。
	for k, vs := range plan.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return g.privClient.Do(req)
}

// newPrivClient 只给白名单中转用：等响应头 8s，正文不设总时长，
// 否则大文件下到一半会被掐断。
func newPrivClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 8 * time.Second,
		ExpectContinueTimeout: time.Second,
	}}
}

// serveRelay 转发白名单中转的响应。中转半路断开时按已写进度用 Range 续传：
// 响应头已经把 Content-Length 报给客户端了，凑不满只能 RST，客户端看到的就是
// curl 18 / HTTP2 stream reset。
func (g *Gateway) serveRelay(ctx context.Context, w http.ResponseWriter, resp *http.Response, t TrustedRelay, plan *adapter.Plan) {
	defer resp.Body.Close()
	var sent int64
	// 中转转发也是实打实出网了，记进流量账；不然面板上这块是空白。
	defer func() {
		if sent > 0 {
			g.st.RecordTraffic(store.TrafficBypass, store.KindOfKey(plan.CacheKey), sent)
		}
	}()
	// 和公开请求一样登记成任务，面板才能看到进度、速度和实际用的中转名。
	// 只有 200 才算一次下载；401 之类透传不占位置。
	var task *liveTask
	if resp.StatusCode == http.StatusOK {
		task = g.tasks.start(plan.Kind, planLabel(plan), "relay", resp.ContentLength, []string{t.Name})
	}
	ok := false
	defer func() { task.finish(ok) }()
	if task != nil {
		w = &taskWriter{ResponseWriter: w, t: task}
	}
	h := w.Header()
	copyHeader(h, resp.Header, "Content-Type", "Content-Length", "Content-Disposition",
		"Accept-Ranges", "ETag", "Last-Modified")
	h.Set("X-ML-Cache", "BYPASS")
	h.Set("X-ML-Relay", t.Name)
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	total := resp.ContentLength
	if resp.StatusCode != http.StatusOK {
		sent, _ = io.Copy(w, resp.Body)
		return
	}
	if total <= 0 {
		// 长度未知：只有正常读到 EOF 才算完整，读错误不能当成功。
		n, err := io.Copy(w, resp.Body)
		sent = n
		ok = err == nil && ctx.Err() == nil
		return
	}

	etag := strongETag(resp.Header)
	buf := make([]byte, 256<<10)
	var off int64
	for off < total {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			// 异常中转多发也不许越过声明长度。
			if off+int64(n) > total {
				n = int(total - off)
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
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
	sent = off
	if off >= total {
		ok = true
		return
	}

	// 没有强 ETag 就没有任何办法证明续传前后是同一份内容；宁可不拼。
	if etag == "" {
		log.Printf("信任中转 %s 断在 %d/%d，响应无强 ETag，放弃续传", t.Name, off, total)
		return
	}
	log.Printf("信任中转 %s 断在 %d/%d，开始续传", t.Name, off, total)
	done := g.resumeRelay(ctx, w, t, plan, off, total, etag, flusher)
	sent = done
	if done >= total {
		ok = true
		log.Printf("信任中转 %s 续传补齐 %d 字节，完成", t.Name, done-off)
	} else {
		log.Printf("信任中转 %s 续传未完成：还差 %d 字节", t.Name, total-done)
	}
}

// resumeRelay 从 off 开始向同一个中转要 Range。只接受 206，并且必须能证明
// 续的是同一份字节：Content-Range 的起止/总长要对、Content-Length 要与区间
// 一致、强 ETag 要原样返回。中转可以只给一段（en < total-1），读完这段再按
// 新偏移请求下一段；每次请求都重新做上面这套校验。
// 整个续传共享 15 分钟总预算，且跟随客户端取消。
func (g *Gateway) resumeRelay(parent context.Context, w io.Writer, t TrustedRelay, plan *adapter.Plan, off, total int64, etag string, flusher http.Flusher) int64 {
	ctx, cancel := context.WithTimeout(parent, 15*time.Minute)
	defer cancel()
	fails := 0
	for off < total && fails < 3 && ctx.Err() == nil {
		target, err := relayURL(t.URL, plan.Upstream.String())
		if err != nil {
			return off
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return off
		}
		for k, vs := range plan.Header {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", off))
		req.Header.Set("If-Range", etag)
		resp, err := g.privClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return off
			}
			fails++
			continue
		}
		if resp.StatusCode != http.StatusPartialContent {
			// 200 = 中转忽略了 Range，不能拿来当续传；其余状态同样不接受。
			resp.Body.Close()
			return off
		}
		st, en, tot, okRange := parseFullCR(resp.Header.Get("Content-Range"))
		if !okRange || st != off || tot != total || en >= total {
			resp.Body.Close()
			return off
		}
		// 首段响应用的是强 ETag，这里必须原样返回同一个：缺失、弱 ETag、
		// 变成了别的版本都拒绝。If-Range 只是请求，不能当服务器已证明。
		if resp.Header.Get("ETag") != etag {
			resp.Body.Close()
			return off
		}
		want := en - st + 1
		// 声明了长度就必须与 Content-Range 区间一致，否则这个 206 本身
		// 是坏的，不能拿它的字节去拼。
		if resp.ContentLength >= 0 && resp.ContentLength != want {
			resp.Body.Close()
			return off
		}
		buf := make([]byte, 256<<10)
		var got int64
		for got < want {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				if got+int64(n) > want {
					n = int(want - got)
				}
				if _, werr := w.Write(buf[:n]); werr != nil {
					resp.Body.Close()
					return off
				}
				got += int64(n)
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
		// 段没读满：已写出的字节仍是同一实体（ETag / Content-Range 已校验），
		// 保留并按新的 off 继续；只有一点没推进才算一次失败。
		if got > 0 {
			fails = 0
		} else {
			fails++
		}
	}
	return off
}

// strongETag 只认强校验器：W/ 开头的是弱 ETag，不能证明字节一致。
func strongETag(h http.Header) string {
	v := h.Get("ETag")
	if v == "" || strings.HasPrefix(v, "W/") || !strings.HasPrefix(v, `"`) {
		return ""
	}
	return v
}

// parseFullCR 严格解析 "bytes 100-199/2000"：起点、终点、总长缺一不可，
// 且必须自洽。续传只认这个结果，解析不出来就不拼。
func parseFullCR(v string) (start, end, total int64, ok bool) {
	if !strings.HasPrefix(v, "bytes ") {
		return 0, 0, 0, false
	}
	v = strings.TrimSpace(strings.TrimPrefix(v, "bytes "))
	slash := strings.IndexByte(v, '/')
	dash := strings.IndexByte(v, '-')
	if slash < 0 || dash < 0 || dash > slash {
		return 0, 0, 0, false
	}
	start, e1 := strconv.ParseInt(v[:dash], 10, 64)
	end, e2 := strconv.ParseInt(v[dash+1:slash], 10, 64)
	total, e3 := strconv.ParseInt(v[slash+1:], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil || total <= 0 || start < 0 || end < start || end >= total {
		return 0, 0, 0, false
	}
	return start, end, total, true
}
