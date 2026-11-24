package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
)

// trySwitch 的 HTTP 层边界：同长不同内容、总长不同、验证器缺失/不一致都必须拒绝。
func TestTrySwitchVersionBoundary(t *testing.T) {
	const total = int64(1 << 20)
	sent := int64(256 << 10)
	body := make([]byte, total)
	for i := range body {
		body[i] = byte(i * 3)
	}

	cases := []struct {
		name string
		cur  http.Header // 旧响应头，nil 时默认 ETag v1
		// 新源响应
		status   int
		etag     string
		lm       string
		crTotal  int64
		crEnd    int64
		bodyFrom int64
		noCL     bool
		expect   string
		wantOK   bool
		wantVer  bool
	}{
		{name: "206 同版本", status: 206, etag: `"v1"`, crTotal: total, bodyFrom: sent, wantOK: true, wantVer: true},
		{name: "206 不同版本", status: 206, etag: `"v2"`, crTotal: total, bodyFrom: sent},
		{name: "206 总长不同", status: 206, etag: `"v1"`, crTotal: total + 1, bodyFrom: sent},
		{name: "206 无验证器无摘要", cur: hdr(), status: 206, crTotal: total, bodyFrom: sent},
		{name: "206 无验证器有摘要", cur: hdr(), status: 206, crTotal: total, bodyFrom: sent, expect: "deadbeef", wantOK: true},
		{name: "206 只有新源有 Last-Modified", status: 206, lm: "Wed, 01 Jan 2026 00:00:00 GMT", crTotal: total, bodyFrom: sent},
		// P0 反例：开放式 Range 只回到 total-2，且 chunked 无 Content-Length。
		// 即使 HTTP 层合法，也不能接管，否则尾部永远缺一个字节。
		{name: "206 短 Range 无 CL", status: 206, etag: `"v1"`, crTotal: total, crEnd: total - 2, bodyFrom: sent, noCL: true},
		// 3.5.23 F3：候选 200 即使长度一致也拒绝接管（旧行为放行）。
		{name: "200 长度一致同版本仍拒绝", status: 200, etag: `"v1"`, crTotal: total, bodyFrom: 0},
		{name: "200 长度不同", status: 200, etag: `"v1"`, crTotal: total + 1, bodyFrom: 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.etag != "" {
					w.Header().Set("ETag", c.etag)
				}
				if c.lm != "" {
					w.Header().Set("Last-Modified", c.lm)
				}
				end := c.crEnd
				if end == 0 {
					end = c.crTotal - 1
				}
				if c.status == 206 {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", c.bodyFrom, end, c.crTotal))
				}
				var payload []byte
				if c.status == 200 {
					payload = body[c.bodyFrom:]
					if c.crTotal != total {
						// 200 的「长度不同」必须真的多给一个字节，否则测不到。
						payload = append(append([]byte{}, payload...), 0)
					}
				} else {
					hi := end + 1
					if hi > int64(len(body)) {
						hi = int64(len(body))
					}
					payload = body[c.bodyFrom:hi]
				}
				if !c.noCL {
					w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
				}
				w.WriteHeader(c.status)
				w.Write(payload)
			}))
			defer srv.Close()

			u, _ := url.Parse(srv.URL)
			const sid = int64(2)
			g := &Gateway{
				cli: srv.Client(),
				par: ParOptions{SwitchFloorMiBPerSec: 1},
				// 造一个够快的样本，否则 trySwitch 直接跳过这台源。
				speedProbe: func(id int64, kind string, n int) []int64 { return []int64{128 << 10, 120 << 10, 140 << 10} },
				parTargets: func(*adapter.Plan) []fetchTarget {
					return []fetchTarget{{u: u, c: &scheduler.Candidate{ID: sid, Name: "b", Preferred: true, RangeCap: 1}}}
				},
			}

			plan := &adapter.Plan{Kind: "docker", Upstream: u, ExpectSHA: c.expect}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			curH := c.cur
			if curH == nil {
				curH = hdr("ETag", `"v1"`)
			}
			resp, _, _, verified, serr := g.trySwitch(ctx, plan, nil, curH, sent, total, map[int64]bool{})
			if resp != nil {
				resp.Body.Close()
			}
			if (serr == nil) != c.wantOK {
				t.Fatalf("err=%v，期望 ok=%v", serr, c.wantOK)
			}
			if c.wantOK && verified != c.wantVer {
				t.Fatalf("verified=%v，期望 %v", verified, c.wantVer)
			}
		})
	}
}
