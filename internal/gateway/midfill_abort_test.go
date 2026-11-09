package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/metrics"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// P0 回归：走生产链路（metrics.Middleware 包一层 recorder）、HTTP/1.1、
// midFill 已经向客户端写出字节，随后分片内容被改坏导致整份 SHA 不符。
// 此时客户端必须看到 RST，而不是正常 EOF——否则损坏文件会被当成完整响应。
// 同时断言 .part 清理、无缓存索引、Lease 归还、失败事件能归因到单源。
func TestMidFillVerifyFailureResetsClient(t *testing.T) {
	const total = 8 << 20
	const sent = 2 << 20
	data := make([]byte, total)
	for i := range data {
		data[i] = byte(i*13 + i>>9)
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	key := "docker:blob:sha256:" + sha

	// 前 4MB 正确，之后的段每段最后一字节改坏：整份摘要必错，但分片请求都成功。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var from, to int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &from, &to); err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		if to >= total {
			to = total - 1
		}
		body := append([]byte(nil), data[from:to+1]...)
		if from >= 4<<20 && len(body) > 0 {
			body[len(body)-1] ^= 0xff
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, total))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body)
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
	sid := addSchedSource(t, st, "only", srv.URL, 10, 9000)
	sch := scheduler.New(st, scheduler.Config{Enabled: true, MaxPerSource: 4, MaxCandidates: 3})

	g := &Gateway{
		st:      st,
		cache:   cs,
		cli:     srv.Client(),
		now:     time.Now,
		maxSize: 1 << 40,
		par:     ParOptions{Enabled: true, Workers: 4, ChunkSize: 1 << 20, MinSize: 1 << 20},
		parTargets: func(*adapter.Plan) []fetchTarget {
			return []fetchTarget{{u: u, c: &scheduler.Candidate{ID: sid, Name: "only"}}}
		},
	}
	g.SetScheduler(sch)

	plan := &adapter.Plan{
		Upstream: u, CacheKey: key, ExpectSHA: sha,
		Stream: true, Cacheable: true, Kind: "docker", TTL: time.Hour,
	}

	metrics.Enable()
	defer metrics.Disable()

	var taken, ok bool
	done := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Length", strconv.Itoa(total))
		w.WriteHeader(http.StatusOK)
		// 模拟单流阶段已经写出的前缀：客户端先收到真实字节，响应无法撤回。
		if _, err := w.Write(data[:sent]); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
		part, err := cs.OpenPartial(key)
		if err != nil {
			t.Errorf("打开 .part: %v", err)
			return
		}
		if _, err := part.Write(data[:sent]); err != nil {
			t.Errorf("写 .part 前缀: %v", err)
			return
		}
		killConn, canAbort := abortResponse(w)
		if !canAbort {
			t.Error("生产链路应支持异常终止连接")
			return
		}
		taken, ok = g.midFill(w, plan, part, sent, total, flusher,
			&http.Response{Request: r, Header: http.Header{}}, killConn)
	})
	ts := httptest.NewServer(metrics.Middleware(h))
	defer ts.Close()

	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(ts.URL, "http://"), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := conn.Write([]byte("GET /blob HTTP/1.1\r\nHost: probe\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	n, rerr := io.Copy(io.Discard, conn)
	<-done

	if n == 0 {
		t.Fatal("客户端一个字节都没收到，没有覆盖「已写出后失败」的路径")
	}
	if !isReset(rerr) {
		t.Fatalf("校验失败后客户端应看到连接重置，实际读完 %d 字节后: %v", n, rerr)
	}
	if !taken || ok {
		t.Fatalf("midFill 应接管并失败，实际 taken=%v ok=%v", taken, ok)
	}
	if e, _ := st.CacheGet(key); e != nil {
		t.Fatalf("校验失败仍写了缓存索引: %+v", e)
	}
	if part, _ := cs.PartialPaths(key); fileExists(part) {
		t.Fatalf(".part 未清理: %s", part)
	}
	if held := sch.LeaseHeld(sid); held != 0 {
		t.Fatalf("Lease 未归还，仍持有 %d", held)
	}
	rows, err := st.BlobFailures(0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SourceID != sid || rows[0].SourceName != "only" {
		t.Fatalf("失败事件未归因到单源: %+v", rows)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
