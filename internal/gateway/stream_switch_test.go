package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

const swTotal = 8 << 20

type swSrc struct {
	srv  *httptest.Server
	slow bool
	etag string
	// cutAt > 0 时，发出这么多字节后直接断开 TCP。
	cutAt int64
	// ignoreRange 时无视 Range，从 0 返回 200 全量。
	ignoreRange bool
	// stallAt > 0 时，发出这么多字节后卡住不动（等客户端换源或取消）。
	stallAt int64
}

func newSwSrc(data []byte, etag string, slow bool, cutAt int64) *swSrc {
	return newSwSrcFull(data, etag, slow, cutAt, 0, false)
}

// newSwSrcFull 额外支持 ignoreRange（200 全量）和 stallAt（发到指定字节后卡住）。
func newSwSrcFull(data []byte, etag string, slow bool, cutAt, stallAt int64, ignoreRange bool) *swSrc {
	s := &swSrc{slow: slow, etag: etag, cutAt: cutAt, ignoreRange: ignoreRange, stallAt: stallAt}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		from, to := int64(0), int64(len(data))-1
		if s.ignoreRange {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		} else {
			if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &from, &to); err != nil {
				// 也接受开放区间 bytes=N-
				if _, err2 := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &from); err2 != nil {
					http.Error(w, "bad range", http.StatusBadRequest)
					return
				}
				to = int64(len(data)) - 1
			}
			if to >= int64(len(data)) {
				to = int64(len(data)) - 1
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, len(data)))
		}
		if etag != "" {
			w.Header().Set("ETag", etag)
		}
		if s.ignoreRange {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusPartialContent)
		}
		fl, _ := w.(http.Flusher)
		var sent int64
		for off := from; off <= to; {
			if s.stallAt > 0 && sent >= s.stallAt {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(10 * time.Second):
					return
				}
			}
			n := int64(64 << 10)
			if off+n > to+1 {
				n = to + 1 - off
			}
			if s.stallAt > 0 && sent+n > s.stallAt {
				n = s.stallAt - sent
			}
			if s.cutAt > 0 && sent+n > s.cutAt {
				// 模拟传输中途断流。
				if hj, ok := w.(http.Hijacker); ok {
					if conn, _, err := hj.Hijack(); err == nil {
						conn.Close()
						return
					}
				}
				return
			}
			if n > 0 {
				if _, err := w.Write(data[off : off+n]); err != nil {
					return
				}
				if fl != nil {
					fl.Flush()
				}
				off += n
				sent += n
			}
			if s.slow {
				time.Sleep(60 * time.Millisecond) // ~1MB/s
			}
		}
	}))
	return s
}

type swFixture struct {
	g    *Gateway
	cs   *cache.Store
	data []byte
	sha  string
}

