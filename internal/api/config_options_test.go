package api

import (
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/config"
)

// TestConfigOptionsAlwaysHaveSelection 保证设置面板里每个 bool/enum 字段
// 的取值都能对上某个选项。
//
// 背景：面板把 bool/enum 渲染成一排分段按钮，靠「当前值 == 某个选项」来点亮选中态。
// 一旦取值对不上所有选项（空串、别名等），界面上就一个都不亮，用户看到的是
// "既没有开也没有关"，而系统里它其实有明确生效值。
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

// TestConfigEnumNormalizesAliases 覆盖日志级别/格式这类有别名的枚举：
// 配置里写空值或 warning，运行时按 info/warn 解释，面板必须显示同一个值。
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
