package limits

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
}

// 测试用的宽松全局层：只考察客户端那一层。
func cfgForIP(perMin, burst, conc int) Config {
	c := Default()
	c.GlobalRPS = 10000
	c.GlobalBurst = 10000
	c.GlobalConcurrency = 10000
	c.IPPerMinute = perMin
	c.IPBurst = burst
	c.IPConcurrency = conc
	return c
}

func req(remote, path string, hdr map[string]string) *http.Request {
	r := httptest.NewRequest("GET", path, nil)
	r.RemoteAddr = remote
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

func TestPerIPBurstThen429(t *testing.T) {
	l := New(cfgForIP(6000, 2, 10)) // 100/s，突发 2
	h := l.Wrap(okHandler())
	var last *httptest.ResponseRecorder
	for i := 0; i < 3; i++ {
		last = httptest.NewRecorder()
		h.ServeHTTP(last, req("1.2.3.4:5555", "/v2/", nil))
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("第三次应被限流，得到 %d", last.Code)
	}
	if got := last.Header().Get("X-ML-Limit"); got != "ip" {
		t.Fatalf("应标出 ip 层，得到 %q", got)
	}
	ra, err := strconv.Atoi(last.Header().Get("Retry-After"))
	if err != nil || ra < 1 || ra > 60 {
		t.Fatalf("Retry-After 不合理: %q", last.Header().Get("Retry-After"))
	}
	if n := l.Hits()["ip"]; n != 1 {
		t.Fatalf("拒绝计数应为 1，得到 %d", n)
	}
}

// 不同 IP 各自一份配额，不该互相牵连。
func TestPerIPIndependent(t *testing.T) {
	l := New(cfgForIP(6000, 1, 10))
	h := l.Wrap(okHandler())
	first := httptest.NewRecorder()
	h.ServeHTTP(first, req("1.1.1.1:1000", "/", nil))
	second := httptest.NewRecorder()
	h.ServeHTTP(second, req("2.2.2.2:1000", "/", nil))
	if first.Code != 200 || second.Code != 200 {
		t.Fatalf("两个不同 IP 都该放行：%d %d", first.Code, second.Code)
	}
}

// 没配可信代理时，X-Forwarded-For 一律不看——否则谁都能换着写绕过限流。
func TestForwardedHeaderIgnoredWithoutTrustedProxy(t *testing.T) {
	l := New(cfgForIP(6000, 1, 10))
	h := l.Wrap(okHandler())
	ok := httptest.NewRecorder()
	h.ServeHTTP(ok, req("1.2.3.4:5555", "/", map[string]string{"X-Forwarded-For": "9.9.9.9"}))
	blocked := httptest.NewRecorder()
	h.ServeHTTP(blocked, req("1.2.3.4:5555", "/", map[string]string{"X-Forwarded-For": "8.8.8.8"}))
	if blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("同一个对端换个 XFF 不该换到新配额，得到 %d", blocked.Code)
	}
}

// 配了可信反代：按 XFF 里的真实客户端分别计数。
func TestForwardedHeaderHonoredBehindTrustedProxy(t *testing.T) {
	c := cfgForIP(6000, 1, 10)
	c.TrustedProxies = []string{"127.0.0.1/32"}
	l := New(c)
	h := l.Wrap(okHandler())
	a := httptest.NewRecorder()
	h.ServeHTTP(a, req("127.0.0.1:5555", "/", map[string]string{"X-Forwarded-For": "9.9.9.9"}))
	b := httptest.NewRecorder()
	h.ServeHTTP(b, req("127.0.0.1:5555", "/", map[string]string{"X-Forwarded-For": "8.8.8.8"}))
	if a.Code != 200 || b.Code != 200 {
		t.Fatalf("两个客户端该各自放行：%d %d", a.Code, b.Code)
	}
}

// 客户端自己往左边塞假地址也没用：只认最右边的「非可信」地址。
func TestForwardedChainRightmostWins(t *testing.T) {
	c := cfgForIP(6000, 1, 10)
	c.TrustedProxies = []string{"127.0.0.1/32"}
	l := New(c)
	h := l.Wrap(okHandler())
	a := httptest.NewRecorder()
	h.ServeHTTP(a, req("127.0.0.1:5555", "/", map[string]string{"X-Forwarded-For": "9.9.9.9, 5.5.5.5"}))
	b := httptest.NewRecorder()
	h.ServeHTTP(b, req("127.0.0.1:5555", "/", map[string]string{"X-Forwarded-For": "7.7.7.7, 5.5.5.5"}))
	if b.Code != http.StatusTooManyRequests {
		t.Fatalf("换掉左边伪造的地址不该换到新配额，得到 %d", b.Code)
	}
	if got := l.ClientIP(req("127.0.0.1:5555", "/", map[string]string{"X-Forwarded-For": "9.9.9.9, 5.5.5.5"})); got != "5.5.5.5" {
		t.Fatalf("应取最右侧非可信地址，得到 %q", got)
	}
}

