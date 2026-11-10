package api

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 面板每行必须是「站点名 + 域名 / 备注 + 延迟 + 状态条」两段，
// 能力标签只允许 HUB 与 RAW，异常行要给人话原因而不是 Go 原始错误。
// 这里直接跑 index.html 里的真脚本（套一层 DOM 壳），避免测试复制一份渲染逻辑。
func TestPanelRowContract(t *testing.T) {
	raw, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	src := string(raw)

	// 结构断言不依赖 node：两行容器、旧的加号式能力标签、集中状态映射。
	if n := strings.Count(src, `<div class="l1">`); n != 1 {
		t.Errorf("行模板应有且仅有一个 l1 容器，实际 %d", n)
	}
	if n := strings.Count(src, `<div class="l2">`); n != 1 {
		t.Errorf("行模板应有且仅有一个 l2 容器，实际 %d", n)
	}
	if strings.Contains(src, "function capChip(") || strings.Contains(src, "capChip(") {
		t.Error("旧的 capChip 仍在：能力标签必须只出 HUB/RAW，不能再拼 ✓")
	}
	css, err := webFS.ReadFile("web/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	// 在线共用 .st 基类（灰底灰字），没有 .st.ok 就还是灰的。
	if !strings.Contains(string(css), ".st.ok{") || !strings.Contains(string(css), ".st.ok{color:var(--ok)") {
		t.Error("缺少 .st.ok 绿色样式：在线状态会显示成灰色")
	}
	if !strings.Contains(src, "var ST = {") {
		t.Error("缺少集中状态映射 ST")
	}
	for _, st := range []string{"usable", "degraded", "pending", "dead", "blocked", "archived"} {
		if !strings.Contains(src, st+":") {
			t.Errorf("状态映射缺少 %q", st)
		}
	}
	for _, msg := range []string{"仓库地址无效，无法获取镜像清单", "限流，请稍后再试", "域名解析失败", "TLS 证书校验失败"} {
		if !strings.Contains(src, msg) {
			t.Errorf("错误翻译缺少 %q", msg)
		}
	}
	// 摘要格数由接口下发，前端不许再写死 24。
	if strings.Contains(src, "for (var i = 0; i < 24; i++)") {
		t.Error("状态条仍在写死 24 格，应使用后端 recent_n")
	}

	node := os.Getenv("NODE_BIN")
	if node == "" {
		node, err = exec.LookPath("node")
		if err != nil {
			t.Skip("node 不可用，跳过前端渲染校验")
		}
	}

	script := panelShim + extractScript(t, src) + panelDriver
	f := filepath.Join(t.TempDir(), "panel.js")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	out, err := exec.Command(node, f).CombinedOutput()
	if err != nil {
		t.Fatalf("执行面板脚本失败: %v\n%s", err, string(out))
	}
	var got struct {
		L1, L2    int
		Tags      []string
		Tags3     []string
		Cells     int
		NameShown bool
		DeadHuman bool
		DeadZero  bool
		DeadDetl  bool
		Err404    bool
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("输出不是 JSON: %v\n%s", err, string(out))
	}

	if got.L1 != 1 || got.L2 != 1 {
		t.Errorf("每行应恰好两段，实际 l1=%d l2=%d", got.L1, got.L2)
	}
	if strings.Join(got.Tags, ",") != "HUB,RAW" {
		t.Errorf("双能力源应显示 HUB、RAW，实际 %v", got.Tags)
	}
	if strings.Join(got.Tags3, ",") != "HUB" {
		t.Errorf("只有 Docker 能力的源不应显示 RAW，实际 %v", got.Tags3)
	}
	if got.Cells != 24 {
		t.Errorf("状态条应有 24 格，实际 %d", got.Cells)
	}
	if !got.NameShown {
		t.Error("第一段应同时显示站点名和域名")
	}
	if !got.DeadHuman {
		t.Error("解析失败应翻译成「域名解析失败」")
	}
	if got.DeadZero {
		t.Error("异常行不应显示 0ms 延迟")
	}
	if !got.DeadDetl {
		t.Error("异常行应保留技术详情（edetail）供点按查看")
	}
	if !got.Err404 {
		t.Error("404 应翻译成「仓库地址无效，无法获取镜像清单」")
	}
}

// extractScript 取 index.html 里最后一个 <script> 块（面板逻辑）。
func extractScript(t *testing.T, src string) string {
	t.Helper()
	i := strings.LastIndex(src, "<script>")
	j := strings.LastIndex(src, "</script>")
	if i < 0 || j < i {
		t.Fatal("未找到面板脚本块")
	}
	return src[i+len("<script>") : j]
}

// DOM 壳：面板脚本末尾会自启动（拉 /api/me、/api/sources），
// fetch 挂起不返回，保证启动逻辑跑到但不产生副作用，只调用渲染函数。
const panelShim = `
function mkEl(){
  return {
    style:{}, dataset:{}, children:[], value:'', textContent:'', innerHTML:'', checked:false, hidden:false,
    classList:{add:function(){},remove:function(){},toggle:function(){},contains:function(){return false;}},
    addEventListener:function(){}, removeEventListener:function(){},
    appendChild:function(){}, removeChild:function(){}, focus:function(){}, blur:function(){},
    setAttribute:function(){}, getAttribute:function(){return null;},
    querySelector:function(){return mkEl();}, querySelectorAll:function(){return [];},
    closest:function(){return null;}, insertAdjacentHTML:function(){},
    scrollIntoView:function(){}, getBoundingClientRect:function(){return {width:300,height:52,top:0,left:0};},
    getContext:function(){return {clearRect:function(){},beginPath:function(){},moveTo:function(){},lineTo:function(){},
      stroke:function(){},fill:function(){},fillRect:function(){},strokeStyle:'',fillStyle:'',lineWidth:1};},
    width:0, height:0, onclick:null
  };
}
global.document={querySelector:function(){return mkEl();},querySelectorAll:function(){return [];},
  getElementById:function(){return mkEl();},
  addEventListener:function(){},createElement:function(){return mkEl();},body:mkEl(),
  documentElement:{dataset:{}},hidden:false};
global.window={matchMedia:function(){return {matches:false,addEventListener:function(){}};},addEventListener:function(){}};
global.localStorage={getItem:function(){return null;},setItem:function(){},removeItem:function(){}};
global.navigator={clipboard:{writeText:function(){return Promise.resolve();}}};
global.location={href:'http://x/',reload:function(){}};
global.setInterval=function(){return 0;};
global.fetch=function(){return new Promise(function(){});};
`

const panelDriver = `
var out={};
function base(o){
  var x={id:1,base_url:'https://docker.1ms.run',site_name:'毫秒镜像',name:'docker.1ms.run',status:'usable',
    enabled:true,docker_cap:1,github_cap:1,note:'实测可用',origin:'seed',
    last:{docker:{ok:true,latency_ms:180,speed_kbps:9000}},recent:[],uptime:{docker:0.99},
    p95_ms:{docker:300},rate429:{docker:0.02},trust:{docker:{level:2,digest7:7,bad7:0}}};
  for (var k in o) x[k]=o[k];
  return x;
}
function tagsOf(h){
  return (h.match(/class="cap on [a-z]+">[A-Z]+</g)||[]).map(function(t){return t.replace(/.*>/,'').replace('<','');});
}
var items=[]; for(var i=0;i<24;i++) items.push({kind:i%2?'github':'docker',ts:1700000000+i*60,ok:i!==5,latency_ms:200});
var h1=rowHtml(base({recent:items}));
out.L1=(h1.match(/class="l1"/g)||[]).length;
out.L2=(h1.match(/class="l2"/g)||[]).length;
out.Tags=tagsOf(h1);
out.Cells=(h1.match(/class="pip/g)||[]).length;
out.NameShown=h1.indexOf('毫秒镜像')>=0 && h1.indexOf('docker.1ms.run')>=0;
var h2=rowHtml(base({status:'dead',last:{docker:{ok:false,error:'Get "https://docker.1ms.run/v2/": dial tcp: lookup docker.1ms.run on 10.1.1.3:53: no such host'}}}));
out.DeadHuman=h2.indexOf('域名解析失败')>=0;
out.DeadZero=h2.indexOf('0ms')>=0;
out.DeadDetl=h2.indexOf('edetail')>=0;
out.Tags3=tagsOf(rowHtml(base({status:'degraded',github_cap:0})));
out.Err404=rowHtml(base({status:'dead',last:{docker:{ok:false,error:'unexpected status 404 Not Found'}}})).indexOf('仓库地址无效')>=0;
console.log(JSON.stringify(out));
`

// 实时区是「让人感觉到在加速」的唯一入口：必须有 DOM 容器、真的去拉
// /api/accel/active、并且 2 秒一刷（15 秒那种刷新看不出进度条在动）。
func TestPanelLiveContract(t *testing.T) {
	raw, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	src := string(raw)
	for _, want := range []string{`id="liveSec"`, `id="lvRows"`, `id="lvThru"`, "/api/accel/active", "function renderLive(", "loadLive()"} {
		if !strings.Contains(src, want) {
			t.Errorf("实时区缺少 %q", want)
		}
	}
	for _, want := range []string{"刚刚完成", `id="lvTitle"`, "d.now"} {
		if !strings.Contains(src, want) {
			t.Errorf("任务刚结束就整块消失，缺少 %q", want)
		}
	}
	if !strings.Contains(src, "loadLive(); }, 2000)") {
		t.Error("实时区必须 2 秒一刷，否则进度条和速度看着是死的")
	}
	if !strings.Contains(src, "app.css?v=14") {
		t.Error("改了样式却没升 app.css 版本号，浏览器会继续用旧缓存")
	}
	css, err := webFS.ReadFile("web/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	for _, want := range []string{".lvbar i{", ".lvthru b{color:var(--ok)", ".lvtag.p{"} {
		if !strings.Contains(string(css), want) {
			t.Errorf("实时区样式缺少 %q", want)
		}
	}
	// 路由必须挂在登录墙后面：任务名会暴露用户在拉什么镜像。
	api, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatalf("read api.go: %v", err)
	}
	if !strings.Contains(string(api), `mux.HandleFunc("/api/accel/active", guard(s.accelActive))`) {
		t.Error("/api/accel/active 必须经过 guard")
	}
}
