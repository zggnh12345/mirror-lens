package store

import (
	"strings"
	"testing"
)

// §15.3 G4 三组：Realm 证据分离的行为边界。
// (1) 有 Challenge 的跨 Host 策略（lscr.io→ghcr.io）必须保留；
// (2) 无 Challenge 场景下格式非法/跨 Host 裸值不得被当作策略 Realm 接受；
// (3) 策略值只进 source_realm_policy，不写进任何 Challenge 证据字段。
// 注：Challenge 实测值属 G5 来源矩阵列；本测试锁定策略侧行为边界。
func TestG4RealmEvidenceSeparation(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{BaseURL: "https://lscr.io", Name: "lscr", RepoHost: "lscr.io", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddSourceHost(id, "lscr.io", "https://ghcr.io", nil); err != nil {
		t.Fatal(err)
	}
	// (1) 有 Challenge 的跨 Host 策略：lscr.io 的 primary 是 ghcr.io，必须接受。
	p, err := st.GetRealmPolicy(id, "lscr.io")
	if err != nil {
		t.Fatal(err)
	}
	if p.PrimaryRealm != "https://ghcr.io" {
		t.Fatalf("跨 Host 策略 Realm 被改写: %q", p.PrimaryRealm)
	}

	// (2) 格式非法 Realm 必须拒绝，不能落库。
	if err := st.SetRealmPolicy(id, "lscr.io", "not-a-url", nil); err == nil {
		t.Fatal("非法格式 Realm 应拒绝")
	}
	// 策略值不会写进 sources.base_url（Challenge/源站 Evidence 证明值）。
	src, err := st.GetSource(id)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(src.BaseURL, "ghcr.io") {
		t.Fatal("策略 Realm 不得覆盖源 Endpoint")
	}

	// (3) 显式设置同 Host 策略：合法，且不影响跨 Host 既有行为。
	if err := st.SetRealmPolicy(id, "lscr.io", "https://lscr.io", nil); err != nil {
		t.Fatalf("同 Host 策略应接受: %v", err)
	}
	p2, err := st.GetRealmPolicy(id, "lscr.io")
	if err != nil {
		t.Fatal(err)
	}
	if p2.PrimaryRealm != "https://lscr.io" {
		t.Fatalf("同 Host 策略未生效: %q", p2.PrimaryRealm)
	}
}
