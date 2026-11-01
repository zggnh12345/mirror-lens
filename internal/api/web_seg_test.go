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

// TestSegSelectedStateRendered 用 node 真正跑一遍设置页的 cfgControl，
// 检查开/关分段按钮的选中态落在 class 上。
//
// 背景（真实 bug）：按钮 HTML 曾经拼成 `<button ... data-v="true" on>`，
// " on" 变成了裸属性 on 而不是 class，于是任何按钮都拿不到 .seg button.on，
// 界面上开/关都是白底，看上去"既没有开也没有关"。
//
// 纯静态断言容易漏（写法换一种照样错），所以这里直接执行页面里的 JS 函数，
// 再用正则检查产出的 HTML。
func TestSegSelectedStateRendered(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node 不可用，跳过前端渲染校验")
	}
	raw, err := webFS.ReadFile("web/settings.html")
	if err != nil {
		t.Fatalf("read settings.html: %v", err)
	}
	s := string(raw)

	// 抽出 cfgControl 函数体（含 esc 依赖），拼一个最小运行环境执行。
	start := strings.Index(s, "function cfgControl(f){")
	if start < 0 {
		t.Fatal("settings.html 未找到 cfgControl")
	}
	end := strings.Index(s[start:], "\n}\n")
	if end < 0 {
		t.Fatal("无法定位 cfgControl 结尾")
	}
	fn := s[start : start+end+3]

	script := `
var esc = function(s){return String(s==null?'':s);};
` + fn + `
var out = [];
out.push(cfgControl({key:'k.bool', kind:'bool', value:'true'}));
out.push(cfgControl({key:'k.bool2', kind:'bool', value:'false'}));
out.push(cfgControl({key:'k.enum', kind:'enum', value:'warn', options:['debug','info','warn','error']}));
console.log(out.join('\n'));
`
	f := filepath.Join(t.TempDir(), "cfg.js")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	out, err := exec.Command(node, f).CombinedOutput()
	if err != nil {
		t.Fatalf("执行 cfgControl 失败: %v\n%s", err, string(out))
	}
	html := string(out)

	btnRe := regexp.MustCompile(`<button[^>]*>`)
	btns := btnRe.FindAllString(html, -1)
	if len(btns) != 8 {
		t.Fatalf("期望渲染 8 个分段按钮（2 个 bool + 1 个 4 选项 enum），实际 %d 个:\n%s", len(btns), html)
	}

	type want struct {
		val    string
		marked bool
	}
	wants := []want{
		{"true", true}, {"false", false},
		{"true", false}, {"false", true},
		{"debug", false}, {"info", false}, {"warn", true}, {"error", false},
	}
	for i, w := range wants {
		b := btns[i]
		if !strings.Contains(b, `data-v="`+w.val+`"`) {
			t.Errorf("第 %d 个按钮 data-v 不是 %q: %s", i, w.val, b)
			continue
		}
		hasOn := strings.Contains(b, `class="`) && regexp.MustCompile(`class="[^"]*\bon\b[^"]*"`).MatchString(b)
		if hasOn != w.marked {
			t.Errorf("第 %d 个按钮（data-v=%q）选中态期望 %v，实际 %v: %s", i, w.val, w.marked, hasOn, b)
		}
		// 裸属性 on（<button ... on>）会让 HTML 语义错误，必须不存在。
		if regexp.MustCompile(`\son(\s|>)`).MatchString(b) {
			t.Errorf("第 %d 个按钮出现裸属性 on，选中态应写在 class 里: %s", i, b)
		}
	}

	// CSS 必须给 .seg button.on 一个明显的底色，否则开/关仍分不清。
	css, err := webFS.ReadFile("web/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	if !strings.Contains(string(css), ".seg button.on{background:var(--brand)") {
		t.Error("app.css 缺少 .seg button.on 的品牌色底，未选中与选中无法区分")
	}
	_ = fmt.Sprint()
}
