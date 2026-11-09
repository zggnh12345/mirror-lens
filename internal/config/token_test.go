package config

import "testing"

// 环境变量优先于设置页保存的 Token：容器部署时注入的环境变量不能被面板值盖掉。
func TestGitHubTokenPrecedence(t *testing.T) {
	g := GitHub{TokenEnv: "MIRRORLENS_TEST_TOKEN", TokenPlain: "from-settings"}
	t.Setenv("MIRRORLENS_TEST_TOKEN", "")
	if got := g.Token(); got != "from-settings" {
		t.Fatalf("无环境变量时应取设置值，实际 %q", got)
	}
	t.Setenv("MIRRORLENS_TEST_TOKEN", "from-env")
	if got := g.Token(); got != "from-env" {
		t.Fatalf("环境变量应优先，实际 %q", got)
	}
	if !g.HasToken() {
		t.Fatal("HasToken 应为 true")
	}
	empty := GitHub{}
	if empty.HasToken() {
		t.Fatal("空配置不应报告有 Token")
	}
}
