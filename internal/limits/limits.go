// Package limits 是入口限流：全局一层，客户端一层（匿名按 IP，带 Token 再加一道）。
//
// 只挡 HTTP 入口，不碰调度器：被限流是「让他慢点」，不是「源站坏了」，
// 所以限流和并发繁忙永远不写进源站健康。
//
// 大文件按「一个请求」算：速率数的是客户端请求，不是服务端拆的分片；
// 并发名额从进入处理函数占到响应写完，大下载与小请求等价。
package limits

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/metrics"
)

// Config 是限流参数：0 值取 Default，Enabled=false 时整层关闭。
type Config struct {
	Enabled bool `yaml:"enabled"`

	// 全局：所有客户端合计，防总量打满出口带宽和磁盘。
	GlobalRPS         float64 `yaml:"global_rps"`
	GlobalBurst       int     `yaml:"global_burst"`
	GlobalConcurrency int     `yaml:"global_concurrency"`

	// 匿名：按真实客户端 IP；一次 docker pull 几十个层，压不到。
	IPPerMinute   int `yaml:"ip_per_minute"`
	IPBurst       int `yaml:"ip_burst"`
	IPConcurrency int `yaml:"ip_concurrency"`

	// Token 客户端：比匿名宽松，但仍有上限。
	TokenPerMinute   int `yaml:"token_per_minute"`
	TokenBurst       int `yaml:"token_burst"`
	TokenConcurrency int `yaml:"token_concurrency"`

	// TrustedProxies 是可信反代网段（CIDR），默认空即不信任何转发头：
	// X-Forwarded-For 谁都能写，信了等于把限流键交给客户端。
	// 如 127.0.0.1/32、::1/128、10.0.0.0/8。
	TrustedProxies []string `yaml:"trusted_proxies"`
}

// Default 够一个人正常 pull / 下 release，又能挡住脚本刷量。
func Default() Config {
	return Config{
		Enabled:           true,
		GlobalRPS:         20,
		GlobalBurst:       100,
		GlobalConcurrency: 200,
		IPPerMinute:       300,
		IPBurst:           60,
		IPConcurrency:     8,
		TokenPerMinute:    1200,
		TokenBurst:        200,
		TokenConcurrency:  32,
	}
}

// Limiter 持有所有计数器，零值不可用，用 New。
type Limiter struct {
	cfg   Config
	nets  []*net.IPNet
	mu    sync.Mutex
	buck  map[string]*bucket
	conc  map[string]int
	glob  int
	hits  map[string]int64 // 各层拒绝次数（面板/指标用）
	sweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
	rate   float64 // 每秒补充
	burst  float64
	seen   time.Time
}

func New(cfg Config) *Limiter {
	d := Default()
	if !cfg.Enabled {
		// 显式 false 保持关闭；其余字段仍填默认值便于日志展示。
		cfg.Enabled = false
	} else {
		cfg.Enabled = true
	}
	if cfg.GlobalRPS <= 0 {
		cfg.GlobalRPS = d.GlobalRPS
	}
	if cfg.GlobalBurst <= 0 {
		cfg.GlobalBurst = d.GlobalBurst
	}
	if cfg.GlobalConcurrency <= 0 {
		cfg.GlobalConcurrency = d.GlobalConcurrency
	}
	if cfg.IPPerMinute <= 0 {
		cfg.IPPerMinute = d.IPPerMinute
	}
	if cfg.IPBurst <= 0 {
		cfg.IPBurst = d.IPBurst
	}
	if cfg.IPConcurrency <= 0 {
		cfg.IPConcurrency = d.IPConcurrency
	}
	if cfg.TokenPerMinute <= 0 {
		cfg.TokenPerMinute = d.TokenPerMinute
	}
	if cfg.TokenBurst <= 0 {
		cfg.TokenBurst = d.TokenBurst
	}
	if cfg.TokenConcurrency <= 0 {
		cfg.TokenConcurrency = d.TokenConcurrency
	}
	l := &Limiter{
		cfg:  cfg,
		buck: map[string]*bucket{},
		conc: map[string]int{},
		hits: map[string]int64{},
	}
	for _, c := range cfg.TrustedProxies {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(c); err == nil {
			l.nets = append(l.nets, n)
			continue
		}
		// 允许写单个地址，省得算 /32。
		if ip := net.ParseIP(c); ip != nil {
			bits := 128
			if ip.To4() != nil {
				bits = 32
				ip = ip.To4()
			}
			l.nets = append(l.nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
		}
	}
	return l
}

// Enabled 供启动日志与测试判断。
func (l *Limiter) Enabled() bool { return l != nil && l.cfg.Enabled }

// Hits 返回各层累计拒绝次数（global/ip/token）。
func (l *Limiter) Hits() map[string]int64 {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]int64, len(l.hits))
	for k, v := range l.hits {
		out[k] = v
	}
	return out
}

