package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/config"
)

func TestConfigViewGroupsAndFields(t *testing.T) {
	c := config.Default()
	v := configView(c, "/etc/mirror-lens/config.yaml")
	secs, ok := v["sections"].([]map[string]any)
	if !ok || len(secs) == 0 {
		t.Fatalf("sections 格式不对: %#v", v["sections"])
	}
	if v["config_path"] != "/etc/mirror-lens/config.yaml" {
		t.Fatalf("config_path 没带上: %v", v["config_path"])
	}
	// 「缓存」分组里的 max_gb 默认 0
	var found bool
	for _, s := range secs {
		if s["title"] != "缓存" {
			continue
		}
		fields := s["fields"].([]map[string]any)
		for _, f := range fields {
			if f["key"] == "cache.max_gb" {
				found = true
				want := i64(config.Default().Cache.MaxGB)
				if f["value"] != want {
					t.Fatalf("cache.max_gb 默认值不对，期望 %s 实际 %v", want, f["value"])
				}
				if f["restart"] != true {
					t.Fatalf("cache.max_gb 应标重启生效")
				}
			}
		}
	}
	if !found {
		t.Fatalf("缓存分组里没找到 cache.max_gb")
	}
}

func TestApplyConfigValuesValidAndInvalid(t *testing.T) {
	c := config.Default()

	// 合法：改 max_gb 与日志级别
	changed, field, err := applyConfigValues(c, map[string]string{
		"cache.max_gb":  "120",
		"logging.level": "debug",
	})
	if err != nil {
		t.Fatalf("合法改动被拒: %v", err)
	}
	if c.Cache.MaxGB != 120 || c.Logging.Level != "debug" {
		t.Fatalf("改动没套用: %+v", c.Cache)
	}
	if len(changed) != 2 {
		t.Fatalf("changed 数量不对: %v", changed)
	}

	// 非法：warn_at 超出 0~1
	c2 := config.Default()
	_, field, err = applyConfigValues(c2, map[string]string{"cache.warn_at": "2"})
	if err == nil || field != "cache.warn_at" {
		t.Fatalf("超出范围的 warn_at 应通过校验被拦下, err=%v field=%v", err, field)
	}

	// 非法：时间格式
	c3 := config.Default()
	_, field, err = applyConfigValues(c3, map[string]string{"janitor.interval": "abc"})
	if err == nil || field != "janitor.interval" {
		t.Fatalf("非法时间应通过校验被拦下, err=%v field=%v", err, field)
	}
}

// TestWriteConfigFilePreservesComments 只改一个键时结构注释不丢。
func TestWriteConfigFilePreservesComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	src := `# Mirror Lens 配置示例
# 这是一行不会被改掉的注释
addr: ":8080"
cache:
  max_gb: 0
  warn_at: 0.9
logging:
  level: info
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := writeConfigFile(path, map[string]string{"cache.max_gb": "200"}); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}

	// 注释仍在
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "这是一行不会被改掉的注释") {
		t.Fatalf("注释被冲掉了:\n%s", raw)
	}
	// 结构正确：max_gb 变 200，warn_at 没动
	c, err := config.Load(path, false)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if c.Cache.MaxGB != 200 {
		t.Fatalf("max_gb 没写成 200: %d", c.Cache.MaxGB)
	}
	if c.Cache.WarnAt != 0.9 {
		t.Fatalf("warn_at 不该被改: %v", c.Cache.WarnAt)
	}
	if c.Logging.Level != "info" {
		t.Fatalf("logging.level 不该被改: %v", c.Logging.Level)
	}
}

// TestWriteConfigFileListKey 验证列表键 trusted_proxies 整段重建。
func TestWriteConfigFileListKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	src := `addr: ":8080"
limits:
  enabled: true
  trusted_proxies:
    - 10.0.0.0/8
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigFile(path, map[string]string{
		"limits.trusted_proxies": "192.168.1.0/24\n10.1.1.0/24",
	}); err != nil {
		t.Fatalf("写列表键失败: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "192.168.1.0/24") || !strings.Contains(string(raw), "10.1.1.0/24") {
		t.Fatalf("列表没写进去:\n%s", raw)
	}
	if strings.Contains(string(raw), "10.0.0.0/8") {
		t.Fatalf("旧列表项没被替换:\n%s", raw)
	}
}
