package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 「此刻不通」不等于「不支持这个协议」：把 429/5xx/超时 当成结论，
// 源就会被永久标记成 CapNo，之后再也不会被探测——网络恢复了也回不来。
func TestTransientFailuresAreNotConclusions(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		header map[string]string
		want   string
	}{
		{"404 明确没有", http.StatusNotFound, "not found", nil, VerdictNo},
		{"410 明确下线", http.StatusGone, "gone", nil, VerdictNo},
		{"429 限流", http.StatusTooManyRequests, "slow down", nil, VerdictUnknown},
		{"500 上游坏了", http.StatusInternalServerError, "boom", nil, VerdictUnknown},
		{"502 网关坏了", http.StatusBadGateway, "bad gateway", nil, VerdictUnknown},
		{"503 维护中", http.StatusServiceUnavailable, "later", nil, VerdictUnknown},
		{"200 但返回网页", http.StatusOK, "<html>portal</html>", map[string]string{"Content-Type": "text/html"}, VerdictNo},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range c.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()

			got := GitHub(context.Background(), srv.URL, 2, "").Verdict
			if got != c.want {
				t.Fatalf("GitHub verdict = %q，期望 %q", got, c.want)
			}
		})
	}
}

// 连不上（DNS 失败/连接被拒）同样只是「此刻不通」。
func TestUnreachableIsNotAConclusion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // 端口已经关了

	if got := GitHub(context.Background(), url, 2, "").Verdict; got != VerdictUnknown {
		t.Fatalf("连不上不该下结论，得到 %q", got)
	}
	if got := Docker(context.Background(), url, 2, "").Verdict; got != VerdictUnknown {
		t.Fatalf("连不上不该下结论，得到 %q", got)
	}
}

// registry 的 /v2/ 404 才是「这里没有 registry」。
func TestDockerV2NotFoundIsConclusion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	if got := Docker(context.Background(), srv.URL, 2, "").Verdict; got != VerdictNo {
		t.Fatalf("verdict = %q，期望 no", got)
	}
}

func TestDockerRateLimitedIsNotConclusion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	if got := Docker(context.Background(), srv.URL, 2, "").Verdict; got != VerdictUnknown {
		t.Fatalf("verdict = %q，期望空", got)
	}
}
