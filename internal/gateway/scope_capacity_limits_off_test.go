package gateway

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter/registryadapter"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/store"
	"github.com/zggnh12345/mirror-lens/internal/unified"
)

// 3.5.17 第五轮：limits.enabled=false 只关「外部入口限流」，内部 Registry
// RequestScope 池（G）仍是独立硬上限——它是**有界排队**，不是无界并发。
//
// 冻结语义（1.6 §D-01）：G 满时请求在 Scope 准入处等待，父 Context 取消才
// 返回错误；因此这里断言「同时在跑的上游派发数不超过 G」，而不是「满了就
// 503」——把排队改成拒绝属于产品语义变更，不在本轮范围。
func TestScopeCapacityHardLimitWithLimitsDisabled(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatalf("缓存: %v", err)
	}

	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	var inflight, maxInflight, total atomic.Int32
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/manifests/") {
			cur := inflight.Add(1)
			mu.Lock()
			if cur > maxInflight.Load() {
				maxInflight.Store(cur)
			}
			mu.Unlock()
			total.Add(1)
			time.Sleep(120 * time.Millisecond)
			defer inflight.Add(-1)
		}
		w.Header().Set("Content-Type", manifestsAccept)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Write(body)
	}))
	t.Cleanup(srv.Close)

	host := strings.TrimPrefix(srv.URL, "http://")
	ad := registryadapter.New(host, 1<<20, time.Minute)
	ad.Scheme = "http"
	ad.CacheOrigin = "registry.test"

	// G=1：容量远小于并发请求数，任何绕过都会立刻体现在 maxInflight 上。
	gw := New(st, Options{
		Policies:     testGatewayPolicy,
		Cache:        cs,
		MaxBytes:     1 << 20,
		Registry:     ad,
		Guard:        safehttp.New(safehttp.Options{AllowLoopback: true}),
		RequireToken: true,
		Pool:         unified.NewPool(1),
	})
	if got := gw.scopePool.Capacity(); got != 1 {
		t.Fatalf("夹具池容量=%d，want 1", got)
	}
	plain, _, err := st.CreateToken("docker")
	if err != nil {
		t.Fatalf("建 Token: %v", err)
	}

	const n = 4
	// 每个请求用不同仓库：不同 CacheKey，才会真的各自派发（同键会被 fill 合并）。
	repos := []string{"alpine", "busybox", "nginx", "redis"}
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodGet, "http://gw/v2/library/"+repos[i]+"/manifests/latest", nil)
			r.SetBasicAuth("x", plain)
			w := httptest.NewRecorder()
			gw.ServeRegistry(w, r)
			codes[i] = w.Code
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("并发请求没有在 60s 内完成：内部容量上限可能没有生效")
	}
	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("第 %d 个请求应最终 200（排队后处理），得到 %d", i+1, c)
		}
	}
	if got := maxInflight.Load(); got > 1 {
		t.Fatalf("G=1 时上游最大并发派发=%d，内部硬上限被绕过", got)
	}
	if total.Load() != n {
		t.Fatalf("上游应被访问 %d 次，实际 %d", n, total.Load())
	}
}
