// Package registryadapter 实现 Docker Registry V2 的只读代理。
//
// 客户端自己拼 /v2/<name>/manifests|blobs/...，所以"上游是哪个 registry"
// 没有地方可塞——只能一个实例对一个上游，由配置决定。
package registryadapter

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
)

type Kind int

const (
	KindPing Kind = iota
	KindManifest
	KindBlob
	KindWrite // 上传/删除，一律 405
	KindUnknown
)

// DefaultAccept 是客户端没带 Accept 时我们替它要的清单类型。
// Hub 按这个顺序返回 manifest list（多架构索引），docker 拿到后再挑平台。
const DefaultAccept = "application/vnd.oci.image.index.v1+json," +
	"application/vnd.docker.distribution.manifest.list.v2+json," +
	"application/vnd.oci.image.manifest.v1+json," +
	"application/vnd.docker.distribution.manifest.v2+json," +
	"application/vnd.docker.distribution.manifest.v1+prettyjws"

type Adapter struct {
	Upstream string        // registry-1.docker.io
	Scheme   string        // 回源协议，默认 https；内网/测试可以改 http
	MaxBytes int64         // 单个 blob 上限，超了只透传
	TagTTL   time.Duration // tag 的重新验证间隔；digest 永久
}

func New(upstream string, maxBytes int64, tagTTL time.Duration) *Adapter {
	if tagTTL < 0 {
		tagTTL = 0
	}
	return &Adapter{Upstream: upstream, Scheme: "https", MaxBytes: maxBytes, TagTTL: tagTTL}
}

func (a *Adapter) Name() string { return "docker" }

func (a *Adapter) Match(path string) bool {
	return path == "/v2" || strings.HasPrefix(path, "/v2/")
}

// Route 把路径拆成 repo / ref / digest。
func (a *Adapter) Route(path string) (Kind, string, string, error) {
	if path == "/v2" || path == "/v2/" {
		return KindPing, "", "", nil
	}
	if !strings.HasPrefix(path, "/v2/") {
		return KindUnknown, "", "", adapter.ErrNoMatch
	}
	p := strings.TrimPrefix(path, "/v2/")

	// 上传三段都先判掉，否则会被 /blobs/ 误吞
	if strings.Contains(p, "/blobs/uploads/") || strings.HasSuffix(p, "/blobs/uploads") {
		return KindWrite, "", "", nil
	}
	if i := strings.LastIndex(p, "/manifests/"); i > 0 {
		repo, ref := p[:i], p[i+len("/manifests/"):]
		if err := checkRepo(repo); err != nil {
			return KindUnknown, "", "", err
		}
		if ref == "" || strings.ContainsAny(ref, "/?#") {
			return KindUnknown, "", "", adapter.ErrBadPath
		}
		return KindManifest, repo, ref, nil
	}
	if i := strings.LastIndex(p, "/blobs/"); i > 0 {
		repo, dig := p[:i], p[i+len("/blobs/"):]
		if err := checkRepo(repo); err != nil {
			return KindUnknown, "", "", err
		}
		if !validDigest(dig) {
			return KindUnknown, "", "", adapter.ErrBadPath
		}
		return KindBlob, repo, dig, nil
	}
	return KindUnknown, "", "", adapter.ErrBadPath
}

// Plan 只负责 manifest 和 blob；ping 由网关直接答。
func (a *Adapter) Plan(r *http.Request) (*adapter.Plan, error) {
	kind, repo, ref, err := a.Route(r.URL.Path)
	if err != nil {
		return nil, err
	}
	scheme := a.Scheme
	if scheme == "" {
		scheme = "https"
	}
	up := &url.URL{Scheme: scheme, Host: a.Upstream}

	switch kind {
	case KindManifest:
		accept := r.Header.Get("Accept")
		if accept == "" {
			accept = DefaultAccept
		}
		up.Path = "/v2/" + repo + "/manifests/" + ref
		p := &adapter.Plan{
			Kind:      "docker",
			Upstream:  up,
			Method:    http.MethodGet,
			Header:    http.Header{"Accept": {accept}},
			CacheKey:  "docker:manifest:" + a.Upstream + "/" + repo + ":" + ref + "|accept=" + normalizeAccept(accept),
			Cacheable: true,
			TTL:       a.TagTTL,
		}
		if strings.HasPrefix(ref, "sha256:") {
			p.Immutable = true
			p.TTL = 0
			// 按 digest 拉清单：摘要来自客户端，是可信参照，落盘前必须比对。
			p.ExpectSHA = strings.TrimPrefix(ref, "sha256:")
		}
		return p, nil

	case KindBlob:
		up.Path = "/v2/" + repo + "/blobs/" + ref
		return &adapter.Plan{
			Kind:      "docker",
			Upstream:  up,
			Method:    http.MethodGet,
			CacheKey:  "docker:blob:" + ref, // 内容寻址：同一个 digest 跨仓库复用
			Cacheable: true,
			Immutable: true,
			Stream:    true,
			RangeOK:   true,
			MaxBytes:  a.MaxBytes,
			ExpectSHA: strings.TrimPrefix(ref, "sha256:"),
		}, nil
	}
	return nil, adapter.ErrNoMatch
}

func checkRepo(repo string) error {
	if repo == "" || len(repo) > 255 {
		return adapter.ErrBadPath
	}
	if strings.Contains(repo, "..") || strings.Contains(repo, "//") ||
		strings.HasPrefix(repo, "/") || strings.HasSuffix(repo, "/") {
		return adapter.ErrBadPath
	}
	for _, c := range repo {
		ok := c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '.' || c == '_' || c == '-' || c == '/'
		if !ok {
			return fmt.Errorf("%w: 仓库名里有非法字符", adapter.ErrBadPath)
		}
	}
	return nil
}

func validDigest(d string) bool {
	if !strings.HasPrefix(d, "sha256:") || len(d) != 7+64 {
		return false
	}
	for _, c := range d[7:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// normalizeAccept 排序去空格：同一个 tag，客户端 Accept 不同拿到的是
// 不同字节（索引 vs 单平台清单），缓存键必须把这层差异带上。
func normalizeAccept(s string) string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
