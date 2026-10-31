// Package raw 实现 GitHub Raw 的 URL Prefix 适配。
//
//	/raw/<owner>/<repo>/<ref>/<path...>
//	  → https://raw.githubusercontent.com/<owner>/<repo>/<ref>/<path...>
package raw

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
)

const Prefix = "/raw/"

var commitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

type Adapter struct {
	base *url.URL
	TTL  time.Duration // 可变资源（分支/标签）的重新验证间隔
}

func New(base string) *Adapter {
	if base == "" {
		base = "https://raw.githubusercontent.com"
	}
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || u.Host == "" {
		u = &url.URL{Scheme: "https", Host: "raw.githubusercontent.com"}
	}
	return &Adapter{base: u, TTL: 5 * time.Minute}
}

func (a *Adapter) Name() string { return "raw" }

func (a *Adapter) Match(p string) bool { return strings.HasPrefix(p, Prefix) }

func (a *Adapter) Plan(r *http.Request) (*adapter.Plan, error) {
	rest := strings.TrimPrefix(r.URL.EscapedPath(), Prefix)
	if rest == "" {
		return nil, adapter.ErrBadPath
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 3 {
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

	owner, repo, ref := segs[0], segs[1], segs[2]
	tail := segs[3:]

	up := *a.base
	up.Path = "/" + strings.Join(segs, "/")
	up.RawQuery = r.URL.RawQuery

	key := "raw:" + owner + "/" + repo + "/" + ref
	if len(tail) > 0 {
		key += "/" + strings.Join(tail, "/")
	}
	if r.URL.RawQuery != "" {
		key += "?" + r.URL.RawQuery
	}

	return &adapter.Plan{
		Kind:      "raw",
		Upstream:  &up,
		Method:    http.MethodGet,
		Header:    http.Header{},
		Redirects: 3,
		CacheKey:  key,
		Cacheable: true,
		// 40 位 commit 的内容永远不变，命中后不用回源；分支和标签会动。
		Immutable: commitRE.MatchString(ref),
		TTL:       a.TTL,
		RangeOK:   true,
	}, nil
}
