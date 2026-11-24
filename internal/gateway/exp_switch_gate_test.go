package gateway

// 3.5.22 第一阶段受控实验（纯本地，不碰生产、不改产品代码）。
//
// 目的：
//  1. 当前源 100KB/s、候选 1.7MB/s、门槛 F=1MB/s → 是否触发换源；
//  2. 当前源 2.5MB/s、候选 1.7MB/s、门槛 F=1MB/s → 是否误换更慢源。
//
// 说明：现有代码的换源资格只看「候选样本中位数 >= SwitchMinMBps」，
// 不看当前源实时速度 C（文档中的 C×1.2 尚未实现）。本实验用现有代码跑，
// 就是为了实测「裸降门槛」的两个后果，作为是否实现动态门槛的依据。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// expSrc 按指定字节速率供数（chunk=32KiB，按目标速率 sleep）。
type expSrc struct {
	srv  *httptest.Server
	rate int64 // bytes/s
	etag string
}

func newExpSrc(data []byte, etag string, rate int64) *expSrc {
	s := &expSrc{rate: rate, etag: etag}
	chunk := int64(32 << 10)
	gap := time.Duration(0)
	if rate > 0 {
		gap = time.Duration(float64(chunk) / float64(rate) * float64(time.Second))
	}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		from, to := int64(0), int64(len(data))-1
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &from, &to); err != nil {
			// 开放区间 bytes=N-：重新解析 from（不要清零，上面可能已赋对值）
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
		if etag != "" {
			w.Header().Set("ETag", etag)
		}
		w.WriteHeader(http.StatusPartialContent)
		fl, _ := w.(http.Flusher)
		for off := from; off <= to; {
			n := chunk
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
			time.Sleep(gap)
		}
	}))
	return s
}

type expFixture struct {
	g    *Gateway
	cs   *cache.Store
	data []byte
	sha  string
}

func newExpFixture(t *testing.T, watchMBps int64, switchMinMBps int64, total int) *expFixture {
	t.Helper()
	data := make([]byte, total)
	for i := range data {
		data[i] = byte(i*7 + i>>8)
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
	return &expFixture{
		g: &Gateway{
			st: st, cache: cs, cli: http.DefaultClient, now: time.Now, maxSize: 1 << 40,
			par: ParOptions{
				Enabled:              false, // 只测单流换源
				SwitchFloorMiBPerSec: switchMinMBps, WatchWindow: time.Second,
				WatchMBps: watchMBps, SwitchMinFileMiB: 1, SwitchMaxSentMiB: 48,
				SwitchMax: 2,
			},
		},
		cs: cs, data: data, sha: hex.EncodeToString(sum[:]),
	}
}

// run 跑一次下载，返回是否换到了候选源。
func (f *expFixture) run(t *testing.T, curRate, candSampleBps int64) bool {
	t.Helper()
	cur := newExpSrc(f.data, `"v1"`, curRate)
	defer cur.srv.Close()
	cand := newExpSrc(f.data, `"v1"`, 0) // 候选实际供数不限速，样本另注入
	defer cand.srv.Close()

	uCur, _ := url.Parse(cur.srv.URL)
	uCand, _ := url.Parse(cand.srv.URL)
	g := f.g
	g.cli = cur.srv.Client()
	g.speedProbe = func(id int64, kind string, n int) []int64 {
		if id == 2 {
			v := candSampleBps
			return []int64{v, v, v} // 三条一致，中位数 = v
		}
		return nil
	}
	g.parTargets = func(*adapter.Plan) []fetchTarget {
		return []fetchTarget{
			{u: uCur, c: &scheduler.Candidate{ID: 1, Name: "cur"}},
			{u: uCand, c: &scheduler.Candidate{ID: 2, Name: "cand", Preferred: true}},
		}
	}
	key := "docker:blob:sha256:" + f.sha
	_ = f.cs.ResetPartial(key)
	plan := &adapter.Plan{
		Upstream: uCur, CacheKey: key, ExpectSHA: f.sha,
		Stream: true, Cacheable: true, Kind: "docker", TTL: time.Hour,
	}
	req := httptest.NewRequest(http.MethodGet, "/v2/x/blobs/sha256:"+f.sha, nil)
	ureq, _ := http.NewRequestWithContext(req.Context(), http.MethodGet, cur.srv.URL+"/v2/x/blobs/sha256:"+f.sha, nil)
	ureq.Header.Set("Range", "bytes=0-")
	resp, err := g.cli.Do(ureq)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	g.streamBody(rec, req, plan, resp, nil, &fetchTarget{u: uCur}, 0)
	// 本实验只观测「是否换源」，不校验内容完整性——那是换源交接逻辑的
	// 另一个测试职责；限速源的时间抖动会让边界差几个 chunk，与本实验无关。
	return plan.Upstream.String() == cand.srv.URL
}

var _ = time.Second // 保留 time import（newExpSrc 用）

// 实验1：当前 100KB/s，候选样本 1.7MB/s，门槛 1MB/s。
// WatchMBps=0 → 兜底 3MB/s 判慢线，100KB/s 远低于它，会进入换源评估。
func TestExp1_SlowCurrentFastCandidate(t *testing.T) {
	f := newExpFixture(t, 0, 1, 2<<20)
	switched := f.run(t, 100<<10, 1700)
	t.Logf("EXP1 当前=100KB/s 候选样本=1.7MB/s 门槛=1MB/s → 换源=%v", switched)
}

// 实验2：当前 2.5MB/s，候选样本 1.7MB/s，门槛 1MB/s。
// 判慢线设 3MB/s（低于 3 才判慢），让 2.5MB/s 进入换源评估。
func TestExp2_FastCurrentSlowerCandidate(t *testing.T) {
	f := newExpFixture(t, 3, 1, 16<<20)
	switched := f.run(t, 2500<<10, 1700)
	t.Logf("EXP2 当前=2.5MB/s 候选样本=1.7MB/s 门槛=1MB/s → 换源=%v（true 表示误换更慢源）", switched)
}

// 对照组：候选样本 0.9MB/s（低于门槛 1MB/s），当前 100KB/s。
func TestExp3_CandidateBelowFloor(t *testing.T) {
	f := newExpFixture(t, 0, 1, 2<<20)
	switched := f.run(t, 100<<10, 900)
	t.Logf("EXP3 当前=100KB/s 候选样本=0.9MB/s 门槛=1MB/s → 换源=%v（应 false）", switched)
}
