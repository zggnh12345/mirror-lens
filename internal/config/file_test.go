package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// 首次启动生成的模板必须包含全部默认值，并能原样读回。
func TestWriteTemplateContainsAllKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	wrote, err := WriteTemplate(path, Default())
	if err != nil || !wrote {
		t.Fatalf("WriteTemplate: wrote=%v err=%v", wrote, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"addr:", "data:", "cache:", "hedge:", "parallel:", "limits:", "logging:"} {
		if !strings.Contains(string(b), key) {
			t.Fatalf("模板缺少 %q：\n%s", key, b)
		}
	}
	var got Config
	if err := yaml.Unmarshal(b, &got); err != nil {
		t.Fatalf("模板无法解析: %v", err)
	}
	want := Default()
	if got.Addr != want.Addr || got.Cache.MaxGB != want.Cache.MaxGB || got.Parallel.Workers != want.Parallel.Workers {
		t.Fatalf("模板读回与默认值不一致: %+v", got)
	}
}

// 模板文件权限必须是 0600。
func TestWriteTemplatePerm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := WriteTemplate(path, Default()); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("权限=%o，期望 600", perm)
	}
}

// 文件已存在时绝不覆盖。
func TestWriteTemplateDoesNotOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("cache:\n  max_gb: 7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrote, err := WriteTemplate(path, Default())
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Fatal("已有文件不应被模板覆盖")
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "max_gb: 7") {
		t.Fatalf("原文件被改动：%s", b)
	}
}
