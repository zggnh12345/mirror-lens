package discover

import "testing"

func hostsOf(cs []candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.host
	}
	return out
}

func TestParseMarkdown(t *testing.T) {
	body := []byte(`
| 站点 | 地址 |
| --- | --- |
| 官方 | https://registry-1.docker.io |
| 可用 | https://docker.1ms.run/ |
| 裸域 | ` + "`dockerproxy.net`" + ` |
| 带路径 | ` + "`hub.kubesre.xyz/docker.io`" + ` |
| 文档 | https://github.com/dongyubin/DockerHub |
| 噪音 | https://mirrors.aliyun.com/ubuntu |
`)
	got := hostsOf(parseMarkdown(body, feed{name: "t", kind: KindDocker}))
	want := map[string]bool{"docker.1ms.run": true, "dockerproxy.net": true, "hub.kubesre.xyz": true}
	if len(got) != len(want) {
		t.Fatalf("解析出 %v，期望 %v", got, want)
	}
	for _, h := range got {
		if !want[h] {
			t.Fatalf("多出 %s", h)
		}
	}
}

func TestParseMarkdownGitHub(t *testing.T) {
	body := []byte("加速地址：https://gh-proxy.com\n镜像：https://mirror.ghproxy.com\n克隆：`git clone https://user:TOKEN@ghproxy.com/https://github.com/a/b`\n")
	got := hostsOf(parseMarkdown(body, feed{name: "t", kind: KindGitHub}))
	if len(got) != 3 {
		t.Fatalf("解析出 %v，期望 3 个（含 user:token@ 前缀那条）", got)
	}
}

const statusHTML = `<div class="group-content active" id="group-dockerhub">
<label class="mirror-option" data-name="a" data-url="https://docker.1ms.run" data-status="healthy"><input type="checkbox"></label>
<label class="mirror-option" data-name="b" data-url="https://down.example.com" data-status="error"><input type="checkbox"></label>
<label class="mirror-option" data-name="c" data-url="https://registry-1.docker.io" data-status="healthy"><input type="checkbox"></label>
</div>
<div class="group-content" id="group-ghcr">
<label class="mirror-option" data-name="d" data-url="https://ghcr.example.com" data-status="healthy"><input type="checkbox"></label>
</div>`

func TestParseStatusHTML(t *testing.T) {
	got := parseStatusHTML([]byte(statusHTML), feed{name: "t", kind: KindDocker, group: "dockerhub"})
	if len(got) != 1 || got[0].host != "docker.1ms.run" || !got[0].online {
		t.Fatalf("只该拿到在线且非官方的 dockerhub 条目，实际 %+v", got)
	}
}

func TestParseStatusJSON(t *testing.T) {
	body := []byte(`[
 {"url":"https://docker.1ms.run","status":"online"},
 {"url":"https://down.example.com","status":"down"},
 {"url":"https://registry-1.docker.io","status":"online","official":true},
 {"url":"https://inner.example.com","status":"online","access":"private"}
]`)
	got := parseStatusJSON(body, feed{name: "t", kind: KindDocker})
	if len(got) != 1 || got[0].host != "docker.1ms.run" {
		t.Fatalf("实际 %+v", got)
	}
}

func TestHostOf(t *testing.T) {
	cases := map[string]string{
		"https://Docker.1MS.run/v2/":   "docker.1ms.run",
		"http://user:pw@a.b.cn:8080/x": "a.b.cn",
		"dockerproxy.net":              "dockerproxy.net",
	}
	for in, want := range cases {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q)=%q 期望 %q", in, got, want)
		}
	}
}

func TestLooksLikeMirror(t *testing.T) {
	if LooksLikeMirror("mirrors.aliyun.com", KindDocker) {
		t.Error("系统镜像站不该算 docker 加速器")
	}
	if LooksLikeMirror("registry.npmmirror.com", KindDocker) {
		t.Error("npm registry 不该算 docker 加速器")
	}
	if !LooksLikeMirror("docker.1ms.run", KindDocker) {
		t.Error("docker 前缀应通过")
	}
	if LooksLikeMirror("github.com", KindDocker) {
		t.Error("github.com 在黑名单")
	}
	if !LooksLikeMirror("gh-proxy.com", KindGitHub) {
		t.Error("gh-proxy 应通过")
	}
}
