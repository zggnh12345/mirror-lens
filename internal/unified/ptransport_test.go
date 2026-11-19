package unified

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func okResponse() *http.Response {
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader("ok")),
		Header:     http.Header{},
	}
}

// P 计量点：Token/认证重发统一走 t.base，因此都能被计入。
func TestPTransportMetersEveryRoundTrip(t *testing.T) {
	s, err := NewScope(context.Background(), ScopeConfig{PMax: 3}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var calls atomic.Int32
	mt := &PTransport{Base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return okResponse(), nil
	})}
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequestWithContext(WithScope(context.Background(), s), "GET", "https://ghcr.io/v2/a/manifests/latest", nil)
		resp, err := mt.RoundTrip(req)
		if err != nil {
			t.Fatalf("第 %d 次: %v", i+1, err)
		}
		resp.Body.Close()
	}
	req, _ := http.NewRequestWithContext(WithScope(context.Background(), s), "GET", "https://ghcr.io/token", nil)
	if _, err := mt.RoundTrip(req); !IsBudgetExhausted(err) {
		t.Fatalf("第 4 次必须因 P 耗尽被拒，得到 %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("底层派发=%d，want 3（耗尽后不得再发）", calls.Load())
	}
	if s.P().Committed() != 3 {
		t.Fatalf("committed=%d，want 3", s.P().Committed())
	}
}

// 无 Scope 的请求（Git/Raw）不受 P 门控影响。
func TestPTransportPassthroughWithoutScope(t *testing.T) {
	var calls atomic.Int32
	mt := &PTransport{Base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return okResponse(), nil
	})}
	req, _ := http.NewRequest("GET", "https://example.com/x", nil)
	if _, err := mt.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d，want 1", calls.Load())
	}
}

// 预留后立即取消：预留归还，底层不派发。
func TestPTransportCancelledBeforeCommitReleases(t *testing.T) {
	s, err := NewScope(context.Background(), ScopeConfig{PMax: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(WithScope(ctx, s), "GET", "https://ghcr.io/v2/", nil)
	cancel()
	mt := &PTransport{Base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatal("取消后不得派发")
		return nil, errors.New("unreachable")
	})}
	if _, err := mt.RoundTrip(req); err == nil {
		t.Fatal("应返回取消错误")
	}
	if s.P().Outstanding() != 0 {
		t.Fatalf("outstanding=%d，取消必须归还预留", s.P().Outstanding())
	}
	// 额度没被吃掉：新的 Scope 请求仍可派发。
	s2, _ := NewScope(context.Background(), ScopeConfig{PMax: 1}, nil)
	defer s2.Close()
	res, err := s2.ReserveP()
	if err != nil {
		t.Fatalf("额度被吃掉：%v", err)
	}
	res.Commit()
}

// B16：RegistryRetryToken 第二次消费被拒。
func TestT59RetryTokenSingleUse(t *testing.T) {
	tok := NewRetryTokens(1)
	if err := tok.Take("ghcr.io"); err != nil {
		t.Fatal(err)
	}
	if err := tok.Take("ghcr.io"); !errors.Is(err, ErrRetryTokenSpent) {
		t.Fatalf("第二次消费必须被拒，得到 %v", err)
	}
	if err := tok.Take("registry.k8s.io"); err != nil {
		t.Fatalf("不同 Registry 独立记令牌：%v", err)
	}
	if tok.Remaining("ghcr.io") != 0 || tok.Remaining("registry.k8s.io") != 0 {
		t.Fatal("剩余令牌应为 0")
	}
}
