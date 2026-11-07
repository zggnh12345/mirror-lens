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
