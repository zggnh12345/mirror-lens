// Package registryadapter 实现 Docker Registry V2 的只读代理。
//
// 客户端自己拼 /v2/<name>/manifests|blobs/...，路径里塞不下"上游是哪个
// registry"，只能一个实例对一个上游，由配置决定。
package registryadapter

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
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
	Upstream string          // registry-1.docker.io
	Scheme   string          // 回源协议，默认 https；内网/测试可以改 http
	MaxBytes int64           // 单个 blob 上限，超了只透传
	TagTTL   time.Duration   // tag 的重新验证间隔；digest 永久
	Hosts    map[string]bool // 允许 /v2/<host>/... 前缀的站；nil 用 DefaultHosts

	mu       sync.RWMutex
	repoHost map[string]string // 裸格式镜像名 → 实测所在站点（Docker Hub 没有时自动找 ghcr）
}

// DefaultHosts 是允许按 /v2/<host>/... 前缀回源的仓库站。
// dpanel / 1Panel 这类工具会把完整镜像地址塞进路径（ghcr.io/owner/repo），
// 不设白名单就等于让 URL 决定回源地址——那是 SSRF。
var DefaultHosts = []string{
	"docker.io", "registry-1.docker.io", "ghcr.io", "quay.io",
	"gcr.io", "registry.k8s.io", "mcr.microsoft.com", "lscr.io",
	"registry.gitlab.com", "public.ecr.aws", "cgr.dev",
}

func (a *Adapter) hostAllowed(h string) bool {
	if a.Hosts != nil {
		return a.Hosts[h]
	}
	for _, x := range DefaultHosts {
		if x == h {
			return true
		}
	}
	return false
}

// splitHost 拆出路径里的仓库站前缀；没有或不在白名单就回落到默认上游。
func (a *Adapter) splitHost(repo string) (host, name string) {
	def := a.Upstream
	if def == "" {
		def = "registry-1.docker.io"
	}
	i := strings.IndexByte(repo, '/')
	if i <= 0 || !strings.Contains(repo[:i], ".") {
		return def, repo
	}
	h := strings.ToLower(repo[:i])
	if h == "docker.io" {
		h = "registry-1.docker.io"
	}
	if !a.hostAllowed(h) {
		return def, repo
	}
	return h, repo[i+1:]
}

func New(upstream string, maxBytes int64, tagTTL time.Duration) *Adapter {
	if tagTTL < 0 {
		tagTTL = 0
	}
	return &Adapter{Upstream: upstream, Scheme: "https", MaxBytes: maxBytes, TagTTL: tagTTL, repoHost: map[string]string{}}
}

// rememberedHost 返回之前实测到的归属站点（裸格式镜像名）。
func (a *Adapter) rememberedHost(name string) string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.repoHost[name]
}

// rememberHost 记住某个镜像名实际在哪个站点：下次 Plan 直接用它，
// 不必再白等一轮 Docker Hub 的 401。
func (a *Adapter) rememberHost(name, host string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.repoHost == nil {
		a.repoHost = map[string]string{}
	}
	a.repoHost[name] = host
}

