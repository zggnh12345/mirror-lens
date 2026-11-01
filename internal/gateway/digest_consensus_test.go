package gateway

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter/registryadapter"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/safehttp"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// blobMirror 是一台只会原样吐字节的镜像：不校验路径，也不支持 Range。
type blobMirror struct {
	mu     sync.Mutex
	body   []byte
	digest string // 自报的 Content-Digest
	status int
	hits   int32
}

func (m *blobMirror) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&m.hits, 1)
	m.mu.Lock()
	body, digest, status := m.body, m.digest, m.status
	m.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	if digest != "" {
		w.Header().Set("Content-Digest", digest)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Write(body)
}

func (m *blobMirror) set(body []byte, digest string) {
	m.mu.Lock()
	m.body, m.digest = body, digest
	m.mu.Unlock()
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// newConsensusFixture 起一台「真源 + 一台镜像」，客户端按 digest 拉 blob。
func newConsensusFixture(t *testing.T) (*Gateway, *store.Store, *regUpstream, *blobMirror, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}

	up := &regUpstream{}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)

	ad := registryadapter.New(srv.URL[len("http://"):], 4<<30, 5*time.Minute)
	ad.Scheme = "http"

	g := New(st, Options{
		Cache:    cs,
		MaxBytes: 256 << 20,
		Registry: ad,
		Guard:    safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	sch := scheduler.New(st, scheduler.Config{Enabled: true, MaxCandidates: 3, FailCooldown: time.Minute})
	g.SetScheduler(sch)

	mirror := &blobMirror{}
	msrv := httptest.NewServer(mirror)
	t.Cleanup(msrv.Close)
	addSchedSource(t, st, "mirror", msrv.URL, 10, 9000)

	plain, _, err := st.CreateToken("docker")
	if err != nil {
		t.Fatal(err)
	}
	return g, st, up, mirror, plain
}

func blobGet(g *Gateway, tok, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "http://gw"+path, nil)
	r.SetBasicAuth("x", tok)
	w := httptest.NewRecorder()
	g.ServeRegistry(w, r)
	return w
}

// 镜像给的字节对不上客户端声明的 digest：坏字节可以已经流给客户端（没法收回），
// 但绝不能进缓存；下一次请求应换一台源，拿到的必须是真内容。
func TestDigestMismatchOnMirrorNeverCached(t *testing.T) {
	g, st, up, mirror, tok := newConsensusFixture(t)
	wrong := []byte("this is not the blob")
	sum := sha256.Sum256(wrong)
	mirror.set(wrong, "sha-256=:"+base64.StdEncoding.EncodeToString(sum[:])+":")

	blobGet(g, tok, "/v2/library/alpine/blobs/"+up.blobDigest())
	if e, _ := st.CacheGet("docker:blob:" + up.blobDigest()); e != nil {
		t.Fatalf("坏字节进了缓存: %+v", e)
	}

	// 第二次：坏镜像已经记过失败，应该换真源，缓存里必须是正确摘要。
	w := blobGet(g, tok, "/v2/library/alpine/blobs/"+up.blobDigest())
	if w.Code != http.StatusOK {
		t.Fatalf("第二次应成功，实际 %d", w.Code)
	}
	e, err := st.CacheGet("docker:blob:" + up.blobDigest())
	if err != nil || e == nil {
		t.Fatalf("正确内容应入缓存: %v %v", e, err)
	}
	if e.SHA256 != sha256Hex(blobBytes) {
		t.Fatalf("缓存里是错内容的摘要 %s", e.SHA256)
	}
	if n := atomic.LoadInt32(&mirror.hits); n != 1 {
		t.Fatalf("坏镜像只该试一次，实际 %d", n)
	}
}

// 所有源都对不上：反复请求也不许留下任何缓存。
func TestDigestMismatchEverywhereNotCached(t *testing.T) {
	g, st, up, mirror, tok := newConsensusFixture(t)
	mirror.set([]byte("bad-bytes"), "")
	up.tamperBlob = true

	for i := 0; i < 2; i++ {
		blobGet(g, tok, "/v2/library/alpine/blobs/"+up.blobDigest())
		if e, _ := st.CacheGet("docker:blob:" + up.blobDigest()); e != nil {
			t.Fatalf("第 %d 次：摘要不符的内容入了缓存: %+v", i+1, e)
		}
	}
}

