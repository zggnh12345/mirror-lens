package gateway

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
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

// tryTrusted 让白名单里所有活着的站同时发请求，第一个交出可用响应头的
// 胜出，其余立刻取消——和公开路径的多源竞速是同一套语义，不排队。
// 一个都没成功就返回 false，由调用方回退「官方多路并行」。
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

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	type result struct {
		t    TrustedRelay
		resp *http.Response
		err  error
	}
	ch := make(chan result, len(alive))
	for _, t := range alive {
		go func(t TrustedRelay) {
			resp, err := g.doTrusted(ctx, plan, t)
			select {
			case ch <- result{t: t, resp: resp, err: err}:
			case <-ctx.Done():
				if resp != nil {
					resp.Body.Close()
				}
			}
		}(t)
	}

	var denied *http.Response
	var deniedName string
	for i := 0; i < len(alive); i++ {
		res := <-ch
		if res.err != nil {
			g.privFail(res.t.Name, res.err.Error())
			continue
		}
		switch {
		case res.resp.StatusCode == http.StatusUnauthorized || res.resp.StatusCode == http.StatusForbidden:
			// 凭据本身有问题：所有站结论一样。先留着，看有没有能成功的。
			if denied == nil {
				denied, deniedName = res.resp, res.t.Name
			} else {
				res.resp.Body.Close()
			}
		case res.resp.StatusCode >= 500 || res.resp.StatusCode == http.StatusNotFound:
			g.privFail(res.t.Name, fmt.Sprintf("HTTP %d", res.resp.StatusCode))
			res.resp.Body.Close()
		default:
			g.privOK(res.t.Name)
			cancel()
			// 没赢的那些请求要关掉 body，否则连接一直挂着；放后台做，
			// 不挡当前响应。
			left := len(alive) - i - 1
			if left > 0 {
				go func(n int) {
					for j := 0; j < n; j++ {
						if rr := <-ch; rr.resp != nil {
							rr.resp.Body.Close()
						}
					}
				}(left)
			}
			g.serveRelay(w, res.resp, res.t.Name)
			return true
		}
	}

	if denied != nil {
		g.privOK(deniedName)
		g.serveRelay(w, denied, deniedName)
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

func (g *Gateway) serveRelay(w http.ResponseWriter, resp *http.Response, name string) {
	defer resp.Body.Close()
	h := w.Header()
	copyHeader(h, resp.Header, "Content-Type", "Content-Length", "Content-Disposition",
		"Accept-Ranges", "ETag", "Last-Modified")
	h.Set("X-ML-Cache", "BYPASS")
	h.Set("X-ML-Relay", name)
	w.WriteHeader(resp.StatusCode)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	io.Copy(w, resp.Body)
}
