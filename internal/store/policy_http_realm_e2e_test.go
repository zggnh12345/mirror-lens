package store

import (
	"context"
	"net/url"
	"testing"
)

// TestStorePolicyHTTPRealmEndToEnd 锁定 P1-1 的端到端链路：
// SetRealmPolicy → StorePolicy.Resolve → PolicySnapshot.Allows。
//
// 关键点（与复核 P1 一致）：
//   - StorePolicy.Resolve 对 sourceID>0 恒置 AllowSameHost=false，
//     必须走 HTTP Realm 完整 Origin 门禁；
//   - AllowPlainHTTP/SourceOrigin 必须由 sources.base_url 在 Resolve 时派生，
//     不能只在 gateway 请求路径覆盖，否则 probe/RealmAllowed 等
//     直接 Resolve→Allows 的路径会错误拒绝 HTTP Source 的合法 Realm；
//   - base_url 从 HTTP 改成 HTTPS 后，旧 HTTP Realm 即使仍在策略白名单里
//     也不得再获得授权。
//
// 这是 store 层测试：直接驱动真实的 SetRealmPolicy/Resolve/Allows 生产代码，
// 不复制任何判断逻辑。
func TestStorePolicyHTTPRealmEndToEnd(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{
		BaseURL: "http://quay.example:5000", Name: "http-mirror", RepoHost: "quay.io",
		DockerCap: 1, Status: "usable", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 写入层窄例外：base_url 是 HTTP，且 Realm 与其完整 Origin 全等，
	// 因此 HTTP Realm 可以持久化。
	realm := "http://quay.example:5000"
	if err := st.SetRealmPolicy(id, "quay.io", realm, nil); err != nil {
		t.Fatalf("SetRealmPolicy 应接受与 HTTP Source 完整 Origin 全等的 Realm: %v", err)
	}

	ru, err := url.Parse(realm + "/token")
	if err != nil {
		t.Fatal(err)
	}
	pol := NewStorePolicy(st)

	// R1：合法 HTTP Source + 同 Origin HTTP Realm → Resolve 出的快照必须允许。
	p, err := pol.Resolve(context.Background(), id, "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if !p.AllowPlainHTTP {
		t.Fatal("Resolve 必须由 HTTP base_url 派生 AllowPlainHTTP=true")
	}
	if p.SourceOrigin.IsZero() {
		t.Fatal("Resolve 必须由 HTTP base_url 派生 SourceOrigin")
	}
	if err := p.Allows(ru); err != nil {
		t.Fatalf("端到端 R1：合法 HTTP Realm 必须允许: %v", err)
	}

	// R2：同 Host 不同端口的 Realm 必须拒绝。
	other, _ := url.Parse("http://quay.example:8080/token")
	if err := p.Allows(other); err == nil {
		t.Fatal("端到端 R2：同 Host 不同端口的 HTTP Realm 必须拒绝")
	}

	// R3 / base_url 编辑失效：把 Source 改成 HTTPS 后重新 Resolve，
	// 旧 HTTP Realm 仍在策略白名单里，但不得再获得授权。
	if _, err := st.db.Exec(`UPDATE sources SET base_url=? WHERE id=?`, "https://quay.example:5000", id); err != nil {
		t.Fatal(err)
	}
	p2, err := pol.Resolve(context.Background(), id, "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if p2.AllowPlainHTTP || !p2.SourceOrigin.IsZero() {
		t.Fatalf("base_url 改 HTTPS 后不得再带明文上下文: %+v", p2)
	}
	if err := p2.Allows(ru); err == nil {
		t.Fatal("端到端 R3：base_url 改 HTTPS 后旧 HTTP Realm 必须拒绝")
	}
}

// TestStorePolicySourcePlainOriginDerivation 用表驱动锁定派生规则的边界：
// 只有 HTTP base_url 才产生明文上下文；HTTPS/非法/空地址一律零值。
func TestStorePolicySourcePlainOriginDerivation(t *testing.T) {
	st := openTest(t)
	pol := NewStorePolicy(st)
	cases := []struct {
		name       string
		baseURL    string
		wantPlain  bool
		wantOrigin string
	}{
		{"HTTP 非默认端口", "http://h.example:5000", true, "http://h.example:5000"},
		{"HTTP 默认端口", "http://h.example", true, "http://h.example:80"},
		{"HTTPS 无明文", "https://h.example:5443", false, ""},
		{"非法地址无明文", "not a url", false, ""},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := st.AddSource(Source{
				BaseURL: tc.baseURL, Name: "s", RepoHost: "quay.io",
				DockerCap: 1, Status: "usable", Enabled: true,
			})
			if err != nil {
				// 非法 base_url 可能在 AddSource 阶段被接受；跳过用 SQL 直接写的情况。
				t.Fatalf("AddSource(%q): %v", tc.baseURL, err)
			}
			_ = i
			// 用一个已存在的策略 Host 读取快照。
			p, err := pol.Resolve(context.Background(), id, "quay.io")
			if err != nil {
				t.Fatal(err)
			}
			if p.AllowPlainHTTP != tc.wantPlain {
				t.Fatalf("AllowPlainHTTP=%v，期望 %v（base=%q）", p.AllowPlainHTTP, tc.wantPlain, tc.baseURL)
			}
			got := p.SourceOrigin.Origin()
			if got != tc.wantOrigin {
				t.Fatalf("SourceOrigin=%q，期望 %q（base=%q）", got, tc.wantOrigin, tc.baseURL)
			}
		})
	}
}
