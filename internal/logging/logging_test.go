package logging

import (
	"bytes"
	"encoding/json"
	"log"
	"log/slog"
	"strings"
	"testing"
)

func TestSetupTextFormat(t *testing.T) {
	var buf bytes.Buffer
	if err := setupTo(Config{Level: "info", Format: "text"}, &buf); err != nil {
		t.Fatal(err)
	}
	slog.Info("upstream request failed", "source_id", 12, "error_code", UpstreamRateLimited)
	out := buf.String()
	for _, want := range []string{"level=INFO", "msg=\"upstream request failed\"", "source_id=12", "error_code=UPSTREAM_RATE_LIMITED"} {
		if !strings.Contains(out, want) {
			t.Fatalf("text 输出缺 %q:\n%s", want, out)
		}
	}
}

func TestSetupJSONFormat(t *testing.T) {
	var buf bytes.Buffer
	if err := setupTo(Config{Format: "json"}, &buf); err != nil {
		t.Fatal(err)
	}
	slog.Warn("upstream request failed", "source_id", 12, "error_code", UpstreamTimeout)

	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("不是合法 JSON: %v\n%s", err, buf.String())
	}
	if m["msg"] != "upstream request failed" || m["level"] != "WARN" {
		t.Fatalf("字段不对: %v", m)
	}
	// JSON 里数字是 float64
	if m["source_id"] != float64(12) {
		t.Fatalf("source_id 丢了: %v", m)
	}
	if m["error_code"] != "UPSTREAM_TIMEOUT" {
		t.Fatalf("error_code 不对: %v", m["error_code"])
	}
}

func TestLevelFilters(t *testing.T) {
	var buf bytes.Buffer
	if err := setupTo(Config{Level: "warn", Format: "text"}, &buf); err != nil {
		t.Fatal(err)
	}
	slog.Info("不该出现")
	slog.Warn("该出现")
	out := buf.String()
	if strings.Contains(out, "不该出现") {
		t.Fatalf("info 级日志不该输出:\n%s", out)
	}
	if !strings.Contains(out, "该出现") {
		t.Fatalf("warn 级日志应输出:\n%s", out)
	}
}

// 没迁移的 log.Printf 也要按同一格式出来，否则日志会两种样式混排。
func TestStdLogTakenOver(t *testing.T) {
	var buf bytes.Buffer
	if err := setupTo(Config{Format: "json"}, &buf); err != nil {
		t.Fatal(err)
	}
	log.Printf("旧代码 %d", 7)

	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("标准库日志没有走 slog: %v\n%s", err, buf.String())
	}
	if m["msg"] != "旧代码 7" {
		t.Fatalf("msg 不对: %v", m["msg"])
	}
}

func TestBadConfig(t *testing.T) {
	if err := setupTo(Config{Level: "verbose"}, &bytes.Buffer{}); err == nil {
		t.Fatal("非法级别应报错")
	}
	if err := setupTo(Config{Format: "xml"}, &bytes.Buffer{}); err == nil {
		t.Fatal("非法格式应报错")
	}
}

func TestSanitizeURL(t *testing.T) {
	cases := []struct{ in, want, banned string }{
		{
			in:     "https://user:pass@example.com/a?token=abc123&x=1",
			want:   "https://example.com/a",
			banned: "abc123",
		},
		{
			in:     "https://example.com/a?X-Amz-Signature=deadbeef&X-Amz-Credential=k",
			want:   "https://example.com/a",
			banned: "deadbeef",
		},
		{
			in:     "https://example.com/a?access_token=zzz",
			want:   "https://example.com/a",
			banned: "zzz",
		},
		{
			in:     "https://example.com/a?page=2",
			want:   "https://example.com/a",
			banned: "page=2",
		},
	}
	for _, c := range cases {
		got := SanitizeURL(c.in)
		if got != c.want {
			t.Errorf("SanitizeURL(%q)=%q，期望 %q", c.in, got, c.want)
		}
		if strings.Contains(got, c.banned) {
			t.Errorf("输出里还有敏感内容 %q: %q", c.banned, got)
		}
	}
	if got := SanitizeURL("://bad url"); got != "<unparsable-url>" {
		t.Errorf("解析失败应返回占位符，得到 %q", got)
	}
	if got := SanitizeURL(""); got != "" {
		t.Errorf("空串应原样返回，得到 %q", got)
	}
}
