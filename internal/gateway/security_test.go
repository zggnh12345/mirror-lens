package gateway

import (
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 私有仓库的 PAT 只能交给 GitHub 官方：镜像站拿到就等于凭据泄露。
func TestPrivateRequestSkipsMirrors(t *testing.T) {
	g, st, _, _, _ := newGW(t, true)
	g.requireToken = false // 匿名模式：Authorization 才有可能是上游 PAT

	var hits int32
	var leaked atomic.Value
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if a := r.Header.Get("Authorization"); a != "" {
			leaked.Store(a)
		}
		http.Error(w, "mirror should not be reached", http.StatusBadGateway)
	}))
	defer proxy.Close()

	if _, err := st.AddSource(store.Source{
		BaseURL: proxy.URL, Name: "evil-mirror", Enabled: true,
		Status: "usable", GithubCap: 1,
	}); err != nil {
		t.Fatalf("插入镜像源: %v", err)
	}
	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true, MaxCandidates: 5}))

	rec := do(g, "", "http://gw/raw/o/r/main/a.txt", map[string]string{
		"Authorization": "token ghp_secret",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("应回源成功，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("私有请求打到了镜像站 %d 次", n)
	}
	if v := leaked.Load(); v != nil {
		t.Fatalf("PAT 泄漏给镜像站: %v", v)
	}
}

// Cacheable=false 的响应不能留任何字节在缓存目录，也不能被后续请求命中。
func TestPrivateResponseNotPersisted(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	defer st.Close()
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatalf("缓存: %v", err)
	}
	up := &fakeUpstream{body: "top-secret", etag: `"v1"`}
	srv := httptest.NewServer(up)
	defer srv.Close()
	g := New(st, Options{
		Cache:   cs,
		RawBase: srv.URL,
		Guard:   safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	g.requireToken = false

	hdr := map[string]string{"Authorization": "token ghp_secret"}
	if rec := do(g, "", "http://gw/raw/o/r/main/secret.txt", hdr); rec.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if n := treeSize(t, filepath.Join(dir, "cache")); n != 0 {
		t.Fatalf("私有内容落盘了: %d 字节", n)
	}
	// 第二次同凭据请求必须重新回源，说明没有缓存可命中。
	do(g, "", "http://gw/raw/o/r/main/secret.txt", hdr)
	// 匿名请求也不能读到私有内容。
	do(g, "", "http://gw/raw/o/r/main/secret.txt", nil)
	if n := atomic.LoadInt32(&up.hits); n != 3 {
		t.Fatalf("三次请求应全部回源，实际回源 %d 次", n)
	}
	// 匿名请求会正常缓存（content 只有 10 字节），这里只要求
	// 私有请求阶段缓存目录为 0，上面已断言。
	if n := treeSize(t, filepath.Join(dir, "cache")); n != int64(len("top-secret")) {
		t.Fatalf("匿名缓存大小异常: %d 字节", n)
	}
}

// 并行竞速两条链都下载成功时，只有一条能提交，且缓存内容必须等于响应内容。
func TestFetchRaceCommitsOnlyWinner(t *testing.T) {
	g, _, _, _, _ := newGW(t, true)
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4096")
		_, _ = w.Write([]byte(strings.Repeat("A", 4096)))
	}))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4096")
		_, _ = w.Write([]byte(strings.Repeat("B", 4096)))
	}))
	defer b.Close()

	plan := &adapter.Plan{
		Kind:      "docker",
		Upstream:  mustURL(t, a.URL),
		Method:    http.MethodGet,
		Header:    http.Header{},
		CacheKey:  "race:test",
		Cacheable: true,
		TTL:       time.Hour,
	}
	e, _, err := g.fetchRace(context.Background(), plan,
		[]fetchTarget{{u: mustURL(t, a.URL)}},
		[]fetchTarget{{u: mustURL(t, b.URL), fb: true}})
	if err != nil {
		t.Fatalf("竞速失败: %v", err)
	}
	got, err := g.st.CacheGet("race:test")
	if err != nil {
		t.Fatalf("读索引: %v", err)
	}
	if got == nil {
		t.Fatal("胜者没有提交缓存")
	}
	if got.SHA256 != e.SHA256 {
		t.Fatalf("缓存内容与响应不一致: cache=%s resp=%s", got.SHA256, e.SHA256)
	}
	// 再取一次，确保索引没有被后完成的链覆盖。
	again, _ := g.st.CacheGet("race:test")
	if again == nil || again.SHA256 != e.SHA256 {
		t.Fatalf("缓存被另一条链覆盖: %+v", again)
	}
}

func treeSize(t *testing.T, root string) int64 {
	t.Helper()
	var n int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, e := d.Info(); e == nil {
			n += fi.Size()
		}
		return nil
	})
	return n
}