// 长下载占着并发名额，直到响应写完；超出并发回 429 而不是排队。
func TestConcurrencyLimit(t *testing.T) {
	l := New(cfgForIP(6000, 100, 2))
	block := make(chan struct{})
	var inflight, peak int
	var mu sync.Mutex
	h := l.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inflight++
		if inflight > peak {
			peak = inflight
		}
		mu.Unlock()
		<-block
		mu.Lock()
		inflight--
		mu.Unlock()
		w.WriteHeader(200)
	}))

	var wg sync.WaitGroup
	codes := make([]int, 3)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req("1.2.3.4:5555", "/big.iso", nil))
			codes[i] = rec.Code
		}(i)
	}
	// 等两个请求都进入处理函数
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := inflight
		mu.Unlock()
		if n == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req("1.2.3.4:5555", "/big2.iso", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("超过并发上限应 429，得到 %d", rec.Code)
	}
	close(block)
	wg.Wait()
	if codes[0] != 200 || codes[1] != 200 {
		t.Fatalf("占用名额的两个请求应成功：%v", codes)
	}
	if peak > 2 {
		t.Fatalf("同时进入处理函数的最多 2 个，实际 %d", peak)
	}
	// 名额要还回来，否则服务跑一阵就自己锁死了
	time.Sleep(20 * time.Millisecond)
	again := httptest.NewRecorder()
	h.ServeHTTP(again, req("1.2.3.4:5555", "/after.iso", nil))
	if again.Code != 200 {
		t.Fatalf("响应结束后名额应释放，得到 %d", again.Code)
	}
}

// 容器探活不能被限流卡住：429 会让容器显示不健康。
func TestHealthzExempt(t *testing.T) {
	c := cfgForIP(60, 1, 1)
	l := New(c)
	h := l.Wrap(okHandler())
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req("1.2.3.4:5555", "/healthz", nil))
		if rec.Code != 200 {
			t.Fatalf("第 %d 次探活被限流：%d", i+1, rec.Code)
		}
	}
}

// 带 Token 的再加一层，按 Token 分开计数。
func TestTokenLayer(t *testing.T) {
	c := cfgForIP(6000, 100, 10)
	c.TokenPerMinute = 6000
	c.TokenBurst = 1
	c.TokenConcurrency = 10
	l := New(c)
	h := l.Wrap(okHandler())

	a := httptest.NewRecorder()
	h.ServeHTTP(a, req("1.2.3.4:5555", "/", map[string]string{"Authorization": "Bearer aaa"}))
	b := httptest.NewRecorder()
	h.ServeHTTP(b, req("1.2.3.4:5555", "/", map[string]string{"Authorization": "Bearer bbb"}))
	if a.Code != 200 || b.Code != 200 {
		t.Fatalf("两个 Token 各有一份配额：%d %d", a.Code, b.Code)
	}
	c2 := httptest.NewRecorder()
	h.ServeHTTP(c2, req("1.2.3.4:5555", "/", map[string]string{"Authorization": "Bearer aaa"}))
	if c2.Code != http.StatusTooManyRequests || c2.Header().Get("X-ML-Limit") != "token" {
		t.Fatalf("同一 Token 用超应回 token 层 429，得到 %d %q", c2.Code, c2.Header().Get("X-ML-Limit"))
	}
}

// 全局层先挡：一台机器再快也压不垮整站。
func TestGlobalLimit(t *testing.T) {
	c := Default()
	c.GlobalRPS = 6000 // 100/s
	c.GlobalBurst = 2
	c.IPPerMinute = 600000
	c.IPBurst = 1000
	c.IPConcurrency = 100
	l := New(c)
	h := l.Wrap(okHandler())
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req("10.0.0."+strconv.Itoa(i+1)+":1", "/", nil))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req("10.0.0.9:1", "/", nil))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("X-ML-Limit") != "global" {
		t.Fatalf("全局层应拦下，得到 %d %q", rec.Code, rec.Header().Get("X-ML-Limit"))
	}
}

func TestDisabledPassesThrough(t *testing.T) {
	c := Default()
	c.Enabled = false
	l := New(c)
	if l.Enabled() {
		t.Fatal("Enabled=false 时应关闭")
	}
	h := l.Wrap(okHandler())
	for i := 0; i < 50; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req("1.2.3.4:5555", "/", nil))
		if rec.Code != 200 {
			t.Fatalf("关闭时不该限流，得到 %d", rec.Code)
		}
	}
}

// 旧键要能清掉，否则一个公网服务被扫一遍内存就回不来了。
func TestSweepDropsIdleBuckets(t *testing.T) {
	l := New(cfgForIP(6000, 5, 10))
	h := l.Wrap(okHandler())
	h.ServeHTTP(httptest.NewRecorder(), req("1.1.1.1:1", "/", nil))
	l.mu.Lock()
	if len(l.buck) == 0 {
		l.mu.Unlock()
		t.Fatal("应记录过键")
	}
	// 把键的时间戳往前拨，再触发一次清扫
	for _, b := range l.buck {
		b.seen = time.Now().Add(-time.Hour)
	}
	l.sweep = time.Now().Add(-time.Hour)
	l.mu.Unlock()
	h.ServeHTTP(httptest.NewRecorder(), req("2.2.2.2:1", "/", nil))
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.buck {
		if k == "ip:1.1.1.1" && b.seen.Before(time.Now().Add(-time.Minute)) {
			t.Fatalf("空闲键没被清掉: %s", k)
		}
	}
}
