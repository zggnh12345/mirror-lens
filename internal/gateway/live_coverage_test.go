package gateway

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// GitHub/git 走「先整份落盘、再下发」，Docker 命中缓存也只下发不发请求：
// 这两段以前都不登记任务，面板看着像没动静。这里锁住回源/下发/命中三条留痕。
func TestLiveTaskCoversFetchServeAndHit(t *testing.T) {
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

	body := make([]byte, 5<<20)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("ETag", `"v1"`)
		w.Write(body)
	}))
	defer origin.Close()

	g := New(st, Options{
		Cache:   cs,
		RawBase: origin.URL,
		RawTTL:  time.Minute,
		Guard:   safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	tok := token(t, st)
	target := "http://gw/raw/o/r/main/big.bin"

	g.requireToken = true
	if rec := do(g, tok, target, nil); rec.Body.Len() != len(body) {
		t.Fatalf("首次下载应拿到 %d 字节，实际 %d", len(body), rec.Body.Len())
	}
	if !hasMode(g.ActiveTasks(), "fetch") || !hasMode(g.ActiveTasks(), "served") {
		t.Fatalf("回源与下发都要有任务，实际 %+v", g.ActiveTasks().Recent)
	}

	if rec := do(g, tok, target, nil); rec.Body.Len() != len(body) {
		t.Fatalf("命中应拿到 %d 字节，实际 %d", len(body), rec.Body.Len())
	}
	if !hasMode(g.ActiveTasks(), "cache") {
		t.Fatalf("命中缓存也要有任务，实际 %+v", g.ActiveTasks().Recent)
	}
}

func hasMode(s ActiveSnapshot, mode string) bool {
	for _, r := range s.Recent {
		if r.Mode == mode && r.Total >= liveMinBytes {
			return true
		}
	}
	return false
}
