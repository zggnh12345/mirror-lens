// Package gitadapter 实现 Git Smart HTTP 的透明代理。
//
//	/git/<host>/<owner>/<repo>.git → https://<host>/<owner>/<repo>.git
//	/git/http/<host>/<owner>/<repo>.git → 显式 http 回源（默认 https）
//
// 只代理读操作：info/refs 与 upload-pack。receive-pack（push）直接拒绝，
// 镜像站不是代码托管平台，不该替别人保管写权限。
package gitadapter

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
)

const Prefix = "/git/"

// info/refs 的响应体随协议版本不同（v1 是 refs 列表，v2 是能力声明），
// 缓存键必须分开，否则 v2 客户端会拿到 v1 的字节。
const protoHeader = "Git-Protocol"

type Adapter struct {
	TTL time.Duration // info/refs 的缓存时长；0 表示完全不缓存
}

func New(ttl time.Duration) *Adapter { return &Adapter{TTL: ttl} }

func (a *Adapter) Name() string { return "git" }

func (a *Adapter) Match(p string) bool { return strings.HasPrefix(p, Prefix) }

func (a *Adapter) Plan(r *http.Request) (*adapter.Plan, error) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		return nil, fmt.Errorf("%w: Git 只支持 GET/POST", adapter.ErrBadPath)
	}
	enc := strings.TrimPrefix(r.URL.EscapedPath(), Prefix)
	if enc == "" {
		return nil, adapter.ErrBadPath
	}
	raw, err := url.PathUnescape(enc)
	if err != nil {
		return nil, adapter.ErrBadPath
	}
	raw = strings.Trim(raw, "/")
	if raw == "" || strings.Contains(raw, "//") {
		return nil, adapter.ErrBadPath
	}

	scheme := "https"
	parts := strings.SplitN(raw, "/", 3)
	if parts[0] == "http" || parts[0] == "https" {
		if len(parts) < 3 {
			return nil, fmt.Errorf("%w: 写法是 /git/<scheme>/<host>/<repo>", adapter.ErrBadPath)
		}
		scheme, raw = parts[0], parts[1]+"/"+parts[2]
		parts = strings.SplitN(raw, "/", 3)
	}
	host := parts[0]
	if host == "" || strings.ContainsAny(host, "@?#") {
		return nil, fmt.Errorf("%w: 主机名不合法", adapter.ErrBadPath)
	}
	repo := ""
	if len(parts) > 1 {
		repo = parts[1]
	}
	if len(parts) == 3 {
		repo += "/" + parts[2]
	}
	repo = strings.Trim(repo, "/")
	if repo == "" {
		return nil, fmt.Errorf("%w: 缺仓库路径", adapter.ErrBadPath)
	}
	// 路径里出现 .. 就可能是想跳出仓库目录，直接拒
	for _, seg := range strings.Split(repo, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return nil, fmt.Errorf("%w: 仓库路径不合法", adapter.ErrBadPath)
		}
	}

	svc := r.URL.Query().Get("service")
	switch svc {
	case "":
		// dumb 协议的裸对象请求，现代 Git 用不到，拒掉免得成为任意文件读取口
		if r.Method == http.MethodGet && strings.Contains(repo, "/objects/") {
			return nil, fmt.Errorf("%w: 不支持 dumb HTTP 协议", adapter.ErrBadPath)
		}
	case "git-upload-pack":
		// 读，允许
	case "git-receive-pack":
		return nil, fmt.Errorf("%w: 只支持拉取，不支持 push", adapter.ErrReadOnly)
	default:
		return nil, fmt.Errorf("%w: 未知的 service=%s", adapter.ErrBadPath, svc)
	}

	u := &url.URL{
		Scheme:   scheme,
		Host:     host,
		Path:     "/" + repo,
		RawQuery: r.URL.RawQuery,
	}

	proto := r.Header.Get(protoHeader)
	plan := &adapter.Plan{
		Kind:      "git",
		Upstream:  u,
		Method:    r.Method,
		Header:    http.Header{},
		Redirects: 3,
		CacheKey:  "git:" + scheme + "://" + host + "/" + path.Clean(repo) + "|" + proto,
		TTL:       a.TTL,
		RangeOK:   false,
	}
	if proto != "" {
		plan.Header.Set(protoHeader, proto)
	}
	// 只有 info/refs 值得缓存，而且只是几秒钟的窗口；packfile 是 POST，永远直通。
	if r.Method == http.MethodGet && svc == "git-upload-pack" && a.TTL > 0 {
		plan.Cacheable = true
		plan.Validate = func(resp *http.Response) error {
			ct := resp.Header.Get("Content-Type")
			if strings.Contains(ct, "text/html") {
				return fmt.Errorf("上游返回的是网页而不是 git 协议响应（可能要求登录或仓库不存在）")
			}
			return nil
		}
	}
	return plan, nil
}
