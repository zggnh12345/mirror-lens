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
// 能力标签按 RepoHost 派生（HUB/GHCR/第一批 Foreign Registry），异常行要给人话原因。
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
		L1, L2            int
		Tags              []string
		Tags3             []string
		TagsEmpty         []string
		TagsGHCR          []string
		TagsGCR           []string
		TagsPKG           []string
		TagsNoProbe       []string
		TagsForeignNotHUB []string
		Cells             int
		NameShown         bool
		DeadHuman         bool
		DeadZero          bool
		DeadDetl          bool
		Err404            bool
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
	if len(got.TagsEmpty) != 0 {
		t.Errorf("repo_host 为空时不得推断 HUB/GHCR，实际 %v", got.TagsEmpty)
	}
	if strings.Join(got.TagsGHCR, ",") != "GHCR" {
		t.Errorf("ghcr.io 源应显示 GHCR，实际 %v", got.TagsGHCR)
	}
	if strings.Join(got.TagsGCR, ",") != "GCR" {
		t.Errorf("gcr.io 已探测源应只显示 GCR，实际 %v", got.TagsGCR)
	}
	if strings.Join(got.TagsPKG, ",") != "PKG" {
		t.Errorf("Artifact Registry 应显示 PKG，实际 %v", got.TagsPKG)
	}
	if len(got.TagsNoProbe) != 0 {
		t.Errorf("未探测的 Foreign Host 不应显示标签，实际 %v", got.TagsNoProbe)
	}
	if strings.Join(got.TagsForeignNotHUB, ",") != "GCR" {
		t.Errorf("Foreign Host 即使 docker_cap=1 也只能显示归属标签，实际 %v", got.TagsForeignNotHUB)
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

// 多源 Host 是固定 8 个独立开关，不再给自由文本；宽屏 4 列、窄屏 2 列。
func TestSettingsMultiOriginSwitchContract(t *testing.T) {
	raw, err := webFS.ReadFile("web/settings.html")
	if err != nil {
		t.Fatalf("read settings.html: %v", err)
	}
	src := string(raw)
	for _, want := range []string{
		"var MOH_HOSTS", "data-moh", "moh-grid", "function mohHtml(",
		"k === 'registry.multi_origin_hosts'", "MOH_EXTRA",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("多源开关缺少 %q", want)
		}
	}
	for _, want := range []string{"gcr.io", "registry.k8s.io", "*.pkg.dev", "quay.io",
		"mcr.microsoft.com", "registry.gitlab.com", "public.ecr.aws", "cgr.dev"} {
		if !strings.Contains(src, want) {
			t.Errorf("多源开关缺少 Host %q", want)
		}
	}
	css, err := webFS.ReadFile("web/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	for _, want := range []string{
		".moh-grid{display:grid;grid-template-columns:repeat(4",
		"@media(max-width:520px){.moh-grid{grid-template-columns:repeat(2",
		".moh-item .sw input:checked + span::after{transform:translateX(16px)}",
	} {
		if !strings.Contains(string(css), want) {
			t.Errorf("多源开关样式缺少 %q", want)
		}
	}
}

