package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

type limitWriter struct {
	limit int
	n     int
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if w.n+len(p) > w.limit {
		return 0, errors.New("client gone")
	}
	w.n += len(p)
	return len(p), nil
}

// 客户端断开后，中途并行的 worker 必须全部退出，且不能留下可续传的错误状态。
func TestMidFillClientGoneStopsWorkers(t *testing.T) {
	data := make([]byte, midTotal)
	for i := range data {
		data[i] = byte(i*11 + i>>9)
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])

	var active atomic.Int32
	mk := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			active.Add(1)
			defer active.Add(-1)
			var from, to int64
			if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &from, &to); err != nil {
				http.Error(w, "bad range", http.StatusBadRequest)
				return
			}
			if to >= midTotal {
				to = midTotal - 1
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, midTotal))
			w.WriteHeader(http.StatusPartialContent)
			fl, _ := w.(http.Flusher)
			for off := from; off <= to; {
				n := int64(32 << 10)
				if off+n > to+1 {
					n = to + 1 - off
				}
				if _, err := w.Write(data[off : off+n]); err != nil {
					return
				}
				if fl != nil {
					fl.Flush()
				}
				off += n
				time.Sleep(10 * time.Millisecond)
			}
		}))
	}
	s1, s2 := mk(), mk()
	defer s1.Close()
	defer s2.Close()
	u1, _ := url.Parse(s1.URL)
	u2, _ := url.Parse(s2.URL)

	dir := t.TempDir()
	st, err := store.Open(dir + "/s.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs, err := cache.New(dir + "/c")
	if err != nil {
		t.Fatal(err)
	}
	g := &Gateway{
		st: st, cache: cs, cli: s1.Client(), now: time.Now, maxSize: 1 << 40,
		par: ParOptions{Enabled: true, Workers: 4, ChunkSize: 1 << 20, MinSize: 1 << 20},
		parTargets: func(*adapter.Plan) []fetchTarget {
			return []fetchTarget{{u: u1}, {u: u2}}
		},
	}
	key := "docker:blob:sha256:" + sha
	if err := cs.ResetPartial(key); err != nil {
		t.Fatal(err)
	}
	part, err := cs.OpenPartial(key)
	if err != nil {
		t.Fatal(err)
	}
	defer part.Close()
	if _, err := part.Write(data[:midSent]); err != nil {
		t.Fatal(err)
	}
	plan := &adapter.Plan{
		Upstream: u1, CacheKey: key, ExpectSHA: sha,
		Stream: true, Cacheable: true, Kind: "docker", TTL: time.Hour,
	}
	req := httptest.NewRequest(http.MethodGet, "/v2/library/postgres/blobs/sha256:"+sha, nil)
	old := &http.Response{Request: req, Header: http.Header{}}

	// 只让客户端收 128KB 就断开。
	out := &limitWriter{limit: 128 << 10}
	taken, ok := g.midFill(out, plan, part, midSent, midTotal, nil, old, nil)
	if !taken || ok {
		t.Fatalf("客户端断开应 taken=true ok=false，实际 %v %v", taken, ok)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if active.Load() == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := active.Load(); n != 0 {
		t.Fatalf("客户端断开后还有 %d 个后台请求没退出", n)
	}
	if _, ok := cs.ReadPartial(key); ok {
		t.Fatal("失败后不应留下可续传的 partial")
	}
}
