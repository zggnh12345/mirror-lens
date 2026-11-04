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
// 出现在配置白名单里的地址；没配置就一个都不试，私有请求直连官方。
type TrustedRelay struct {
	Name    string
	URL     string        // 含 {url} 则替换成原地址，否则按「relay + "/" + 原地址」拼接
	Timeout time.Duration // 只用于「拿到响应头」这一段，0 = 8s
}

// PrivateOptions 控制带凭据的私有请求怎么走。
type PrivateOptions struct {
	Trusted     []TrustedRelay
	BreakAfter  int           // 连续失败几次熔断，0 = 3
	BreakFor    time.Duration // 熔断多久，0 = 60s
	FallbackSem int           // 回退官方时最多几个文件同时下载，0 = 2
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

// tryTrusted 依次试白名单里的自建加速端；任何一步失败都返回 false，
// 由调用方回退到「官方 4 路并行」。
func (g *Gateway) tryTrusted(w http.ResponseWriter, r *http.Request, plan *adapter.Plan) bool {
	if len(g.priv.Trusted) == 0 || !plan.OfficialOnly || r.Header.Get("Range") != "" {
		return false
	}
	for _, t := range g.priv.Trusted {
		if t.URL == "" || g.privBroken(t.Name) {
			continue
		}
		if g.serveTrusted(w, r, plan, t) {
			return true
		}
	}
	return false
}

func (g *Gateway) serveTrusted(w http.ResponseWriter, r *http.Request, plan *adapter.Plan, t TrustedRelay) bool {
	target, err := relayURL(t.URL, plan.Upstream.String())
	if err != nil {
		return false
	}
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, plan.Method, target, nil)
	if err != nil {
		return false
	}
	// 原样转发上游头，其中包括 Authorization: token <PAT>。
	for k, vs := range plan.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if a := r.Header.Get("Accept"); a != "" && req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", a)
	}

	resp, err := g.privClient.Do(req)
	if err != nil {
		g.privFail(t.Name, err.Error())
		return false
	}
	defer resp.Body.Close()

	// 凭据本身有问题：回退官方也是同样结果，直接把结论交回客户端。
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		g.privOK(t.Name)
		copyHeader(w.Header(), resp.Header, "Content-Type", "WWW-Authenticate")
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, io.LimitReader(resp.Body, 64<<10))
		return true
	}
	// 5xx 或 404 当成「这个中转没取到」：404 可能是它不支持私有路径。
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusNotFound {
		g.privFail(t.Name, fmt.Sprintf("HTTP %d", resp.StatusCode))
		return false
	}

	g.privOK(t.Name)
	h := w.Header()
	copyHeader(h, resp.Header, "Content-Type", "Content-Length", "Content-Disposition",
		"Accept-Ranges", "ETag", "Last-Modified")
	h.Set("X-ML-Cache", "BYPASS")
	h.Set("X-ML-Relay", t.Name)
	w.WriteHeader(resp.StatusCode)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	io.Copy(w, resp.Body)
	return true
}
