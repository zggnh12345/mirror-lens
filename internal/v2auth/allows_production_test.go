package v2auth

import (
	"testing"
)

// TestAllowsProductionSourcePolicy 用第三方 Source 的真实策略形态
// （AllowSameHost=false，来自 StorePolicy.Resolve）验证 R1/R2/R3 与 P1 回归。
//
// 关键：StorePolicy.Resolve 对 sourceID>0 恒置 AllowSameHost=false，
// 因此不能用 AllowSameHost=true 的人工组合测试 Realm 授权路径。
// HTTP Realm 的窄例外必须在所有分支（同 Host、显式白名单）之前统一生效。
func TestAllowsProductionSourcePolicy(t *testing.T) {
	t.Run("R1 HTTP 同 Origin 放行（Realm 同时在白名单）", func(t *testing.T) {
		p := mustPolicy(t, "http://h:5000", "h", "https", false, true, "http://h:5000")
		if err := p.Allows(realmURL(t, "http://h:5000/v2/token")); err != nil {
			t.Fatalf("R1 应允许: %v", err)
		}
	})

	t.Run("R2 HTTP 同 Host 不同端口拒绝", func(t *testing.T) {
		p := mustPolicy(t, "http://h:5000", "h", "https", false, true, "http://h:5000")
		if err := p.Allows(realmURL(t, "http://h:8080/v2/token")); err == nil {
			t.Fatal("R2 必须拒绝 :8080")
		}
	})

	t.Run("R3 Source 由 HTTP 改 HTTPS 后旧 HTTP Realm 必须拒绝（P1 回归）", func(t *testing.T) {
		// 旧 http://h:5000 Realm 仍留在白名单，但当前 Source 已是 HTTPS，
		// AllowPlainHTTP=false。即使 Realm 命中白名单也不得放行。
		p := mustPolicy(t, "https://h", "h", "https", false, false, "http://h:5000")
		if err := p.Allows(realmURL(t, "http://h:5000/v2/token")); err == nil {
			t.Fatal("P1 回归：AllowPlainHTTP=false 时白名单里的 HTTP Realm 必须拒绝")
		}
	})

	t.Run("HTTP Realm 白名单命中但缺 SourceOrigin 时拒绝", func(t *testing.T) {
		p := mustPolicy(t, "", "h", "https", false, true, "http://h:5000")
		if err := p.Allows(realmURL(t, "http://h:5000/v2/token")); err == nil {
			t.Fatal("缺 SourceOrigin 不得放行 HTTP Realm")
		}
	})

	t.Run("R4 HTTP Source 允许 HTTPS Realm 安全升级", func(t *testing.T) {
		// 白名单里登记 https://h；HTTP Source 收到 HTTPS Realm 属安全升级，
		// 走 HTTPS 白名单分支（CanonicalHost 比较）放行。
		p := mustPolicy(t, "http://h:5000", "h", "https", false, true, "https://h")
		if err := p.Allows(realmURL(t, "https://h/v2/token")); err != nil {
			t.Fatalf("R4 HTTP→HTTPS 升级应允许: %v", err)
		}
	})
}
