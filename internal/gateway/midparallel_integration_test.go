package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

const (
	midTotal = 8 << 20
	midSent  = 2 << 20
)

type midFixture struct {
	g     *Gateway
	st    *store.Store
	cs    *cache.Store
	data  []byte
	sha   string
	urls  []*url.URL
	key   string
	close func()
}

func newMidFixture(t *testing.T, srcs int) *midFixture {
	return newMidFixtureOpt(t, srcs, false)
}

func newMidFixtureOpt(t *testing.T, srcs int, noRange bool) *midFixture {
	t.Helper()
	data := make([]byte, midTotal)
	for i := range data {
		data[i] = byte(i*7 + i>>11)
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])

	var servers []*httptest.Server
	mk := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if noRange { // 模拟 HubProxy：不认 Range，整份 200
				w.Header().Set("Content-Type", "application/octet-stream")
				w.WriteHeader(http.StatusOK)
				w.Write(data)
				return
			}
			var from, to int64
			if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &from, &to); err != nil {
				http.Error(w, "bad range", http.StatusBadRequest)
				return
			}
			if to >= midTotal {
				to = midTotal - 1
			}
			// 探测请求不拖，段请求故意慢一点：段没下完就发会读到空洞。
			if !(from == 0 && to == 0) {
				time.Sleep(20 * time.Millisecond)
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, midTotal))
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusPartialContent)
			w.Write(data[from : to+1])
		}))
	}
	var urls []*url.URL
	for i := 0; i < srcs; i++ {
		s := mk()
		servers = append(servers, s)
		u, _ := url.Parse(s.URL)
		urls = append(urls, u)
	}

	dir := t.TempDir()
	st, err := store.Open(dir + "/s.db")
	if err != nil {
		t.Fatal(err)
	}
	cs, err := cache.New(dir + "/c")
	if err != nil {
		t.Fatal(err)
	}
	g := &Gateway{
		st:      st,
		cache:   cs,
		cli:     servers[0].Client(),
		now:     time.Now,
		maxSize: 1 << 40,
		par: ParOptions{
			Enabled:   true,
			Workers:   4,
			ChunkSize: 1 << 20,
			MinSize:   1 << 20,
		},
		parTargets: func(*adapter.Plan) []fetchTarget {
			out := make([]fetchTarget, len(urls))
			for i, u := range urls {
				out[i] = fetchTarget{u: u}
			}
			return out
		},
	}
	return &midFixture{g: g, st: st, cs: cs, data: data, sha: sha, urls: urls,
		key: "docker:blob:sha256:" + sha,
		close: func() {
			st.Close()
			for _, s := range servers {
				s.Close()
			}
		}}
}

// run 写前缀、调 midFill，返回客户端收到的字节与结果。
func (f *midFixture) run(t *testing.T, expectSHA string) ([]byte, bool, bool) {
	t.Helper()
	if err := f.cs.ResetPartial(f.key); err != nil {
		t.Fatal(err)
	}
	part, err := f.cs.OpenPartial(f.key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(f.data[:midSent]); err != nil {
		t.Fatal(err)
	}
	plan := &adapter.Plan{
		Upstream:  f.urls[0],
		CacheKey:  f.key,
		ExpectSHA: expectSHA,
		Stream:    true,
		Cacheable: true,
		Kind:      "docker",
		TTL:       time.Hour,
	}
	req := httptest.NewRequest(http.MethodGet, "/v2/library/postgres/blobs/sha256:"+f.sha, nil)
	old := &http.Response{Request: req, Header: http.Header{}}
	var out bytes.Buffer
	taken, ok := f.g.midFill(&out, plan, part, midSent, midTotal, nil, old)
	return out.Bytes(), taken, ok
}

func TestMidFillFromOffset(t *testing.T) {
	f := newMidFixture(t, 2)
	defer f.close()
	out, taken, ok := f.run(t, f.sha)
	if !taken || !ok {
		t.Fatalf("midFill taken=%v ok=%v", taken, ok)
	}
	if len(out) != midTotal-midSent {
		t.Fatalf("客户端收到 %d 字节，期望 %d", len(out), midTotal-midSent)
	}
	if !bytes.Equal(out, f.data[midSent:]) {
		t.Fatal("续传字节与源数据不一致")
	}
	e, err := f.st.CacheGet(f.key)
	if err != nil || e == nil {
		t.Fatalf("缓存未写入: %v", err)
	}
	if e.SHA256 != f.sha || e.Size != midTotal {
		t.Fatalf("缓存条目不对: %+v", e)
	}
}

// 整份校验不通过时必须不入缓存（客户端已收到的字节无法收回）。
func TestMidFillChecksumMismatch(t *testing.T) {
	f := newMidFixture(t, 2)
	defer f.close()
	_, taken, ok := f.run(t, strings.Repeat("0", 64))
	if !taken || ok {
		t.Fatalf("校验失败应返回 taken=true ok=false，实际 %v %v", taken, ok)
	}
	if e, _ := f.st.CacheGet(f.key); e != nil {
		t.Fatal("校验失败仍写入了缓存")
	}
}

// 一台能分片的源也没有时，不能动客户端连接。
func TestMidFillNoSources(t *testing.T) {
	f := newMidFixtureOpt(t, 2, true)
	defer f.close()
	if err := f.cs.ResetPartial(f.key); err != nil {
		t.Fatal(err)
	}
	part, _ := f.cs.OpenPartial(f.key)
	part.Write(f.data[:midSent])
	plan := &adapter.Plan{
		Upstream:  f.urls[0],
		CacheKey:  f.key,
		ExpectSHA: f.sha,
		Stream:    true,
		Cacheable: true,
		Kind:      "docker",
		TTL:       time.Hour,
	}
	req := httptest.NewRequest(http.MethodGet, "/v2/x", nil)
	old := &http.Response{Request: req, Header: http.Header{}}
	var out bytes.Buffer
	taken, ok := f.g.midFill(&out, plan, part, midSent, midTotal, nil, old)
	if taken || ok {
		t.Fatalf("无分片源应放弃接入，实际 taken=%v ok=%v", taken, ok)
	}
	if out.Len() != 0 {
		t.Fatalf("放弃时不该写客户端，实际写了 %d 字节", out.Len())
	}
}

// 只有一台支持 Range 时，对同一台开多连接补齐剩余段。
func TestMidFillSingleSource(t *testing.T) {
	f := newMidFixture(t, 1)
	defer f.close()
	out, taken, ok := f.run(t, f.sha)
	if !taken || !ok {
		t.Fatalf("单源也应接入，实际 taken=%v ok=%v", taken, ok)
	}
	if !bytes.Equal(out, f.data[midSent:]) {
		t.Fatal("单源多连接拼出的字节不对")
	}
	if e, _ := f.st.CacheGet(f.key); e == nil {
		t.Fatal("单源接入后缓存未写入")
	}
}