// 多源开关只覆盖固定 8 个 Host；额外 Host 必须保留，并明确它们仍可能参与多源。
func TestSettingsMultiOriginExtraContract(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node 不可用，跳过前端渲染校验")
	}
	raw, err := webFS.ReadFile("web/settings.html")
	if err != nil {
		t.Fatalf("read settings.html: %v", err)
	}
	src := string(raw)
	start := strings.Index(src, "var MOH_HOSTS")
	if start < 0 {
		t.Fatal("settings.html 未找到 MOH_HOSTS")
	}
	end := strings.Index(src[start:], "function cfgControl")
	if end < 0 {
		t.Fatal("settings.html 未找到 cfgControl")
	}
	snippet := src[start : start+end]

	script := `var esc=function(s){return String(s==null?'':s);};
` + snippet + `
var out={};
out.empty=mohHtml('');
out.fixed=mohHtml('gcr.io\nextra.example');
out.extra=mohHtml('extra.example');
out.extraAfter=MOH_EXTRA.slice();
console.log(JSON.stringify(out));
`
	f := filepath.Join(t.TempDir(), "moh.js")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	out, err := exec.Command(node, f).CombinedOutput()
	if err != nil {
		t.Fatalf("执行 mohHtml 失败: %v\n%s", err, string(out))
	}
	var got struct {
		Empty, Fixed, Extra string
		ExtraAfter          []string
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("输出不是 JSON: %v\n%s", err, string(out))
	}
	if strings.Count(got.Empty, "data-moh=") != 8 {
		t.Errorf("无额外 Host 时应渲染 8 个开关，实际 %q", got.Empty)
	}
	if strings.Contains(got.Empty, "当前配置里还有") {
		t.Error("无额外 Host 时不应显示额外 Host 提示")
	}
	if !strings.Contains(got.Fixed, "extra.example") || !strings.Contains(got.Fixed, "仍可能参与多源") {
		t.Errorf("有额外 Host 时必须提示仍可能参与多源，实际 %q", got.Fixed)
	}
	if !strings.Contains(got.Extra, "extra.example") || !strings.Contains(got.Extra, "仍可能参与多源") {
		t.Errorf("全关固定开关时额外 Host 仍须提示，实际 %q", got.Extra)
	}
	if len(got.ExtraAfter) != 1 || got.ExtraAfter[0] != "extra.example" {
		t.Errorf("额外 Host 必须保留，实际 %v", got.ExtraAfter)
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
  return (h.match(/class="cap on [a-z]+"[^>]*>[A-Z]+</g)||[]).map(function(t){return t.replace(/.*>/,'').replace('<','');});
}
var items=[]; for(var i=0;i<24;i++) items.push({kind:i%2?'github':'docker',ts:1700000000+i*60,ok:i!==5,latency_ms:200});
var h1=rowHtml(base({repo_host:'docker.io',recent:items}));
out.L1=(h1.match(/class="l1"/g)||[]).length;
out.L2=(h1.match(/class="l2"/g)||[]).length;
out.Tags=tagsOf(h1);
out.Cells=(h1.match(/class="pip/g)||[]).length;
out.NameShown=h1.indexOf('毫秒镜像')>=0 && h1.indexOf('docker.1ms.run')>=0;
var h2=rowHtml(base({status:'dead',last:{docker:{ok:false,error:'Get "https://docker.1ms.run/v2/": dial tcp: lookup docker.1ms.run on 10.1.1.3:53: no such host'}}}));
out.DeadHuman=h2.indexOf('域名解析失败')>=0;
out.DeadZero=h2.indexOf('0ms')>=0;
out.DeadDetl=h2.indexOf('edetail')>=0;
out.Tags3=tagsOf(rowHtml(base({repo_host:'docker.io',status:'degraded',github_cap:0})));
out.TagsEmpty=tagsOf(rowHtml(base({repo_host:'',docker_cap:1,ghcr_cap:1,github_cap:0})));
out.TagsGHCR=tagsOf(rowHtml(base({repo_host:'ghcr.io',docker_cap:0,ghcr_cap:1,github_cap:0})));
out.TagsGCR=tagsOf(rowHtml(base({repo_host:'gcr.io',registry_cap:1,registry_path_mode:'native',docker_cap:0,github_cap:0})));
out.TagsPKG=tagsOf(rowHtml(base({repo_host:'asia-northeast1-docker.pkg.dev',registry_cap:1,registry_path_mode:'host_prefix',docker_cap:0,github_cap:0})));
out.TagsNoProbe=tagsOf(rowHtml(base({repo_host:'gcr.io',registry_cap:-1,registry_path_mode:'',docker_cap:0,github_cap:0})));
out.TagsForeignNotHUB=tagsOf(rowHtml(base({repo_host:'gcr.io',registry_cap:1,registry_path_mode:'native',docker_cap:1,github_cap:0})));
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
	if !strings.Contains(src, "app.css?v=16") {
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
