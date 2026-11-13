package main

import (
	"os"
	"path/filepath"
	"strings"
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

func TestCheckRestoreJournal(t *testing.T) {
	dir := t.TempDir()
	if err := checkRestoreJournal(dir); err != nil {
		t.Fatalf("无 journal 时不应拦截: %v", err)
	}
	jp := filepath.Join(dir, ".restore-journal.json")
	if err := os.WriteFile(jp, []byte(`{"txid":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := checkRestoreJournal(dir)
	if err == nil {
		t.Fatal("journal 存在时必须拒绝启动")
	}
	if !strings.Contains(err.Error(), "--recover") || !strings.Contains(err.Error(), jp) {
		t.Fatalf("错误信息应指路 --recover 与路径，得到: %v", err)
	}
}

// 启动早期顺序：先看 journal，再碰密钥。数据目录里同时有 journal 和
// 「有库无 key」时，绝不能先生成新 key。
func TestJournalCheckedBeforeKeyCreation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mirror-lens.db"), []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".restore-journal.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkRestoreJournal(dir); err == nil {
		t.Fatal("应拦截")
	}
	if _, err := os.Stat(filepath.Join(dir, "secret.key")); !os.IsNotExist(err) {
		t.Fatal("拦截路径不应产生 secret.key")
	}
}
