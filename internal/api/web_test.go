package api

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestWebUIRegressions 锁定三处 UI 修正：
// 1) 监测页错误用 .errchip 中文提示，点击展开技术参数；
// 2) 不可用节点统一 chip 样式，不再用裸红字 .err；
// 3) 设置页 Tab 用 .active 类切换，不依赖 [hidden]。
func TestWebUIRegressions(t *testing.T) {
	settings, err := webFS.ReadFile("web/settings.html")
	if err != nil {
		t.Fatalf("read settings.html: %v", err)
	}
	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	css, err := webFS.ReadFile("web/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}

	s, ix, c := string(settings), string(index), string(css)

	// 设置页账号面板默认 active，Tab 切换基于 .active 类。
	if !strings.Contains(s, `<div data-panel="account" class="active">`) {
		t.Error("账号面板应默认带 class=\"active\"")
	}
	if !strings.Contains(s, `classList.toggle('active'`) {
		t.Error("Tab 切换应基于 .active 类，而非 [hidden] 属性")
	}
	if strings.Contains(s, `p.hidden = (p.dataset.panel`) {
		t.Error("不应再依赖 hidden 属性切换面板（脆弱，部分 webview 不生效）")
	}
	if !strings.Contains(c, `[data-panel].active`) {
		t.Error("app.css 应定义 [data-panel].active 显示规则")
	}

	// 任何面板都不该带 hidden（旧内核对 hidden 支持不稳）。
	for _, p := range []string{"account", "appearance", "probe", "accel", "config", "about"} {
		if strings.Contains(s, `data-panel="`+p+`" hidden`) {
			t.Errorf("面板 %s 不应再带 hidden 属性", p)
		}
	}
	// matchMedia 要兼容旧内核：必须留 addListener 兜底。
	if strings.Contains(s, `matchMedia('(prefers-color-scheme:dark)').addEventListener`) {
		t.Error("matchMedia 不得直接调用 addEventListener（旧 webview 不支持会中断脚本）")
	}
	if !strings.Contains(s, `addListener`) {
		t.Error("matchMedia 应保留 addListener 兜底以兼容旧内核")
	}

	// 监测页错误用 .errchip，可展开原始技术参数，不再用裸红字 .err。
	if !strings.Contains(ix, `function errHuman(`) {
		t.Error("index.html 应定义 errHuman 中文错误映射")
	}
	if !strings.Contains(ix, `errchip`) {
		t.Error("index.html 应使用 .errchip 友好错误样式")
	}
	if strings.Contains(ix, `<span class="err"`) {
		t.Error("不应再使用原始红字 .err 渲染错误")
	}
}

// TestWebUISyntax 解析内嵌 JS，防止语法错误让整段脚本失效。
// 依赖 node，环境无 node 时跳过。
func TestWebUISyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node 不可用，跳过 JS 语法校验")
	}
	re := regexp.MustCompile(`<script>(.*?)</script>`)
	for _, name := range []string{"web/settings.html", "web/index.html"} {
		raw, err := webFS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, b := range re.FindAllStringSubmatch(string(raw), -1) {
			f := filepath.Join(t.TempDir(), fmt.Sprintf("%s_%d.js", filepath.Base(name), i))
			if err := os.WriteFile(f, []byte(b[1]), 0o644); err != nil {
				t.Fatalf("write temp: %v", err)
			}
			out, err := exec.Command(node, "--check", f).CombinedOutput()
			if err != nil {
				t.Errorf("%s 脚本块%d 语法错误:\n%s", name, i, string(out))
			}
		}
	}
}
