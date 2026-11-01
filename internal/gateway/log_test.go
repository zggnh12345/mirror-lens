package gateway

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/logging"
)

// 日志里不能出现查询串：回源地址的查询参数经常就是签名或 Token。
func TestLoggwRedactsCacheKey(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))

	plan := &adapter.Plan{
		Kind:     "http",
		CacheKey: "http:https://example.com/a.tgz?token=SECRET123&X-Amz-Signature=deadbeef",
	}
	ctx := logging.WithRequestID(context.Background(), "abcdef12")
	loggw(slog.LevelWarn, "回源失败", ctx, plan, logging.UpstreamTimeout, errors.New("timeout"))

	out := buf.String()
	for _, banned := range []string{"SECRET123", "deadbeef", "X-Amz-Signature"} {
		if strings.Contains(out, banned) {
			t.Fatalf("日志里出现了敏感内容 %q:\n%s", banned, out)
		}
	}
	for _, want := range []string{"abcdef12", "UPSTREAM_TIMEOUT", "kind=http"} {
		if !strings.Contains(out, want) {
			t.Fatalf("日志缺 %q:\n%s", want, out)
		}
	}
}

func TestCodeForClassifies(t *testing.T) {
	cases := []struct {
		err  error
		want logging.Code
	}{
		{&upstreamError{Code: 429}, logging.UpstreamRateLimited},
		{&upstreamError{Code: 503}, logging.UpstreamHTTPError},
		{&localError{err: errors.New("disk full")}, logging.CacheCommitFailed},
		{context.DeadlineExceeded, logging.UpstreamTimeout},
		{errors.New("dial tcp: connection refused"), logging.UpstreamConnectFailed},
	}
	for _, c := range cases {
		if got := codeFor(c.err); got != c.want {
			t.Errorf("codeFor(%v)=%s，期望 %s", c.err, got, c.want)
		}
	}
}