// Wrap 包住整个 mux。/healthz 不受限：探活被 429 会让容器显示不健康，
// 那是自己给自己造故障。
func (l *Limiter) Wrap(next http.Handler) http.Handler {
	if l == nil || !l.cfg.Enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		now := time.Now()

		// 1. 全局速率：先挡总量，最便宜。
		if !l.take("g:", l.cfg.GlobalRPS, float64(l.cfg.GlobalBurst), now) {
			l.reject(w, "global", l.cfg.GlobalRPS)
			return
		}
		// 2. 全局并发：传输中的大文件也一直占名额。
		if !l.enter("g:", l.cfg.GlobalConcurrency) {
			l.reject(w, "global", 0)
			return
		}
		defer l.leave("g:")

		ip := l.ClientIP(r)
		// 3. 客户端速率：匿名按 IP。
		if !l.take("ip:"+ip, float64(l.cfg.IPPerMinute)/60, float64(l.cfg.IPBurst), now) {
			l.reject(w, "ip", float64(l.cfg.IPPerMinute)/60)
			return
		}
		// 4. 客户端并发：名额占到响应写完。
		if !l.enter("ip:"+ip, l.cfg.IPConcurrency) {
			l.reject(w, "ip", 0)
			return
		}
		defer l.leave("ip:" + ip)

		// 5. 有凭据再加 Token 层。用哈希做键，不把凭据留在内存；
		//    伪造 Token 能换键，但 IP 层仍拦着。
		if tok := credential(r); tok != "" {
			sum := sha256.Sum256([]byte(tok))
			k := "tk:" + hex.EncodeToString(sum[:8])
			if !l.take(k, float64(l.cfg.TokenPerMinute)/60, float64(l.cfg.TokenBurst), now) {
				l.reject(w, "token", float64(l.cfg.TokenPerMinute)/60)
				return
			}
			if !l.enter(k, l.cfg.TokenConcurrency) {
				l.reject(w, "token", 0)
				return
			}
			defer l.leave(k)
		}

		next.ServeHTTP(w, r)
	})
}

// ClientIP 返回限流用的客户端地址：默认取 TCP 对端，
// 对端可信时才读转发头，并从右往左取第一个非可信地址。
// 取最左等于让攻击者每次换个假 IP 绕过限流。
func (l *Limiter) ClientIP(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return host
	}
	if !l.trusted(ip) {
		return ip.String()
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			c := net.ParseIP(strings.TrimSpace(parts[i]))
			if c == nil {
				continue
			}
			if l.trusted(c) {
				continue
			}
			return c.String()
		}
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		if c := net.ParseIP(xr); c != nil && !l.trusted(c) {
			return c.String()
		}
	}
	return ip.String()
}

func (l *Limiter) trusted(ip net.IP) bool {
	for _, n := range l.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// take 取一个令牌，返回是否放行。
func (l *Limiter) take(key string, rate, burst float64, now time.Time) bool {
	if rate <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buck[key]
	if b == nil {
		b = &bucket{tokens: burst, last: now, rate: rate, burst: burst}
		l.buck[key] = b
	} else {
		b.tokens += now.Sub(b.last).Seconds() * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.last = now
	}
	b.seen = now
	l.maybeSweep(now)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// enter 占一个并发名额。
func (l *Limiter) enter(key string, max int) bool {
	if max <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conc[key] >= max {
		return false
	}
	l.conc[key]++
	return true
}

func (l *Limiter) leave(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conc[key] > 0 {
		l.conc[key]--
	}
	if l.conc[key] == 0 {
		delete(l.conc, key)
	}
}

// reject 回 429；Retry-After 按令牌补充速度算，客户端照着重试即可。
func (l *Limiter) reject(w http.ResponseWriter, layer string, rate float64) {
	secs := 1
	if rate > 0 {
		secs = int(1 / rate)
		if secs < 1 {
			secs = 1
		}
	}
	if secs > 60 {
		secs = 60
	}
	l.mu.Lock()
	l.hits[layer]++
	l.mu.Unlock()
	metrics.RateLimited(layer)

	w.Header().Set("Retry-After", itoa(secs))
	w.Header().Set("X-ML-Limit", layer)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":       "请求过于频繁，稍后重试",
		"limit":       layer,
		"retry_after": secs,
	})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// maybeSweep 清掉久未活动的键：公网会被无数 IP 扫，map 不能只进不出。
// 不用后台 goroutine，免得测试留下跑不完的循环。
func (l *Limiter) maybeSweep(now time.Time) {
	if now.Sub(l.sweep) < time.Minute {
		return
	}
	l.sweep = now
	cut := now.Add(-10 * time.Minute)
	for k, b := range l.buck {
		if b.seen.Before(cut) && l.conc[k] == 0 {
			delete(l.buck, k)
		}
	}
}

// credential 取凭据只用于限流键，不校验有效性。
func credential(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	if i := strings.IndexByte(h, ' '); i > 0 {
		return h[i+1:]
	}
	return h
}
