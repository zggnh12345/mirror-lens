package gateway

// 3.5.23 F4.3 候选资格回归测试（T1–T9）。
//
// 覆盖对应关系：
//   T1/T2 本文件；T3 本文件；T4 本文件；T5 见 watch_switch_test.go
//   「200 长度一致仍拒绝」与 switch_flow_test.go「200 长度一致同版本仍拒绝」；
//   T6 本文件；T7 本文件；T8 见 stream_switch_test.go TestStreamSwitchToVerifiedSource；
//   T9 见 switch_dynamic_test.go TestPeerExclusionRecomputesAfterFailedCandidate。
//
// 这些测试验证的是 switchCandidates 的 RangeCap==1 资格门（fail-closed）
// 及其对 peerMedianBps / trySwitchWithSpeed 的一致性，不放宽生产过滤条件。

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
)

const eligTotal = int64(1 << 20)
const eligSent = int64(256 << 10)

// eligSrv 造一个返回 206 的合格候选服务器，并记录被请求次数。
func eligSrv(t *testing.T, hits *int64) *httptest.Server {
	t.Helper()
	body := bytes.Repeat([]byte("z"), int(eligTotal-eligSent))
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(hits, 1)
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", eligSent, eligTotal-1, eligTotal))
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body)
	}))
}

// eligGateway 用固定候选列表与固定速度样本构造 Gateway。
func eligGateway(cands []fetchTarget, speeds map[int64]int64) *Gateway {
	return &Gateway{
		cli: &http.Client{Timeout: 3 * time.Second},
		par: ParOptions{SwitchFloorMiBPerSec: 1},
		parTargets: func(*adapter.Plan) []fetchTarget { return cands },
		speedProbe: func(id int64, _ string, _ int) []int64 {
			if v, ok := speeds[id]; ok {
				return []int64{v, v, v}
			}
			return nil
		},
	}
}

func eligPlan() *adapter.Plan { return &adapter.Plan{Kind: "docker"} }

func eligCur() *fetchTarget { return &fetchTarget{c: &scheduler.Candidate{ID: 9, Name: "CUR"}} }

func eligCurHdr() http.Header { return hdr("ETag", `"v1"`) }

// T1：候选 RangeCap=0（不支持 Range）不参与中途接管。
func TestEligibilityT1RangeCapZeroNotTakenOver(t *testing.T) {
	var hits int64
	srv := eligSrv(t, &hits)
	defer srv.Close()
	u, _ := url.Parse(srv.URL + "/blob")
	g := eligGateway(
		[]fetchTarget{{u: u, c: &scheduler.Candidate{ID: 2, Name: "no-range", Preferred: true, RangeCap: 0}}},
		map[int64]int64{2: 64 << 10},
	)
	_, _, _, _, err := g.trySwitchWithSpeed(context.Background(), eligPlan(), eligCur(), eligCurHdr(), eligSent, eligTotal, 0, map[int64]bool{})
	if err == nil {
		t.Fatal("RangeCap=0 候选不应参与接管，期望返回无可换源错误")
	}
	if atomic.LoadInt64(&hits) != 0 {
		t.Fatalf("RangeCap=0 候选不应被请求，实际命中 %d 次", hits)
	}
}

// T2：候选 RangeCap=-1（未检测）同样 fail-closed，不参与中途接管。
func TestEligibilityT2RangeCapUnknownNotTakenOver(t *testing.T) {
	var hits int64
	srv := eligSrv(t, &hits)
	defer srv.Close()
	u, _ := url.Parse(srv.URL + "/blob")
	g := eligGateway(
		[]fetchTarget{{u: u, c: &scheduler.Candidate{ID: 2, Name: "unknown", Preferred: true, RangeCap: -1}}},
		map[int64]int64{2: 64 << 10},
	)
	_, _, _, _, err := g.trySwitchWithSpeed(context.Background(), eligPlan(), eligCur(), eligCurHdr(), eligSent, eligTotal, 0, map[int64]bool{})
	if err == nil {
		t.Fatal("RangeCap=-1 候选不应参与接管，期望返回无可换源错误")
	}
	if atomic.LoadInt64(&hits) != 0 {
		t.Fatalf("RangeCap=-1 候选不应被请求，实际命中 %d 次", hits)
	}
}

// T3：不合格候选历史速度再高也不得进入 peerMedianBps，不得抬高门槛。
// 合格 cand=1000b/ms、不合格 badpeer=12914b/ms：若 badpeer 参与，
// 中位数会变成 (1000+12914)/2*1000≈6.957M；正确结果应只有合格的 1.0M。
func TestEligibilityT3BadPeerExcludedFromMedian(t *testing.T) {
	g := eligGateway(
		[]fetchTarget{
			{u: nil, c: &scheduler.Candidate{ID: 2, Name: "cand", Preferred: true, RangeCap: 1}},
			{u: nil, c: &scheduler.Candidate{ID: 3, Name: "badpeer", Preferred: true, RangeCap: 0}},
		},
		map[int64]int64{2: 1000, 3: 12914},
	)
	// excludeID=9（不在候选内），使两台都成为同伴；badpeer 应被资格门挡掉。
	p := g.peerMedianBps(eligPlan(), nil, 9, nil)
	if want := int64(1000 * 1000); p != want {
		t.Fatalf("P_i 应只含合格候选 = %d B/s，实际 %d（不合格高速同伴疑似混入）", want, p)
	}
}