// 镜像自报摘要与自己的字节不符（撒谎）：没有客户端摘要兜底时，也要靠它拦下。
func TestLyingSelfDigestRejected(t *testing.T) {
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
	origin := srv(t, http.StatusInternalServerError, "")
	lie := sha256.Sum256([]byte("something else"))
	mirror := &blobMirror{
		body:   []byte("hello"),
		digest: "sha-256=:" + base64.StdEncoding.EncodeToString(lie[:]) + ":",
	}
	msrv := httptest.NewServer(mirror)
	defer msrv.Close()
	addSchedSource(t, st, "mirror", msrv.URL, 10, 9000)

	g := New(st, Options{
		Cache:   cs,
		RawBase: origin.URL,
		RawTTL:  time.Minute,
		Guard:   safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true, MaxCandidates: 3, FailCooldown: time.Minute}))

	rec := do(g, token(t, st), "http://gw/raw/o/r/main/a.txt", nil)
	if rec.Code < 500 {
		t.Fatalf("自报摘要不符应失败，实际 %d", rec.Code)
	}
	if list, _ := st.CacheList(10); len(list) != 0 {
		t.Fatalf("摘要不符的内容不该入缓存: %+v", list)
	}
	if g.VerifyFailed() == 0 {
		t.Fatal("撒谎的摘要应计入校验失败")
	}
}

// raw 路径：谁都没给摘要，也只能记 unverified，不能凭空升级成可信。
func TestNoDigestAnywhereStaysUnverified(t *testing.T) {
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
	origin := srv(t, http.StatusInternalServerError, "")
	mirror := &blobMirror{body: []byte("hello")}
	msrv := httptest.NewServer(mirror)
	defer msrv.Close()
	addSchedSource(t, st, "mirror", msrv.URL, 10, 9000)

	g := New(st, Options{
		Cache:   cs,
		RawBase: origin.URL,
		RawTTL:  time.Minute,
		Guard:   safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true, MaxCandidates: 3, FailCooldown: time.Minute}))

	if rec := do(g, token(t, st), "http://gw/raw/o/r/main/a.txt", nil); rec.Code != http.StatusOK {
		t.Fatalf("镜像应能供货，实际 %d", rec.Code)
	}
	list, err := st.CacheList(10)
	if err != nil || len(list) != 1 {
		t.Fatalf("应有一条缓存: %v %v", list, err)
	}
	if list[0].Verify != "unverified" {
		t.Fatalf("没有可信摘要时只能是 unverified，实际 %q", list[0].Verify)
	}
}

// 摘要格式坏掉：当成「没给」，既不拒收也不当作已验证。
func TestMalformedDigestTreatedAsAbsent(t *testing.T) {
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
	origin := srv(t, http.StatusInternalServerError, "")
	mirror := &blobMirror{body: []byte("hello"), digest: "sha-256=:not-base64:"}
	msrv := httptest.NewServer(mirror)
	defer msrv.Close()
	addSchedSource(t, st, "mirror", msrv.URL, 10, 9000)

	g := New(st, Options{
		Cache:   cs,
		RawBase: origin.URL,
		RawTTL:  time.Minute,
		Guard:   safehttp.New(safehttp.Options{AllowLoopback: true}),
	})
	g.SetScheduler(scheduler.New(st, scheduler.Config{Enabled: true, MaxCandidates: 3, FailCooldown: time.Minute}))

	if rec := do(g, token(t, st), "http://gw/raw/o/r/main/a.txt", nil); rec.Code != http.StatusOK {
		t.Fatalf("坏摘要头不该导致失败，实际 %d", rec.Code)
	}
	list, _ := st.CacheList(10)
	if len(list) != 1 || list[0].Verify == "verified" {
		t.Fatalf("坏摘要不能变成 verified，实际 %+v", list)
	}
}
