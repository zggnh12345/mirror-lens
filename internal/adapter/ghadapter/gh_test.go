package ghadapter

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
)

func plan(t *testing.T, path string) *adapter.Plan {
	t.Helper()
	p, err := New().Plan(httptest.NewRequest("GET", path, nil))
	if err != nil {
		t.Fatalf("%s 应能解析: %v", path, err)
	}
	return p
}

func TestReleaseAsset(t *testing.T) {
	p := plan(t, "/gh/octocat/Hello-World/releases/download/v1.0/asset.zip")
	if got := p.Upstream.String(); got != "https://github.com/octocat/Hello-World/releases/download/v1.0/asset.zip" {
		t.Fatalf("上游拼错: %s", got)
	}
	if p.Kind != "release" {
		t.Fatalf("release 资产要走 release 池，实际 %q", p.Kind)
	}
	if p.CacheKey != "gh:octocat/hello-world/releases/download/v1.0/asset.zip" {
		t.Fatalf("缓存键不对: %s", p.CacheKey)
	}
	if !p.Stream || !p.RangeOK {
		t.Fatalf("release 资产要能边下边发和续传")
	}
	if p.Immutable {
		t.Fatalf("不能标不可变：release 删了重传同名文件是允许的")
	}
}

func TestReleaseLatest(t *testing.T) {
	p := plan(t, "/gh/cli/cli/releases/latest/download/gh_linux_amd64.tar.gz")
	if got := p.Upstream.Path; got != "/cli/cli/releases/latest/download/gh_linux_amd64.tar.gz" {
		t.Fatalf("latest 形式拼错: %s", got)
	}
}

func TestArchive(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"/gh/octocat/Hello-World/archive/refs/tags/v1.0.tar.gz", "/octocat/Hello-World/archive/refs/tags/v1.0.tar.gz"},
		{"/gh/octocat/Hello-World/archive/refs/heads/main.zip", "/octocat/Hello-World/archive/refs/heads/main.zip"},
		{"/gh/octocat/Hello-World/archive/main.zip", "/octocat/Hello-World/archive/main.zip"},
	} {
		p := plan(t, c.in)
		if p.Upstream.Path != c.want {
			t.Fatalf("%s → %s，期望 %s", c.in, p.Upstream.Path, c.want)
		}
		if p.Kind != "release" {
			t.Fatalf("源码包也走 release 池，实际 %q", p.Kind)
		}
	}
}

// 缓存键大小写归一：GitHub 的 owner/repo 不分大小写，标签和文件名分。
func TestCacheKeyCase(t *testing.T) {
	a := plan(t, "/gh/Octocat/Hello-World/releases/download/V1.0/Asset.ZIP")
	b := plan(t, "/gh/octocat/hello-world/releases/download/V1.0/Asset.ZIP")
	if a.CacheKey != b.CacheKey {
		t.Fatalf("owner/repo 大小写不同应命中同一份缓存：%s vs %s", a.CacheKey, b.CacheKey)
	}
	c := plan(t, "/gh/octocat/hello-world/releases/download/V1.0/asset.zip")
	if a.CacheKey == c.CacheKey {
		t.Fatalf("标签和文件名区分大小写，不该合并")
	}
}

func TestRejects(t *testing.T) {
	for _, p := range []string{
		"/gh/o/r",                                 // 太短
		"/gh/o/r/releases/tag/v1.0",               // 网页不是文件
		"/gh/o/r/releases/download",               // 缺标签和文件名
		"/gh/o/r/archive/main",                    // 缺扩展名
		"/gh/o/r/archive/main.iso",                // 不支持的格式
		"/gh/o/r/raw/main/a.txt",                  // raw 走 /raw/
		"/gh/../etc/passwd/releases/download/v/f", // 路径穿越
	} {
		if _, err := New().Plan(httptest.NewRequest("GET", p, nil)); !errors.Is(err, adapter.ErrBadPath) {
			t.Fatalf("%s 应被拒绝，实际 %v", p, err)
		}
	}
}
