package config

import "testing"

func TestNetworkAndChannelDefaults(t *testing.T) {
	c := Default()
	if c.Network.IPFamily != "auto" {
		t.Fatalf("默认 IP 族应为 auto，得到 %q", c.Network.IPFamily)
	}
	if !c.Registry.Docker || !c.Registry.GHCR {
		t.Fatalf("默认应同时开启 docker/ghcr: %+v", c.Registry)
	}
	if !c.GitHub.Enabled {
		t.Fatalf("默认应开启 GitHub 加速")
	}
}

func TestIPFamilyValidate(t *testing.T) {
	for _, ok := range []string{"", "auto", "v4", "v6"} {
		c := Default()
		c.Network.IPFamily = ok
		if err := c.Validate(); err != nil {
			t.Fatalf("%q 应通过校验: %v", ok, err)
		}
	}
	c := Default()
	c.Network.IPFamily = "v5"
	if err := c.Validate(); err == nil {
		t.Fatalf("v5 应被拒绝")
	}
	// 老配置文件里没有这个键：零值要被归一成 auto，而不是留空。
	c = Default()
	c.Network.IPFamily = ""
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Network.IPFamily != "auto" {
		t.Fatalf("空值应归一为 auto，得到 %q", c.Network.IPFamily)
	}
}
