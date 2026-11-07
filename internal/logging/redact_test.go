package logging

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestScrubURLsDropsQueryAndUserinfo(t *testing.T) {
	cases := []string{
		`Get "https://user:pass@host/path?token=abc&X-Amz-Signature=def": dial tcp: timeout`,
		`回源失败 http://10.1.1.5:8080/relay?token=secret#frag`,
		`http:host/path?access_token=1&sig=2`,
	}
	for _, in := range cases {
		out := ScrubURLs(in)
		for _, bad := range []string{"token=", "X-Amz-Signature", "access_token", "sig=2", "user:pass", "frag"} {
			if strings.Contains(out, bad) {
				t.Fatalf("清洗后仍含 %q：%s", bad, out)
			}
		}
		if strings.Contains(out, "?") {
			t.Fatalf("清洗后仍保留查询串：%s", out)
		}
	}
}

func TestScrubURLsKeepsUsableSkeleton(t *testing.T) {
	in := `Get "https://gh.example.com/repo/archive.zip?token=x": EOF`
	out := ScrubURLs(in)
	if !strings.Contains(out, "gh.example.com") || !strings.Contains(out, "/repo/archive.zip") {
		t.Fatalf("清洗后应保留 host 和 path，实际 %q", out)
	}
	if !strings.Contains(out, "EOF") {
		t.Fatalf("清洗后应保留原始错误描述，实际 %q", out)
	}
}

func TestScrubURLsLeavesPlainErrorAlone(t *testing.T) {
	in := "write /data/cache/x.part: no space left on device"
	if out := ScrubURLs(in); out != in {
		t.Fatalf("无 URL 的文本不该被改动：%q", out)
	}
}

// SafeErr 只改输出文本，不破坏错误链。
func TestSafeErrPreservesChain(t *testing.T) {
	inner := &url.Error{Op: "Get", URL: "https://h/p?token=t", Err: errors.New("boom")}
	err := fmt.Errorf("回源失败: %w", inner)

	var ue *url.Error
	if !errors.As(SafeErr(err), &ue) {
		t.Fatal("SafeErr 后 errors.As 仍应能取到 *url.Error")
	}
	if strings.Contains(SafeErr(err).Error(), "token=t") {
		t.Fatalf("SafeErr 输出仍含 token：%s", SafeErr(err).Error())
	}
	if !errors.Is(SafeErr(err), inner) {
		t.Fatal("SafeErr 后 errors.Is 应仍然成立")
	}
	if SafeErr(nil) != nil {
		t.Fatal("nil 错误应原样返回 nil")
	}
}
