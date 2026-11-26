package api

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestSaveConfigRejectsInvalidRegistryUpstream 覆盖 Web 保存路径：
// 非法 registry.upstream 必须 400/错误返回，且原配置文件字节不变。
func TestSaveConfigRejectsInvalidRegistryUpstream(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeInitialConfig(t, path)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := newCfgServer(t, path)

	bad := []string{
		"registry.example.com:5000",
		"203.0.113.10",
		"[2001:db8::10]",
		"[2001:db8::10]:5000",
		"https://registry.example.com",
		"ftp://host",
		"registry.example.com/v2",
		"registry.example.com?x=1",
		"user@registry.example.com",
		"",
		"   ",
	}
	for _, v := range bad {
		if _, _, err := s.saveConfig(map[string]string{"registry.upstream": v}); err == nil {
			t.Fatalf("registry.upstream=%q 应拒绝保存", v)
		}
		after, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if !bytes.Equal(after, before) {
			t.Fatalf("registry.upstream=%q 校验失败后原文件被改动", v)
		}
	}
}

// TestSaveConfigAcceptsValidRegistryUpstream 确认合法纯主机名仍可保存。
func TestSaveConfigAcceptsValidRegistryUpstream(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeInitialConfig(t, path)
	s := newCfgServer(t, path)

	if _, _, err := s.saveConfig(map[string]string{"registry.upstream": "registry.example.com"}); err != nil {
		t.Fatalf("合法 upstream 保存失败: %v", err)
	}
	if got := s.cfg.Load().Registry.Upstream; got != "registry.example.com" {
		t.Fatalf("运行态 upstream = %q", got)
	}
}
