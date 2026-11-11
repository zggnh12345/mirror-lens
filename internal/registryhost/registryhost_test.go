package registryhost

import "testing"

func TestNormalize(t *testing.T) {
	cases := []struct {
		in, want string
		bad      bool
	}{
		{"", "", false},
		{"  docker.io ", "", false},
		{"registry-1.docker.io", "", false},
		{"DOCKER.IO.", "", false},
		{"ghcr.io", "ghcr.io", false},
		{"GHCR.IO.", "ghcr.io", false},
		{"lscr.io", "lscr.io", false},
		{"registry.gitlab.com", "registry.gitlab.com", false},
		// 非法：协议、路径、端口、IP、空标签
		{"https://ghcr.io", "", true},
		{"ghcr.io/owner", "", true},
		{"ghcr.io:443", "", true},
		{"10.0.0.1", "", true},
		{"ghcr..io", "", true},
		{"-ghcr.io", "", true},
		{"ghcr.io?x=1", "", true},
	}
	for _, c := range cases {
		got, err := Normalize(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("Normalize(%q) 应报错，得到 %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Normalize(%q) 意外报错: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Normalize(%q)=%q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestCheckWhitelist(t *testing.T) {
	if h, err := Check(""); err != nil || h != "" {
		t.Fatalf("Hub 应通过: %q %v", h, err)
	}
	if h, err := Check("GHCR.IO"); err != nil || h != "ghcr.io" {
		t.Fatalf("ghcr 应规范化通过: %q %v", h, err)
	}
	if _, err := Check("nvcr.io"); err == nil {
		t.Fatal("未入白名单的 Host 应被拒绝")
	}
}

// 白名单要么被 adapter 引用，要么就没意义——这条测试保证列表不空且含常用站。
func TestHostsCopy(t *testing.T) {
	hs := Hosts()
	if len(hs) != 11 {
		t.Fatalf("白名单数量变化：%d", len(hs))
	}
	hs[0] = "mutated"
	if Hosts()[0] == "mutated" {
		t.Fatal("Hosts() 必须返回副本，否则调用方能改掉全局白名单")
	}
	for _, h := range []string{"ghcr.io", "quay.io", "registry.k8s.io"} {
		if !Allowed(h) {
			t.Errorf("白名单缺少 %s", h)
		}
	}
}
