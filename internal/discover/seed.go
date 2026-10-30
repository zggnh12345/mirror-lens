package discover

import (
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// Seed 首次启动时写入候选池。全部标记 pending，由探测决定去留。
// 清单会过期：这些只是起点，不是推荐。
var dockerSeeds = []struct{ host, note string }{
	{"docker.m.daocloud.io", "DaoCloud，标准 /v2/"},
	{"docker.1ms.run", "标准 /v2/，token 在 /openapi/v1/auth/token"},
	{"dockerproxy.net", "标准 /v2/，无 challenge 直接 200"},
	{"docker.1panel.dev", "1Panel"},
	{"docker.xuanyuan.me", "标准 /v2/"},
	{"docker.1panel.top", "标准 /v2/，高负载常 429"},
	{"hub.rat.dev", "发 token 但 manifest 401，多半已坏"},
	{"docker.hlmirror.com", "token realm 返回 401"},
	{"docker.imgdb.de", "返回落地页，不是 registry"},
	{"hub1.nat.tf", "manifest 404"},
	{"docker.1panel.live", "路径代理，不能进 daemon.json"},
	{"docker.kejilion.pro", "Cloudflare 525"},
	{"docker.agsvpt.work", "530"},
	{"docker.agsv.top", "404"},
	{"dockerpull.org", "404"},
	{"docker.fxxk.dedyn.io", "429 限流"},
	{"docker.rainbond.cc", "待验证"},
	{"docker.hpcloud.cloud", "待验证"},
	{"docker.unsee.tech", "待验证"},
	{"docker.anye.icu", "待验证"},
	{"docker.1panel.work", "待验证"},
	{"docker.hub.devyu.com", "待验证"},
	{"docker.chenby.cn", "NXDOMAIN"},
	{"dockerproxy.cn", "连接超时"},
	{"mirror.ccs.tencentyun.com", "腾讯云内网，已下线"},
	{"docker.mirrors.ustc.edu.cn", "已下线"},
	{"registry.docker-cn.com", "已下线"},
}

var githubSeeds = []struct{ host, note string }{
	{"gh-proxy.com", "实测最快"},
	{"ghfast.top", "稳定第二"},
	{"gh.llkk.cc", "稳定"},
	{"ghproxy.net", "可用但常慢一个量级"},
	{"ghps.cc", "raw 路径返回反爬脚本"},
	{"ghproxy.cc", "证书已过期，不要用"},
	{"github.akams.cn", "自有 UI，raw 路径 404"},
	{"ghproxy.1888866.xyz", "525"},
	{"hub.gitmirror.com", "NXDOMAIN"},
	{"ghproxy.cfd", "NXDOMAIN"},
	{"ghp.ci", "NXDOMAIN"},
	{"ghproxy.com", "连接超时"},
	{"mirror.ghproxy.com", "连接超时"},
	{"github.moeyy.xyz", "连接超时"},
	{"gh.api.99988866.xyz", "TLS 握手失败"},
	{"gh-download.ckyl.me", "待验证"},
	{"gh.jasonzeng.dev", "待验证"},
	{"gh.xxooo.cf", "待验证"},
}

// Seed 把内置候选池写入空库；已有记录不覆盖。
func Seed(st *store.Store) (int, error) {
	n, err := st.Count()
	if err != nil {
		return 0, err
	}
	if n > 0 {
		return 0, nil
	}
	added := 0
	for _, s := range dockerSeeds {
		if _, err := st.AddSource(store.Source{
			Kind: "docker", BaseURL: "https://" + s.host, Name: s.host,
			Origin: "seed", Enabled: true, Status: "pending", Note: s.note,
		}); err == nil {
			added++
		}
	}
	for _, s := range githubSeeds {
		if _, err := st.AddSource(store.Source{
			Kind: "github", BaseURL: "https://" + s.host, Name: s.host,
			Origin: "seed", Enabled: true, Status: "pending", Note: s.note,
		}); err == nil {
			added++
		}
	}
	return added, nil
}
