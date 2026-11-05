package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// 默认配置整份序列化后，所有键都必须是已知字段（不能误报）。
func TestUnknownYAMLFieldsNoFalsePositive(t *testing.T) {
	out, err := yaml.Marshal(Default())
	if err != nil {
		t.Fatal(err)
	}
	if got := unknownYAMLFields(out); len(got) != 0 {
		t.Fatalf("默认配置被误报为未知字段: %v", got)
	}
}

// 拼错的键和未来字段要给出完整路径。
func TestUnknownYAMLFieldsReportsPaths(t *testing.T) {
	src := strings.Join([]string{
		"cache:",
		"  max_gb: 10",
		"  max_gbb: 20", // 拼写错误
		"future_key: 1",
		"raw:",
		"  ttl: 5m",
		"  ttl_extra: 1m",
		"",
	}, "\n")
	got := unknownYAMLFields([]byte(src))
	want := []string{"cache.max_gbb", "future_key", "raw.ttl_extra"}
	if len(got) != len(want) {
		t.Fatalf("未知字段=%v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("未知字段=%v，期望 %v", got, want)
		}
	}
}

// 嵌套结构的合法字段不能被当成未知（含 limits.trusted_proxies 这类切片）。
func TestUnknownYAMLFieldsNestedKnown(t *testing.T) {
	src := strings.Join([]string{
		"limits:",
		"  trusted_proxies:",
		"    - 10.0.0.0/8",
		"janitor:",
		"  interval: 10m",
		"",
	}, "\n")
	if got := unknownYAMLFields([]byte(src)); len(got) != 0 {
		t.Fatalf("合法嵌套字段被误报: %v", got)
	}
}

// 顶层不是 mapping（比如空文档或纯标量）时不应 panic。
func TestUnknownYAMLFieldsNonMapping(t *testing.T) {
	if got := unknownYAMLFields([]byte("# 只有注释\n")); len(got) != 0 {
		t.Fatalf("空文档返回 %v", got)
	}
	if got := unknownYAMLFields([]byte("just-a-scalar\n")); len(got) != 0 {
		t.Fatalf("标量文档返回 %v", got)
	}
}
