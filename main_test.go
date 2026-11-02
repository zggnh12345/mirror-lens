package main

import (
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/config"
)

// 明确写过的参数必须压过环境变量：写反了运维会以为 -addr 生效，
// 实际监听地址被 ADDR 悄悄换掉。
func TestOverridesOrder(t *testing.T) {
	env := func(k string) string {
		switch k {
		case "ADDR":
			return ":9000"
		case "DATA_DIR":
			return "/env/data"
		case "CACHE_DIR":
			return "/env/cache"
		}
		return ""
	}
	cfg := &config.Config{}
	cfg.Addr, cfg.Data, cfg.Cache.Dir = ":8080", "/file/data", "/file/cache"

	// 都没显式给：环境变量赢
	applyOverrides(cfg, func(string) bool { return false }, env, ":7000", "/flag/data", false)
	if cfg.Addr != ":9000" || cfg.Data != "/env/data" || cfg.Cache.Dir != "/env/cache" {
		t.Fatalf("环境变量应覆盖配置文件: %+v", cfg)
	}

	// 命令行给了：命令行赢
	applyOverrides(cfg, func(n string) bool { return true }, env, ":7000", "/flag/data", true)
	if cfg.Addr != ":7000" || cfg.Data != "/flag/data" || !cfg.NoSeed {
		t.Fatalf("命令行应覆盖环境变量: %+v", cfg)
	}

	// 谁都没给：保留配置文件的值
	cfg2 := &config.Config{}
	cfg2.Addr = ":8080"
	applyOverrides(cfg2, func(string) bool { return false }, func(string) string { return "" }, "", "", false)
	if cfg2.Addr != ":8080" {
		t.Fatalf("没人覆盖时应保持原值: %+v", cfg2)
	}
}