// T4：排除不合格候选后，合格候选达到门槛应正常接管。
// 不合格 bad=12914b/ms（高速但 RangeCap=0），合格 good=3867b/ms。
func TestEligibilityT4QualifiedCandidateTakenOver(t *testing.T) {
	var badHits, goodHits int64
	badSrv := eligSrv(t, &badHits)
	defer badSrv.Close()
	goodSrv := eligSrv(t, &goodHits)
	defer goodSrv.Close()
	badU, _ := url.Parse(badSrv.URL + "/blob")
	goodU, _ := url.Parse(goodSrv.URL + "/blob")
	g := eligGateway(
		[]fetchTarget{
			{u: badU, c: &scheduler.Candidate{ID: 1, Name: "bad", Preferred: true, RangeCap: 0}},
			{u: goodU, c: &scheduler.Candidate{ID: 2, Name: "good", Preferred: true, RangeCap: 1}},
		},
		map[int64]int64{1: 12914, 2: 3867},
	)
	resp, tgt, _, verified, err := g.trySwitchWithSpeed(context.Background(), eligPlan(), eligCur(), eligCurHdr(), eligSent, eligTotal, 100*1024, map[int64]bool{})
	if err != nil {
		t.Fatalf("合格候选应能接管: %v", err)
	}
	if resp != nil {
		defer resp.Body.Close()
	}
	if tgt == nil || tgt.c.ID != 2 {
		t.Fatalf("应由合格候选 good(ID=2) 接管，实际 tgt=%v", tgt)
	}
	if !verified {
		t.Fatal("验证器一致时应 verified=true")
	}
	if atomic.LoadInt64(&goodHits) != 1 || atomic.LoadInt64(&badHits) != 0 {
		t.Fatalf("应只请求合格候选：good=%d bad=%d", goodHits, badHits)
	}
}

// T6：当前源以 HTTP 200 下载（初始源），候选返回 HTTP 206 仍允许接管。
// F3 只拒绝「候选返回 200」，不限制当前源的初始下载协议。
func TestEligibilityT6Current200Candidate206Allowed(t *testing.T) {
	var hits int64
	srv := eligSrv(t, &hits)
	defer srv.Close()
	u, _ := url.Parse(srv.URL + "/blob")
	g := eligGateway(
		[]fetchTarget{{u: u, c: &scheduler.Candidate{ID: 2, Name: "range-ok", Preferred: true, RangeCap: 1}}},
		map[int64]int64{2: 3867},
	)
	resp, tgt, _, verified, err := g.trySwitchWithSpeed(context.Background(), eligPlan(), eligCur(), eligCurHdr(), eligSent, eligTotal, 0, map[int64]bool{})
	if err != nil {
		t.Fatalf("候选 206 应允许接管（当前源初始 200 不受影响）: %v", err)
	}
	if resp != nil {
		defer resp.Body.Close()
	}
	if tgt == nil || tgt.c.ID != 2 || !verified {
		t.Fatalf("候选 206 应成功接管并验证: tgt=%v verified=%v", tgt, verified)
	}
}

// T7：所有候选都不满足 RangeCap==1 时返回 ok=false，当前流继续（不接管）。
func TestEligibilityT7NoQualifiedCandidateKeepsCurrent(t *testing.T) {
	var h0, h1 int64
	s0 := eligSrv(t, &h0)
	defer s0.Close()
	s1 := eligSrv(t, &h1)
	defer s1.Close()
	u0, _ := url.Parse(s0.URL + "/blob")
	u1, _ := url.Parse(s1.URL + "/blob")
	g := eligGateway(
		[]fetchTarget{
			{u: u0, c: &scheduler.Candidate{ID: 1, Name: "zero", Preferred: true, RangeCap: 0}},
			{u: u1, c: &scheduler.Candidate{ID: 2, Name: "unknown", Preferred: true, RangeCap: -1}},
		},
		map[int64]int64{1: 64 << 10, 2: 64 << 10},
	)
	resp, tgt, _, _, err := g.trySwitchWithSpeed(context.Background(), eligPlan(), eligCur(), eligCurHdr(), eligSent, eligTotal, 0, map[int64]bool{})
	if err == nil {
		t.Fatal("全部候选不合格时应返回 ok=false（无可换源错误）")
	}
	if resp != nil || tgt != nil {
		t.Fatalf("不应产生接管响应/目标: resp=%v tgt=%v", resp, tgt)
	}
	if atomic.LoadInt64(&h0) != 0 || atomic.LoadInt64(&h1) != 0 {
		t.Fatalf("不合格候选不应被请求: h0=%d h1=%d", h0, h1)
	}
}
