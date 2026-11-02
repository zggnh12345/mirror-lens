package discover

import "testing"

// 种子表是手改的，改错了没人提醒：黑名单里的域名、重复项都会在这里被挡住。
// 不查 LooksLikeMirror：种子是人工确认过的，名字里没有关键字也可能是加速器
// （github.moeyy.xyz 就是），关键字只用来筛自动发现的候选。
func TestSeedList(t *testing.T) {
	seen := map[string]bool{}
	check := func(host string) {
		if seen[host] {
			t.Errorf("种子表里有重复域名 %s", host)
		}
		seen[host] = true
		if denied(host) {
			t.Errorf("%s 在黑名单里，不该当加速器", host)
		}
	}
	for _, s := range dockerSeeds {
		check(s.host)
	}
	for _, s := range githubSeeds {
		check(s.host)
	}
	if len(dockerSeeds)+len(githubSeeds) == 0 {
		t.Fatal("种子表空了")
	}
}
