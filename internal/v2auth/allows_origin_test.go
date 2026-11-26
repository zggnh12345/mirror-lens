package v2auth

import (
	"net/url"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/endpoint"
)

func mustPolicy(t *testing.T, sourceOrigin, logical, scheme string, allowSame, allowPlain bool, realms ...string) PolicySnapshot {
	t.Helper()
	var se endpoint.Endpoint
	if sourceOrigin != "" {
		var err error
		se, err = endpoint.Parse(sourceOrigin)
		if err != nil {
			t.Fatalf("source origin %q: %v", sourceOrigin, err)
		}
	}
	return PolicySnapshot{
		LogicalHost:    logical,
		AllowSameHost:  allowSame,
		Scheme:         scheme,
		AllowPlainHTTP: allowPlain,
		SourceOrigin:   se,
		Realms:         realms,
	}
}

func realmURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestAllowsRealmMatrix 对应任务书 §4.5 R1-R6（冻结）。
//
// P2-3：R1/R2/R3/R4 原先使用 AllowSameHost=true 的人工快照，但
// StorePolicy.Resolve 对第三方 Source（sourceID>0）恒置 AllowSameHost=false，
// 该组合在生产中不可能出现，会掩盖 HTTP Realm 白名单分支的门禁。
// 现改为真实生产形态：AllowSameHost=false + Realm 在 p.Realms 白名单中。
func TestAllowsRealmMatrix(t *testing.T) {
	t.Run("R1 HTTP 同 Host 同端口放行", func(t *testing.T) {
		// 生产形态：HTTP Source（AllowPlainHTTP=true）+ 白名单里的同 Origin HTTP Realm。
		p := mustPolicy(t, "http://h:5000", "h", "https", false, true, "http://h:5000")
		if err := p.Allows(realmURL(t, "http://h:5000/v2/")); err != nil {
			t.Fatalf("R1 应允许: %v", err)
		}
	})

	t.Run("R2 HTTP 同 Host 不同端口拒绝", func(t *testing.T) {
		p := mustPolicy(t, "http://h:5000", "h", "https", false, true, "http://h:5000")
		if err := p.Allows(realmURL(t, "http://h:8080/v2/")); err == nil {
			t.Fatal("R2 必须拒绝 :8080")
		}
	})

	t.Run("R3 HTTPS Source 拒绝 HTTP Realm", func(t *testing.T) {
		// 生产形态：HTTPS Source（AllowPlainHTTP=false），旧 HTTP Realm 仍在白名单。
		p := mustPolicy(t, "https://h", "h", "https", false, false, "http://h")
		if err := p.Allows(realmURL(t, "http://h/v2/")); err == nil {
			t.Fatal("R3 必须拒绝 HTTP Realm")
		}
	})

	t.Run("R4 HTTP Source 允许 HTTPS Realm 安全升级", func(t *testing.T) {
		// 生产形态：HTTP Source + 白名单里的 HTTPS Realm（CanonicalHost 比较）。
		p := mustPolicy(t, "http://h", "h", "https", false, true, "https://h")
		if err := p.Allows(realmURL(t, "https://h/v2/")); err != nil {
			t.Fatalf("R4 应允许 HTTPS 升级: %v", err)
		}
	})

	t.Run("R4 未命中的 HTTPS Realm 拒绝", func(t *testing.T) {
		p := mustPolicy(t, "http://h", "h", "https", false, true, "https://h")
		if err := p.Allows(realmURL(t, "https://other.example/v2/")); err == nil {
			t.Fatal("R4 未命中必须拒绝")
		}
	})

	t.Run("R5 显式 HTTPS 跨 Host 白名单放行", func(t *testing.T) {
		p := mustPolicy(t, "https://lscr.io", "lscr.io", "https", false, false, "https://ghcr.io")
		if err := p.Allows(realmURL(t, "https://ghcr.io/v2/")); err != nil {
			t.Fatalf("R5 应保留白名单能力: %v", err)
		}
	})

	t.Run("R6 未配置跨 Host 拒绝", func(t *testing.T) {
		p := mustPolicy(t, "https://h", "h", "https", false, false)
		if err := p.Allows(realmURL(t, "https://unconfigured.example/v2/")); err == nil {
			t.Fatal("R6 必须拒绝未配置跨 Host")
		}
	})

	t.Run("HTTPS 同 Host 默认端口等价", func(t *testing.T) {
		// 生产形态：HTTPS Realm 登记在白名单，CanonicalHost 去端口后等价。
		p := mustPolicy(t, "https://h", "h", "https", false, false, "https://h")
		if err := p.Allows(realmURL(t, "https://h:443/v2/")); err != nil {
			t.Fatalf("显式 :443 应等价: %v", err)
		}
	})

	t.Run("HTTP 例外缺 SourceOrigin 时拒绝", func(t *testing.T) {
		p := mustPolicy(t, "", "h", "https", false, true, "http://h:5000")
		if err := p.Allows(realmURL(t, "http://h:5000/v2/")); err == nil {
			t.Fatal("缺 SourceOrigin 不得放行 HTTP Realm")
		}
	})

	t.Run("HTTPS 同 Host 非默认端口放行", func(t *testing.T) {
		// 生产形态：Realm 身份按 CanonicalHost 去端口，白名单匹配后放行。
		p := mustPolicy(t, "https://h", "h", "https", false, false, "https://h")
		if err := p.Allows(realmURL(t, "https://h:8443/v2/")); err != nil {
			t.Fatalf("Realm 身份按 Host 去端口，应放行: %v", err)
		}
	})

	t.Run("白名单端口忽略放行", func(t *testing.T) {
		p := mustPolicy(t, "https://h", "h", "https", false, false, "https://ghcr.io:8443")
		if err := p.Allows(realmURL(t, "https://ghcr.io/v2/")); err != nil {
			t.Fatalf("Realm 身份忽略端口，应放行: %v", err)
		}
	})
}
