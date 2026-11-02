package discover

import (
	"regexp"
	"strings"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// SiteNameUnknown 是来源链的兜底值。取不到名字就写这个，不要编一个。
const SiteNameUnknown = "未知"

// siteAliases 是内置种子表的手写「域名 → 展示名」映射。
// 状态页拿不到名字时用它兜底；键是 host[:port] 归一后的值（见 nameKey）。
var siteAliases = map[string]string{
	// Docker Hub 侧
	"docker.m.daocloud.io": "DaoCloud",
	"docker.1ms.run":       "毫秒镜像",
	"dockerproxy.net":      "DockerProxy",
	"docker.xuanyuan.me":   "轩辕镜像",
	"hub1.nat.tf":          "NAT.TF",
	"docker.1panel.dev":    "1Panel",
	"docker.1panel.top":    "1Panel",
	"docker.fxxk.dedyn.io": "FXXK",
	"dockerproxy.cn":       "DockerProxy CN",
	"dockerpull.org":       "DockerPull",
	"docker.1panel.work":   "1Panel",
	"docker.anye.icu":      "AnyE",
	"docker.hpcloud.cloud": "HPCloud",
	"docker.rainbond.cc":   "Rainbond",
	"docker.unsee.tech":    "Unsee",
	// GitHub 侧
	"gh-proxy.com":       "gh-proxy",
	"ghfast.top":         "GhFast",
	"ghproxy.net":        "GhProxy",
	"gh.llkk.cc":         "LLKK",
	"ghproxy.com":        "GhProxy",
	"mirror.ghproxy.com": "GhProxy",
	"github.moeyy.xyz":   "Moeyy",
	"gh.jasonzeng.dev":   "JasonZeng",
	"gh.xxooo.cf":        "XXOOO",
}

// versionSuffixRe 匹配名字末尾的版本/套餐后缀。
// 这些后缀在窄列里会被截成「毫秒镜…」，所以在入库前就剥掉，不留到渲染时处理。
var versionSuffixRe = regexp.MustCompile(`\s*[（(]\s*(免费版|付费版|专业版|企业版|社区版|测试版|试用版|正式版|Beta|beta|Alpha|alpha|Preview|preview)\s*[)）]\s*$`)

// nameKey 把域名归一成 host[:port]：去协议、去 user:token@、去路径，保留端口。
func nameKey(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.Index(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return strings.Trim(s, ".")
}

// CleanSiteName 剥掉名字里的版本后缀。重复剥是为了处理「（Beta）（免费版）」这种叠加。
func CleanSiteName(s string) string {
	s = strings.TrimSpace(s)
	for {
		t := strings.TrimSpace(versionSuffixRe.ReplaceAllString(s, ""))
		if t == s || t == "" {
			break
		}
		s = t
	}
	return s
}

// SiteNameFor 按「手写映射」查展示名，查不到返回空串。
// 来源链的第一、二环（状态页 data-name / 状态 JSON name）在解析侧写入，
// 这里只负责内置种子和兜底。
func SiteNameFor(raw string) string {
	return CleanSiteName(siteAliases[nameKey(raw)])
}

// ResolveSiteName 按来源链给一个展示名：解析到的名字 > 手写映射 > 未知。
func ResolveSiteName(parsed, baseURL string) string {
	if n := CleanSiteName(parsed); n != "" {
		return n
	}
	if n := SiteNameFor(baseURL); n != "" {
		return n
	}
	return SiteNameUnknown
}

// BackfillSiteNames 给已有记录补站点名（老库升级后 site_name 是空串）。
// 手写映射里有的写真实名字，没有的写「未知」；之后发现流程拿到状态页
// 真名时还能用 SetSiteNameIfEmpty 顶掉「未知」。
func BackfillSiteNames(st *store.Store) (int, error) {
	srcs, err := st.ListSources()
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, x := range srcs {
		if x.SiteName != "" && x.SiteName != SiteNameUnknown {
			continue
		}
		name := ResolveSiteName("", x.BaseURL)
		if err := st.SetSiteNameIfEmpty(x.ID, name); err != nil {
			return changed, err
		}
		changed++
	}
	return changed, nil
}
