package api

import (
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/config"
)

// TestConfigOptionsAlwaysHaveSelection 保证 bool/enum 取值都对上选项。
// 对不上时面板一个按钮都不亮，看起来像「没设置」。
func TestConfigOptionsAlwaysHaveSelection(t *testing.T) {
	c := config.Default()
	for _, f := range cfgSchema {
		if f.Kind != "bool" && f.Kind != "enum" {
			continue
		}
		opts := f.Options
		if f.Kind == "bool" {
			opts = []string{"true", "false"}
		}
		v := f.Get(c)
		hit := false
		for _, o := range opts {
			if v == o {
				hit = true
				break
			}
		}
		if !hit {
			t.Errorf("%s（%s）取值 %q 不在选项 %v 里，面板会一个都不选中", f.Key, f.Label, v, opts)
		}
	}
}

// TestConfigEnumNormalizesAliases 覆盖别名枚举：空值或 warning
// 运行时按 info/warn 解释，面板要显示同一个值。
func TestConfigEnumNormalizesAliases(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"", "info"},
		{"warning", "warn"},
		{"WARN", "warn"},
		{" error ", "error"},
	}
	for _, cs := range cases {
		if got := effLevel(cs.raw); got != cs.want {
			t.Errorf("effLevel(%q) = %q，期望 %q", cs.raw, got, cs.want)
		}
	}
	fcases := []struct {
		raw  string
		want string
	}{
		{"", "text"},
		{"JSON", "json"},
		{"xml", "text"},
	}
	for _, cs := range fcases {
		if got := effFormat(cs.raw); got != cs.want {
			t.Errorf("effFormat(%q) = %q，期望 %q", cs.raw, got, cs.want)
		}
	}
}