// addGhcrFallback 给裸格式的 plan 挂一个 ghcr.io 备用站点。
// 只对默认上游 registry-1.docker.io 生效：只有「配成 Docker Hub 加速、
// 却拉了 ghcr-only 镜像」这一种场景需要猜；自定义上游（内网 registry、
// 测试替身）不该被引向外网。
func (a *Adapter) addGhcrFallback(p *adapter.Plan, name, host, def string) {
	if !strings.EqualFold(host, def) || !strings.EqualFold(def, "registry-1.docker.io") {
		return
	}
	// ghcr 没有 library 命名空间：Docker Hub 官方镜像（library/xxx）在
	// ghcr 上必然 404。挂备链只会白打一次请求，还会把主链拆成两条并行链，
	// 让主链内部的对冲（hedge）失去机会。
	if strings.HasPrefix(name, "library/") {
		return
	}
	gh := &url.URL{Scheme: p.Upstream.Scheme, Host: "ghcr.io", Path: p.Upstream.Path}
	p.Fallbacks = []adapter.Fallback{{URL: gh, Kind: "ghcr"}}
	p.OnFallback = func(h string) { a.rememberHost(name, h) }
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
		// docker 26 拉 attestation manifest 时用的是 sha256-<hex> 这种 tag 形式，
		// 上游按 tag 查必然 404；它本意就是 digest，这里还原成冒号形式。
		if d, ok := dashDigest(ref); ok {
			ref = d
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
	// 路径里可能带 ghcr.io / quay.io 之类前缀，回源站跟着它走。
	host, name := a.splitHost(repo)
	// origin 是客户端请求的逻辑仓库（ghcr.io / docker.io），不随后续
	// 记住的镜像站点漂移；缓存身份只认它，否则同一 tag 会因为这次
	// 命中南京大学、下次命中凤凰城而算出两个 key，反复回源。
	origin := host
	// foreign 表示路径里真的带了别家站点前缀（ghcr.io 之类），不是默认上游。
	def := a.Upstream
	if def == "" {
		def = "registry-1.docker.io"
	}
	foreign := !strings.EqualFold(host, def)
	// 裸格式（Docker Hub 名）：之前实测发现它其实在 ghcr.io，直接走对站点。
	if !foreign {
		if h := a.rememberedHost(name); h != "" {
			host, foreign = h, true
		}
	}
	up := &url.URL{Scheme: scheme, Host: host}

	switch kind {
	case KindManifest:
		accept := UpstreamAccept(r.Header.Get("Accept"), foreign)
		up.Path = "/v2/" + name + "/manifests/" + ref
		p := &adapter.Plan{
			Kind:         a.kindForHost(host, foreign),
			Upstream:     up,
			OfficialOnly: a.officialOnly(host, foreign),
			Method:       http.MethodGet,
			Header:       http.Header{"Accept": {accept}},
			Label:        a.label(host, name, ref),
			// 缓存键里的 Accept 固定按客户端原样归一（foreign=false），
			// 不能跟着回源站点补 OCI 类型：否则同一个 tag 在「官方/镜像」
			// 两条路径上算出两个键，换源就丢命中。
			CacheKey:  "docker:manifest:" + origin + "/" + name + ":" + ref + "|accept=" + UpstreamAccept(r.Header.Get("Accept"), false),
			Cacheable: true,
			TTL:       a.TagTTL,
		}
		if strings.HasPrefix(ref, "sha256:") {
			p.Immutable = true
			p.TTL = 0
			// 按 digest 拉清单：摘要来自客户端，是可信参照，落盘前必须比对。
			p.ExpectSHA = strings.TrimPrefix(ref, "sha256:")
			p.ExpectSource = adapter.DigestSourceDocker
		}
		a.addGhcrFallback(p, name, host, def)
		return p, nil

	case KindBlob:
		up.Path = "/v2/" + name + "/blobs/" + ref
		// 不给 blob 挂备用站点：镜像归属在拉 manifest 时已经记住
		// （OnFallback 写 repoHost），blob 跟着 manifest 走同一个站点即可。
		// 独立挂 fallback 会让每个 4xx 的层请求都多打一次真实外网。
		return &adapter.Plan{
			Kind:         a.kindForHost(host, foreign),
			Upstream:     up,
			OfficialOnly: a.officialOnly(host, foreign),
			Method:       http.MethodGet,
			Label:        "layer " + shortDigest(ref),
			CacheKey:     "docker:blob:" + ref, // 内容寻址：同一个 digest 跨仓库复用
			Cacheable:    true,
			Immutable:    true,
			Stream:       true,
			RangeOK:      true,
			MaxBytes:     a.MaxBytes,
			ExpectSHA:    strings.TrimPrefix(ref, "sha256:"),
			ExpectSource: adapter.DigestSourceDocker,
		}, nil
	}
	return nil, adapter.ErrNoMatch
}

// isGhcrHost ghcr.io 有国内镜像源可加速；其余 foreign 站点（quay/gcr/...）
// 没有镜像源，仍直连官方。
func isGhcrHost(host string) bool { return strings.EqualFold(host, "ghcr.io") }

// kindForHost 决定回源类型：ghcr.io 走 ghcr 源池，docker hub 与其余 foreign
// 站点走 docker（后者靠 OfficialOnly 直连）。
func (a *Adapter) kindForHost(host string, foreign bool) string {
	if foreign && isGhcrHost(host) {
		return "ghcr"
	}
	return "docker"
}

// officialOnly ghcr.io 有镜像源时不再只回官方；其余 foreign 仍只回官方。
func (a *Adapter) officialOnly(host string, foreign bool) bool {
	if foreign && isGhcrHost(host) {
		return false
	}
	return foreign
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

// dashDigest 把 sha256-<64hex> 还原成 sha256:<hex>。
// docker 拉 attestation manifest 时用的是这种 tag 形式，上游按 tag 查只会 404。
func dashDigest(ref string) (string, bool) {
	h, ok := strings.CutPrefix(ref, "sha256-")
	if !ok || len(h) != 64 {
		return "", false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", false
		}
	}
	return "sha256:" + h, true
}

// UpstreamAccept 把客户端的 Accept 收成上游认得的集合。
// ghcr 这类站很严格：只带 schema1、*/* 或纯 docker 类型时一律 404，
// 而 docker 客户端重试时就这么发。所以过滤掉无用类型，非 Hub 站再补上 OCI 类型。
func UpstreamAccept(client string, foreign bool) string {
	modern := map[string]bool{
		"application/vnd.oci.image.index.v1+json":                   true,
		"application/vnd.docker.distribution.manifest.list.v2+json": true,
		"application/vnd.oci.image.manifest.v1+json":                true,
		"application/vnd.docker.distribution.manifest.v2+json":      true,
	}
	var keep []string
	seen := map[string]bool{}
	for _, p := range strings.Split(client, ",") {
		if i := strings.IndexByte(p, ';'); i >= 0 {
			p = p[:i]
		}
		p = strings.ToLower(strings.TrimSpace(p))
		if modern[p] && !seen[p] {
			seen[p] = true
			keep = append(keep, p)
		}
	}
	if foreign {
		for _, t := range []string{"application/vnd.oci.image.index.v1+json", "application/vnd.oci.image.manifest.v1+json"} {
			if !seen[t] {
				seen[t] = true
				keep = append(keep, t)
			}
		}
	}
	if len(keep) == 0 {
		return DefaultAccept
	}
	return strings.Join(keep, ",")
}

// shortDigest 把 sha256:xxxx... 缩成面板能放下的一小段。
func shortDigest(d string) string {
	if len(d) <= 19 {
		return d
	}
	return d[:19] + "…"
}

// label 在跨站时带上站点名，面板里才分得清同名的 nginx。
func (a *Adapter) label(host, name, ref string) string {
	if host != a.Upstream {
		return host + "/" + name + ":" + ref
	}
	return name + ":" + ref
}
