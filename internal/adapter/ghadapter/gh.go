// Package ghadapter 实现 GitHub Release 与源码包的 URL Prefix 适配。
//
//	/gh/<owner>/<repo>/releases/download/<tag>/<file> → release 资产
//	/gh/<owner>/<repo>/releases/latest/download/<file> → 同上，tag 取 latest
//	/gh/<owner>/<repo>/archive/refs/tags/<tag>.tar.gz → 源码包
//	/gh/<owner>/<repo>/archive/refs/heads/<branch>.zip → 分支快照
//	/gh/<owner>/<repo>/archive/<ref>.tar.gz → 同上，ref 写短形式
//
// 不用 /http/ 是因为它只原样转发、不认镜像源，而 release 资产最需要多源。
// 这里翻译成 kind=release，镜像源才能按「base + 完整 URL」接上去。
package ghadapter

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
)

const Prefix = "/gh/"

type Adapter struct {
	// 可变资源的重新验证间隔
	ReleaseTTL time.Duration
	ArchiveTTL time.Duration
}

func New() *Adapter {
	return &Adapter{ReleaseTTL: 24 * time.Hour, ArchiveTTL: time.Hour}
}

func (a *Adapter) Name() string { return "gh" }

func (a *Adapter) Match(p string) bool { return strings.HasPrefix(p, Prefix) }

func (a *Adapter) Plan(r *http.Request) (*adapter.Plan, error) {
	rest := strings.TrimPrefix(r.URL.EscapedPath(), Prefix)
	if rest == "" {
		return nil, adapter.ErrBadPath
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 4 {
		return nil, adapter.ErrBadPath
	}
	segs := make([]string, len(parts))
	for i, p := range parts {
		s, err := url.PathUnescape(p)
		if err != nil {
			return nil, adapter.ErrBadPath
		}
		if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "\x00\r\n\\") {
			return nil, adapter.ErrBadPath
		}
		segs[i] = s
	}

	owner, repo, kind := segs[0], segs[1], segs[2]
	var upstreamPath string
	var ttl time.Duration
	switch kind {
	case "releases":
		// releases/download/<tag>/<file> 或 releases/latest/download/<file>
		switch {
		case len(segs) >= 6 && segs[3] == "download" && segs[4] != "":
			upstreamPath = strings.Join(segs, "/")
			ttl = a.ReleaseTTL
		case len(segs) >= 6 && segs[3] == "latest" && segs[4] == "download":
			upstreamPath = strings.Join(segs, "/")
			ttl = a.ReleaseTTL
		default:
			return nil, adapter.ErrBadPath
		}
	case "archive":
		// archive/<ref>.zip | archive/refs/{tags,heads}/<ref>.tar.gz
		last := segs[len(segs)-1]
		if !strings.HasSuffix(last, ".zip") && !strings.HasSuffix(last, ".tar.gz") && !strings.HasSuffix(last, ".tar.bz2") {
			return nil, adapter.ErrBadPath
		}
		upstreamPath = strings.Join(segs, "/")
		ttl = a.ArchiveTTL
	default:
		return nil, adapter.ErrBadPath
	}

	up := &url.URL{Scheme: "https", Host: "github.com", Path: "/" + upstreamPath}

	// 缓存身份按 owner/repo/路径拼，不用上游 URL：
	// 镜像源换域名不影响命中，官方源与镜像源写同一份。
	key := "gh:" + strings.ToLower(owner) + "/" + strings.ToLower(repo) + "/" + strings.Join(segs[2:], "/")

	return &adapter.Plan{
		Kind:      "release",
		Upstream:  up,
		Method:    http.MethodGet,
		Header:    http.Header{},
		Redirects: 5, // release 资产会 302 到 objects.githubusercontent.com
		CacheKey:  key,
		Cacheable: true,
		// 不设 Immutable：release 可以删掉再传同名文件，标签也能被强推。
		// 代价是每天回源验证一次，换来不会把旧内容当成新内容发出去。
		TTL:     ttl,
		RangeOK: true,
		Stream:  true,
	}, nil
}
