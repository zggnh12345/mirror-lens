package discover

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 列表仓库：这些 README 里维护着镜像站清单。抓的是"候选"，不是"可用"，
// 一律以 pending 入库，只有通过 L2 真拉取才会转成 usable。
var listRepos = []struct {
	kind string
	url  string
}{
	{"docker", "https://raw.githubusercontent.com/dongyubin/DockerHub/main/README.md"},
	{"docker", "https://raw.githubusercontent.com/dongyubin/DockerHub/master/README.md"},
	{"github", "https://raw.githubusercontent.com/hunshcn/gh-proxy/master/README.md"},
}

var hostRe = regexp.MustCompile(`https?://([a-zA-Z0-9](?:[a-zA-Z0-9.-]*[a-zA-Z0-9])?\.[a-zA-Z]{2,})`)

// 自身或文档站点，不是加速器
var deny = map[string]bool{
	"github.com": true, "raw.githubusercontent.com": true, "api.github.com": true,
	"gitee.com": true, "docker.com": true, "docker.io": true, "hub.docker.com": true,
	"registry-1.docker.io": true, "docs.docker.com": true, "img.shields.io": true,
	"badgen.net": true, "shields.io": true, "opensource.org": true, "go.dev": true,
	"golang.org": true, "githubusercontent.com": true, "camo.githubusercontent.com": true,
	"user-images.githubusercontent.com": true, "avatars.githubusercontent.com": true,
	"api.github.io": true, "github.dev": true,
}

// 关键词过滤：只保留看起来是加速器的域名，压掉列表里的杂音。
var hints = []string{"docker", "mirror", "ghproxy", "gh-proxy", "ghp", "proxy", "registry", "hub", "加速"}

// 文档站、代码托管站自身的子域，一律不是加速器
var denySuffix = []string{"github.com", "github.io", "githubusercontent.com", "vercel.app", "jsdelivr.net", "cloudflare.com"}

func LooksLikeMirror(host, kind string) bool {
	if deny[host] || strings.Count(host, ".") < 1 {
		return false
	}
	for _, sfx := range denySuffix {
		if host == sfx || strings.HasSuffix(host, "."+sfx) {
			return false
		}
	}
	h := strings.ToLower(host)
	if kind == "github" {
		return strings.Contains(h, "gh") || strings.Contains(h, "proxy") || strings.Contains(h, "mirror")
	}
	for _, k := range hints {
		if strings.Contains(h, k) {
			return true
		}
	}
	return false
}

// Scan 抓取列表仓库，返回新发现的主机名（不含已在库里的）。
func Scan(ctx context.Context, st *store.Store) (added int, err error) {
	client := &http.Client{Timeout: 25 * time.Second}
	known := map[string]bool{}
	srcs, e := st.ListSources()
	if e != nil {
		return 0, e
	}
	for _, s := range srcs {
		known[strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(s.BaseURL, "https://"), "http://"))] = true
	}

	found := map[string]string{} // host -> kind
	for _, repo := range listRepos {
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, repo.url, nil)
		if e != nil {
			continue
		}
		req.Header.Set("User-Agent", "mirror-lens/1.0")
		resp, e := client.Do(req)
		if e != nil {
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			continue
		}
		for _, m := range hostRe.FindAllStringSubmatch(string(body), -1) {
			h := strings.ToLower(m[1])
			if known[h] || found[h] != "" {
				continue
			}
			if LooksLikeMirror(h, repo.kind) {
				found[h] = repo.kind
			}
		}
	}

	keys := make([]string, 0, len(found))
	for h := range found {
		keys = append(keys, h)
	}
	sort.Strings(keys)
	for _, h := range keys {
		// 能力留空，交给探测判断：列表里说它是 docker 站，不代表真是
		_, e := st.AddSource(store.Source{
			BaseURL:   "https://" + h,
			Name:      h,
			Origin:    "discovered",
			Enabled:   true,
			Status:    "pending",
			Note:      "自动发现，待验证",
			DockerCap: store.CapUnknown,
			GithubCap: store.CapUnknown,
		})
		if e == nil {
			added++
		}
	}
	return added, nil
}