func newSwFixture(t *testing.T) *swFixture {
	t.Helper()
	data := make([]byte, swTotal)
	for i := range data {
		data[i] = byte(i*13 + i>>10)
	}
	sum := sha256.Sum256(data)
	dir := t.TempDir()
	st, err := store.Open(dir + "/s.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cs, err := cache.New(dir + "/c")
	if err != nil {
		t.Fatal(err)
	}
	return &swFixture{
		g: &Gateway{
			st: st, cache: cs, cli: http.DefaultClient, now: time.Now, maxSize: 1 << 40,
			par: ParOptions{
				Enabled:       false, // 只测单流换源，别让中途并行抢先
				SwitchMinMBps: 1, WatchWindow: 2 * time.Second, WatchMBps: 3,
				SwitchMinFileMiB: 1, SwitchMaxSentMiB: 48, SwitchMax: 2,
			},
		},
		cs: cs, data: data, sha: hex.EncodeToString(sum[:]),
	}
}

func (f *swFixture) stream(t *testing.T, slow, fast *swSrc, expect string) ([]byte, *adapter.Plan) {
	t.Helper()
	uSlow, _ := url.Parse(slow.srv.URL)
	uFast, _ := url.Parse(fast.srv.URL)
	g := f.g
	g.cli = slow.srv.Client()
	g.speedProbe = func(id int64, kind string, n int) []int64 {
		if id == 2 {
			return []int64{64 << 10, 64 << 10, 64 << 10} // 64MB/s 的快样本（中位数取 3 条）
		}
		return nil
	}
	g.parTargets = func(*adapter.Plan) []fetchTarget {
		return []fetchTarget{
			{u: uSlow, c: &scheduler.Candidate{ID: 1, Name: "slow"}},
			{u: uFast, c: &scheduler.Candidate{ID: 2, Name: "fast", Preferred: true}},
		}
	}
	key := "docker:blob:sha256:" + f.sha
	_ = f.cs.ResetPartial(key)
	plan := &adapter.Plan{
		Upstream: uSlow, CacheKey: key, ExpectSHA: expect,
		Stream: true, Cacheable: true, Kind: "docker", TTL: time.Hour,
	}
	req := httptest.NewRequest(http.MethodGet, "/v2/library/postgres/blobs/sha256:"+f.sha, nil)
	ureq, _ := http.NewRequestWithContext(req.Context(), http.MethodGet, slow.srv.URL+"/v2/x/blobs/sha256:"+f.sha, nil)
	ureq.Header.Set("Range", "bytes=0-")
	resp, err := g.cli.Do(ureq)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	g.streamBody(rec, req, plan, resp, nil, &fetchTarget{u: uSlow}, 0)
	return rec.Body.Bytes(), plan
}

// 验证器一致时换源：内容必须完整，续传地址要切到新源。
func TestStreamSwitchToVerifiedSource(t *testing.T) {
	f := newSwFixture(t)
	slow := newSwSrc(f.data, `"v1"`, true, 0)
	defer slow.srv.Close()
	fast := newSwSrc(f.data, `"v1"`, false, 0)
	defer fast.srv.Close()

	out, plan := f.stream(t, slow, fast, f.sha)
	if len(out) != len(f.data) {
		t.Fatalf("收到 %d 字节，期望 %d", len(out), len(f.data))
	}
	if hex.EncodeToString(func() []byte { h := sha256.Sum256(out); return h[:] }()) != f.sha {
		t.Fatal("换源后内容不一致")
	}
	if plan.Upstream.String() != fast.srv.URL {
		t.Fatalf("换源后 Upstream=%s，应指向实际供数的新源 %s", plan.Upstream, fast.srv.URL)
	}
}

// 验证器不一致（同长不同版本）时必须拒绝换源，整份仍由原源提供。
func TestStreamSwitchRejectsVersionChange(t *testing.T) {
	f := newSwFixture(t)
	slow := newSwSrc(f.data, `"v1"`, true, 0)
	defer slow.srv.Close()
	fast := newSwSrc(f.data, `"v2"`, false, 0)
	defer fast.srv.Close()

	out, plan := f.stream(t, slow, fast, f.sha)
	if plan.Upstream.String() != slow.srv.URL {
		t.Fatalf("版本不一致时不该换源，Upstream=%s", plan.Upstream)
	}
	if len(out) != len(f.data) {
		t.Fatalf("收到 %d 字节，期望 %d", len(out), len(f.data))
	}
}

// 靠摘要兜底换源后断流：不能留下无法证明版本的前缀。
func TestStreamSwitchUnverifiedDropPartial(t *testing.T) {
	f := newSwFixture(t)
	slow := newSwSrc(f.data, "", true, 0)
	defer slow.srv.Close()
	// 快源发 256KB 就断：此时已经接管，但不是整份。
	fast := newSwSrc(f.data, "", false, 256<<10)
	defer fast.srv.Close()

	out, _ := f.stream(t, slow, fast, f.sha)
	if len(out) >= len(f.data) {
		t.Fatalf("应该在断流处停下，实际收到 %d 字节", len(out))
	}
	key := "docker:blob:sha256:" + f.sha
	if m, ok := f.cs.ReadPartial(key); ok {
		t.Fatalf("未验证版本的换源断流不该留续传点: %+v", m)
	}
}

// 强 ETag 一致、换源已通过版本验证，但随后断流：策略 A 仍然丢弃 .part，
// 不留混合来源的续传点。
func TestStreamSwitchDropsPartialAfterVerifiedSwitch(t *testing.T) {
	f := newSwFixture(t)
	slow := newSwSrc(f.data, `"v1"`, true, 0)
	defer slow.srv.Close()
	fast := newSwSrc(f.data, `"v1"`, false, 256<<10) // 换源后只再发 256KB 就断
	defer fast.srv.Close()

	out, _ := f.stream(t, slow, fast, f.sha)
	if len(out) >= len(f.data) {
		t.Fatalf("应该在断流处停下，实际收到 %d 字节", len(out))
	}
	key := "docker:blob:sha256:" + f.sha
	if m, ok := f.cs.ReadPartial(key); ok {
		t.Fatalf("换源后断流不该留续传点: %+v", m)
	}
	part, meta := f.cs.PartialPaths(key)
	for _, p := range []string{part, meta} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s 仍存在", p)
		}
	}
}

// 新源忽略 Range 返回 200，且首块大于已发送偏移：多出的部分必须写出，
// 否则客户端少一截，长度与 SHA256 都对不上。
func TestStreamSwitch200FirstBlockBeyondSent(t *testing.T) {
	f := newSwFixture(t)
	// 老源只发 512 字节就卡住，判慢时 sent 很小，换源首块必然大于它。
	slow := newSwSrcFull(f.data, `"v1"`, false, 0, 512, false)
	defer slow.srv.Close()
	// 新源无视 Range，从 0 返回 200 全量。
	fast := newSwSrcFull(f.data, `"v1"`, false, 0, 0, true)
	defer fast.srv.Close()

	out, plan := f.stream(t, slow, fast, f.sha)
	if len(out) != len(f.data) {
		t.Fatalf("响应长度 %d，期望 %d（少 %d 字节）", len(out), len(f.data), len(f.data)-len(out))
	}
	sum := sha256.Sum256(out)
	if hex.EncodeToString(sum[:]) != f.sha {
		t.Fatalf("200 换源后内容 SHA256 不一致")
	}
	if plan.Upstream.String() != fast.srv.URL {
		t.Fatalf("plan.Upstream 未更新为新源: %s", plan.Upstream)
	}
}

// 两边都没有强验证器：Docker blob 靠 ExpectSHA 兜底仍应换源成功，
// 并且最终整份 SHA256 必须校验通过、缓存提交。
func TestStreamSwitchByDigestOnly(t *testing.T) {
	f := newSwFixture(t)
	slow := newSwSrc(f.data, "", true, 0) // 无 ETag
	defer slow.srv.Close()
	fast := newSwSrc(f.data, "", false, 0) // 无 ETag，但同源同内容
	defer fast.srv.Close()

	out, plan := f.stream(t, slow, fast, f.sha)
	if len(out) != len(f.data) {
		t.Fatalf("响应长度 %d，期望 %d", len(out), len(f.data))
	}
	sum := sha256.Sum256(out)
	if hex.EncodeToString(sum[:]) != f.sha {
		t.Fatalf("摘要兜底换源后内容不一致")
	}
	if plan.Upstream.String() != fast.srv.URL {
		t.Fatalf("plan.Upstream 未更新为新源: %s", plan.Upstream)
	}
}
