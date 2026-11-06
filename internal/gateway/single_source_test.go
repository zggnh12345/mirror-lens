package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 单源多连接：busy[0] 记了首段占用后，源 0 仍是可派活的分片源；
// 连续两次都能跑完，说明名额有借有还，没有泄漏。
func TestMidFillSingleSourceRepeat(t *testing.T) {
	data := make([]byte, 6<<20)
	for i := range data {
		data[i] = byte(i*7 + i>>8)
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])

	var active atomic.Int32
	var peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		var from, to int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &from, &to); err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		if to >= int64(len(data)) {
			to = int64(len(data)) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[from : to+1])
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

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
		st: st, cache: cs, cli: srv.Client(), now: time.Now, maxSize: 1 << 40,
		par: ParOptions{Enabled: true, Workers: 4, ChunkSize: 1 << 20, MinSize: 1 << 20},
		parTargets: func(*adapter.Plan) []fetchTarget {
			return []fetchTarget{{u: u, c: &scheduler.Candidate{ID: 1, Name: "only", Preferred: true}}}
		},
	}

	for round := 0; round < 2; round++ {
		key := fmt.Sprintf("docker:blob:sha256:%s-%d", sha, round)
		if err := cs.ResetPartial(key); err != nil {
			t.Fatal(err)
		}
		part, err := cs.OpenPartial(key)
		if err != nil {
			t.Fatal(err)
		}
		const sent = int64(700 << 10)
		if _, err := part.Write(data[:sent]); err != nil {
			t.Fatal(err)
		}
		plan := &adapter.Plan{
			Upstream: u, CacheKey: key, ExpectSHA: sha,
			Stream: true, Cacheable: true, Kind: "docker", TTL: time.Hour,
		}
		req := httptest.NewRequest(http.MethodGet, "/v2/x/blobs/sha256:"+sha, nil)
		old := &http.Response{Request: req, Header: http.Header{}}
		var out bytes.Buffer
		taken, ok := g.midFill(&out, plan, part, sent, int64(len(data)), nil, old)
		part.Close()
		if !taken || !ok {
			t.Fatalf("第 %d 轮单源多连接没接上: taken=%v ok=%v", round+1, taken, ok)
		}
		if out.Len() != len(data)-int(sent) {
			t.Fatalf("第 %d 轮补发 %d 字节，期望 %d", round+1, out.Len(), len(data)-int(sent))
		}
	}
	if peak.Load() < 2 {
		t.Fatalf("单源 4 worker 峰值只有 %d 个并发请求，说明源 0 被 busy 计数挡住了", peak.Load())
	}
	if n := active.Load(); n != 0 {
		t.Fatalf("结束后还有 %d 个后台请求", n)
	}
}
