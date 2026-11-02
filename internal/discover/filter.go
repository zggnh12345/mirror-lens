package discover

import (
	"regexp"
	"strings"
)

var (
	// URL 里的主机名，允许数字和大写（用前统一转小写），跳过 user:token@ 前缀。
	urlHostRe = regexp.MustCompile(`https?://(?:[^/\s@]+@)?([a-zA-Z0-9](?:[a-zA-Z0-9.-]*[a-zA-Z0-9])?\.[a-zA-Z]{2,})`)
	// 反引号里的裸域名：README 里常见 `dockerproxy.net` 这种写法。
	codeHostRe = regexp.MustCompile("`([a-z0-9][a-z0-9.-]*\\.[a-z]{2,})(?:[/:][^`\\s]*)?`")
)

// 自身或文档站点，不是加速器
var denyHost = map[string]bool{
	"github.com": true, "raw.githubusercontent.com": true, "api.github.com": true,
	"gitee.com": true, "docker.com": true, "docker.io": true, "hub.docker.com": true,
	"registry-1.docker.io": true, "docs.docker.com": true, "img.shields.io": true,
	"badgen.net": true, "shields.io": true, "opensource.org": true, "go.dev": true,
	"golang.org": true, "githubusercontent.com": true, "camo.githubusercontent.com": true,
	"user-images.githubusercontent.com": true, "avatars.githubusercontent.com": true,
	"api.github.io": true, "github.dev": true,
}

// 文档站、代码托管站自身的子域，一律不是加速器
var denySuffix = []string{"github.com", "github.io", "githubusercontent.com", "vercel.app", "jsdelivr.net", "cloudflare.com"}

// 关键词过滤：只留下像镜像加速器的域名，压掉列表里的杂音。
var dockerHints = []string{"docker", "registry", "hub", "ghproxy", "gh-proxy", "ghp", "加速"}

// 包管理器的 registry 也叫 registry，别当 Docker 加速器。
var denyWord = []string{"npm", "pypi", "pythonhosted", "goproxy", "crates", "maven", "gradle",
	"rubygems", "nuget", "debian", "ubuntu", "centos", "aliyun", "tuna", "ustc", "nodejs", "cpan"}

func hostOf(raw string) string {
	h := strings.ToLower(strings.TrimSpace(raw))
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.Index(h, "@"); i >= 0 {
		h = h[i+1:]
	}
	if i := strings.IndexAny(h, "/?#"); i >= 0 {
		h = h[:i]
	}
	if i := strings.Index(h, ":"); i >= 0 {
		h = h[:i]
	}
	return strings.Trim(h, ".")
}

func denied(h string) bool {
	if h == "" || !strings.Contains(h, ".") {
		return true
	}
	if denyHost[h] || strings.HasPrefix(h, "mirrors.") {
		return true
	}
	for _, w := range denyWord {
		if strings.Contains(h, w) {
			return true
		}
	}
	for _, sfx := range denySuffix {
		if h == sfx || strings.HasSuffix(h, "."+sfx) {
			return true
		}
	}
	return false
}

// LooksLikeMirror 判断域名像不像某一类加速器。
func LooksLikeMirror(host, kind string) bool {
	if denied(host) {
		return false
	}
	if kind == KindGitHub {
		return strings.Contains(host, "gh") || strings.Contains(host, "proxy") || strings.Contains(host, "mirror")
	}
	for _, k := range dockerHints {
		if strings.Contains(host, k) {
			return true
		}
	}
	return false
}
