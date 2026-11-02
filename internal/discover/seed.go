package discover

import (
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 内置候选：手工整理的起点清单，不是推荐清单。全部 pending，由探测决定去留。
//
// 2026-11-02 用探测器复测全部 45 条，只剩 8 条能用，于是删掉 21 条不会自己好的：
// DNS 不存在、证书过期或域名不匹配、返回落地页、路径代理当不了 registry、源站 5xx。
// 超时、429、连接重置留着——可能只是我们这边出口的问题，不是站点没了。
// 其余靠自动发现补：它每天扫公开列表和状态页，活着的源会自己进来。
var dockerSeeds = []struct{ host, note string }{
	{"docker.m.daocloud.io", "实测可用，标准 /v2/"},
	{"docker.1ms.run", "实测可用，token 在 /openapi/v1/auth/token"},
	{"dockerproxy.net", "实测可用，无 challenge 直接 200"},
	{"docker.xuanyuan.me", "实测可用，标准 /v2/"},
	{"hub1.nat.tf", "实测可用"},
	{"docker.1panel.dev", "连接被重置，等恢复"},
	{"docker.1panel.top", "高负载常 429，等负载降下来"},
	{"docker.fxxk.dedyn.io", "429 限流，等窗口"},
	{"dockerproxy.cn", "连接超时，等恢复"},
	{"dockerpull.org", "响应超时，等恢复"},
	{"docker.1panel.work", "待验证"},
	{"docker.anye.icu", "待验证"},
	{"docker.hpcloud.cloud", "待验证"},
	{"docker.rainbond.cc", "待验证"},
	{"docker.unsee.tech", "待验证"},
}

var githubSeeds = []struct{ host, note string }{
	{"gh-proxy.com", "实测最快"},
	{"ghfast.top", "实测稳定"},
	{"ghproxy.net", "实测可用，常慢一个量级"},
	{"gh.llkk.cc", "响应超时，等恢复"},
	{"ghproxy.com", "连接超时，等恢复"},
	{"mirror.ghproxy.com", "连接超时，等恢复"},
	{"github.moeyy.xyz", "连接超时，等恢复"},
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
	add := func(host, note, kind string) {
		x := store.Source{
			BaseURL: "https://" + host, Name: host,
			Origin: "seed", Enabled: true, Status: "pending", Note: note,
			DockerCap: store.CapUnknown, GithubCap: store.CapUnknown,
		}
		if kind == "docker" {
			x.DockerCap = store.CapYes
		} else {
			x.GithubCap = store.CapYes
		}
		if _, err := st.AddSource(x); err == nil {
			added++
			return
		}
		// 同一个域名在两个清单里都出现过：合并能力，别丢掉
		if old, err := st.ByURL(x.BaseURL); err == nil && old != nil {
			d, g := old.DockerCap, old.GithubCap
			if x.DockerCap == store.CapYes {
				d = store.CapYes
			}
			if x.GithubCap == store.CapYes {
				g = store.CapYes
			}
			if d != old.DockerCap || g != old.GithubCap {
				_ = st.SetCaps(old.ID, d, g)
			}
		}
	}
	for _, s := range dockerSeeds {
		add(s.host, s.note, "docker")
	}
	for _, s := range githubSeeds {
		add(s.host, s.note, "github")
	}
	return added, nil
}
