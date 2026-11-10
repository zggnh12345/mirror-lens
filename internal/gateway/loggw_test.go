package gateway

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/logging"
)

// 输出边界的真实测试：带 token 的 URL 无论出现在缓存键还是错误对象里，
// 都不能进日志。
func TestLoggwRedactsSecrets(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	raw := "https://gh.example.com/v2/repo/blobs/sha256:abc?token=up-secret&X-Amz-Signature=sig"
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	plan := &adapter.Plan{
		Kind:     "blob",
		Source:   "relay",
		CacheKey: "http:gh.example.com/v2/repo/blobs/sha256:abc?token=cache-secret",
		Upstream: u,
	}
	uerr := &url.Error{Op: "Get", URL: raw, Err: errors.New("EOF")}

	loggw(slog.LevelError, "回源失败", context.Background(), plan, logging.UpstreamHTTPError, uerr)

	out := buf.String()
	for _, bad := range []string{"cache-secret", "up-secret", "X-Amz-Signature", "sig"} {
		if strings.Contains(out, bad) {
			t.Fatalf("日志仍含敏感值 %q：%s", bad, out)
		}
	}
	if !strings.Contains(out, "gh.example.com") {
		t.Fatalf("日志应保留可定位的 host，实际：%s", out)
	}
	if !strings.Contains(out, "UPSTREAM_HTTP_ERROR") {
		t.Fatalf("日志应保留错误代码，实际：%s", out)
	}
}

// 竞速输掉/换源关掉的那条连接不该以 Warn 出现在日志里，更不能带错误码去聚合。
func TestLoggwDropsAbort(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(old) })

	plan := &adapter.Plan{Kind: "blob", Source: "relay", CacheKey: "http:gh.example.com/v2/x"}
	loggw(slog.LevelWarn, "回源失败", context.Background(), plan, logging.UpstreamConnectFailed, context.Canceled)
	if buf.Len() != 0 {
		t.Fatalf("取消类错误不该进 Warn 日志，实际：%s", buf.String())
	}

	// 真故障照常出日志。
	loggw(slog.LevelWarn, "回源失败", context.Background(), plan, logging.UpstreamConnectFailed, errors.New("dial tcp 1.2.3.4:443: i/o timeout"))
	if !strings.Contains(buf.String(), "i/o timeout") {
		t.Fatalf("真故障被误吞：%s", buf.String())
	}
}
