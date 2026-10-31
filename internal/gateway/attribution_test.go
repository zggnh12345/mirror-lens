package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

func TestIsSourceFault(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"上游 500", &upstreamError{Code: 500}, true},
		{"连不上", errors.New("dial tcp: connection refused"), true},
		{"读上游中断", errors.New("读取上游中断: unexpected EOF"), true},
		{"写盘失败", local(errors.New("no space left on device")), false},
		{"提交缓存失败", local(errors.New("rename: permission denied")), false},
		{"超上限", &tooLargeError{Max: 1 << 20}, false},
		{"客户端取消", context.Canceled, false},
	}
	for _, c := range cases {
		if got := isSourceFault(c.err); got != c.want {
			t.Errorf("%s: isSourceFault=%v，期望 %v", c.name, got, c.want)
		}
	}
}

// 磁盘写不进去是我们的问题：不能记到源站账上（磁盘满一排好源全被熔断）。
func TestDiskFailureNotBlamedOnSource(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	// 让 CreateTemp 必定失败（root 也绕不过 ENOTDIR）。
	tmpDir := filepath.Join(dir, "cache", "tmp")
	if err := os.RemoveAll(tmpDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmpDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	origin := srv(t, 0, "origin-body")
	mirror := srv(t, 0, "mirror-body")
	id := addSchedSource(t, st, "mirror", mirror.URL, 10, 9000)

	g := New(st, Options{
		Cache:   cs,
		RawBase: origin.URL,
		RawTTL:  time.Minute,
		Guard:   safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true, MaxCandidates: 3}))

	tok := token(t, st)
	rec := do(g, tok, "http://gw/raw/o/r/main/a.txt", nil)
	// 盘写不进去也要把字节送到：降级成透传，而不是 502。
	if rec.Code != http.StatusOK || rec.Body.String() != "mirror-body" {
		t.Fatalf("应降级透传 200 mirror-body，实际 %d %q", rec.Code, rec.Body.String())
	}
	okN, failN, _, _ := st.SourceEvents(id, 10)
	if failN != 0 {
		t.Fatalf("本地磁盘故障不该记源站失败，实际 ok=%d fail=%d", okN, failN)
	}
	if okN != 1 {
		t.Fatalf("透传成功应记 1 次成功，实际 ok=%d", okN)
	}
}

// 对照组：真实的上游故障仍然要记，别把归因修成「什么都不记」。
func TestUpstreamFailureStillBlamed(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}

	origin := srv(t, 0, "origin-body")
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	id := addSchedSource(t, st, "bad", bad.URL, 10, 9000)

	g := New(st, Options{
		Cache:   cs,
		RawBase: origin.URL,
		RawTTL:  time.Minute,
		Guard:   safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true, MaxCandidates: 3}))

	rec := do(g, token(t, st), "http://gw/raw/o/r/main/a.txt", nil)
	// 换源成功是正常结果；关键是坏源要被记一笔。
	if rec.Code != http.StatusOK || rec.Body.String() != "origin-body" {
		t.Fatalf("应换到官方源 200 origin-body，实际 %d %q", rec.Code, rec.Body.String())
	}
	if _, failN, _, _ := st.SourceEvents(id, 10); failN == 0 {
		t.Fatal("真实上游故障必须记失败")
	}
}
