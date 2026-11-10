package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/config"
	"github.com/zggnh12345/mirror-lens/internal/secret"
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
	// 星标源降级的两个参数要能在设置页里改
	wantKeys := map[string]string{
		"parallel.degrade_mbps": "4", "parallel.degrade_hold_min": "30",
		"parallel.watch_mbps": "3", "parallel.watch_sec": "10",
		"parallel.switch_min_mbps": "8", "parallel.switch_min_file_mib": "60",
		"parallel.switch_max_sent_mib": "48", "parallel.switch_max": "2",
		"parallel.switch_gap_sec": "30",
	}
	for _, s := range secs {
		if s["title"] != "分片并行" {
			continue
		}
		for _, f := range s["fields"].([]map[string]any) {
			k, _ := f["key"].(string)
			if w, ok := wantKeys[k]; ok {
				if f["value"] != w {
					t.Fatalf("%s 默认值不对，期望 %s 实际 %v", k, w, f["value"])
				}
				delete(wantKeys, k)
			}
		}
	}
	if len(wantKeys) > 0 {
		t.Fatalf("设置页缺少字段: %v", wantKeys)
	}

	// 探测节奏也要能从设置页改
	pkeys := map[string]string{
		"probe.interval_sec": "30", "probe.concurrency": "8",
		"probe.timeout_sec": "45", "probe.sample_mib": "4",
	}
	for _, s := range secs {
		if s["title"] != "探测" {
			continue
		}
		for _, f := range s["fields"].([]map[string]any) {
			k, _ := f["key"].(string)
			if w, ok := pkeys[k]; ok {
				if f["value"] != w {
					t.Fatalf("%s 默认值不对，期望 %s 实际 %v", k, w, f["value"])
				}
				delete(pkeys, k)
			}
		}
	}
	if len(pkeys) > 0 {
		t.Fatalf("设置页缺少探测字段: %v", pkeys)
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
	changed, _, err := applyConfigValues(c, map[string]string{
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
	var field string
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

// TestConfigViewGroupAssignment 分组是前端分栏的唯一依据：漏一个 section
// 或写错组名，那一栏就永远不显示；新增开关也必须能在配置页里看到。
func TestConfigViewGroupAssignment(t *testing.T) {
	v := configView(config.Default(), "")
	groups, ok := v["groups"].([]string)
	if !ok || len(groups) == 0 {
		t.Fatalf("groups 格式不对: %#v", v["groups"])
	}
	known := map[string]bool{}
	for _, g := range groups {
		known[g] = true
	}
	secs := v["sections"].([]map[string]any)
	seen := map[string]bool{}
	for _, s := range secs {
		g, _ := s["group"].(string)
		if !known[g] {
			t.Errorf("section %q 的组 %q 不在 groups 里，前端会漏显示", s["title"], g)
		}
		seen[g] = true
	}
	for _, g := range groups {
		if !seen[g] {
			t.Errorf("组 %q 没有任何 section，点了是空白", g)
		}
	}

	want := map[string]string{
		"network.ip_family":       "enum",
		"registry.docker_enabled": "bool",
		"registry.ghcr_enabled":   "bool",
		"github.enabled":          "bool",
	}
	got := map[string]string{}
	for _, s := range secs {
		for _, f := range s["fields"].([]map[string]any) {
			if k, _ := f["key"].(string); want[k] != "" {
				got[k], _ = f["kind"].(string)
			}
		}
	}
	for k, kind := range want {
		if got[k] != kind {
			t.Errorf("%s 字段缺失或类型不对: %q", k, got[k])
		}
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

// TestGitHubTokenEncryptedAtRest 设置页保存的 Token 不能以明文进配置文件，
// 也不能从 GET 回显；密钥跟数据目录走，能解回原值。
func TestGitHubTokenEncryptedAtRest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	src := "data: " + dir + "\ngithub:\n  enabled: true\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	const tok = "ghp_0123456789abcdefghijklmnopqrstuvwx"
	if err := writeConfigFile(path, map[string]string{"github.token": tok}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), tok) {
		t.Fatalf("配置文件里出现明文 Token:\n%s", raw)
	}
	c, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if c.GitHub.TokenEnc == "" {
		t.Fatal("token_enc 没写进配置")
	}
	key, err := secret.LoadOrCreateKey(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := secret.Decrypt(key, c.GitHub.TokenEnc)
	if err != nil {
		t.Fatal(err)
	}
	if plain != tok {
		t.Fatalf("解出 %q，期望原 Token", plain)
	}

	// GET 不回显内容，只标记 set。
	v := configView(c, path)
	for _, s := range v["sections"].([]map[string]any) {
		for _, f := range s["fields"].([]map[string]any) {
			if f["key"] != "github.token" {
				continue
			}
			if f["value"] != "" {
				t.Fatalf("设置页回显了 Token: %v", f["value"])
			}
			if f["set"] != true {
				t.Fatal("set 标记应为 true")
			}
		}
	}

	// 传空串 = 清除。
	if err := writeConfigFile(path, map[string]string{"github.token": ""}); err != nil {
		t.Fatal(err)
	}
	c2, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if c2.GitHub.TokenEnc != "" {
		t.Fatal("清除后 token_enc 仍非空")
	}
}

// 设置页每一项都要有说明：缺 Hint 的字段在页面上只剩一个标签，用户看不出作用。
// 数值型还必须有单位，否则不知道填的是秒、MB 还是次数。
func TestCfgSchemaHasHintAndUnit(t *testing.T) {
	for _, f := range cfgSchema {
		if strings.TrimSpace(f.Hint) == "" {
			t.Errorf("%s (%s) 缺 Hint", f.Key, f.Label)
		}
		switch f.Kind {
		case "int", "float":
			if f.Unit == "" {
				t.Errorf("%s (%s) 缺 Unit", f.Key, f.Label)
			}
		}
		if f.Kind == "duration" && !strings.Contains(f.Hint, "时间格式") && !strings.Contains(f.Hint, "秒") {
			t.Errorf("%s (%s) 时间类要写明格式或单位", f.Key, f.Label)
		}
	}
}
