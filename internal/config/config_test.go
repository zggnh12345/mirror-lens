package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaults(t *testing.T) {
	c, err := Load("", false)
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":8080" || c.Data != "/data" {
		t.Fatalf("默认地址不对: %s %s", c.Addr, c.Data)
	}
	if c.Cache.MaxFileMB != 256 || c.Cache.MaxGB != 100 {
		t.Fatalf("默认容量不对: %+v", c.Cache)
	}
	if !c.Registry.Enabled || c.Registry.Upstream != "registry-1.docker.io" {
		t.Fatalf("默认上游不对: %+v", c.Registry)
	}
	if c.Git.TTL.Duration != 30*time.Second || c.Raw.TTL.Duration != 5*time.Minute {
		t.Fatalf("默认 TTL 不对: %v %v", c.Git.TTL, c.Raw.TTL)
	}
	if got := c.CacheDir(); got != "/data/cache" {
		t.Fatalf("缓存目录: %s", got)
	}
}

// 只写几个字段，其余保持默认——配置文件不该要求写全。
func TestPartialFileKeepsDefaults(t *testing.T) {
	p := write(t, `
addr: ":9000"
cache:
  max_gb: 20
registry:
  upstream: "ghcr.io"
`)
	c, err := Load(p, true)
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":9000" {
		t.Fatalf("addr: %s", c.Addr)
	}
	if c.Cache.MaxGB != 20 {
		t.Fatalf("max_gb: %d", c.Cache.MaxGB)
	}
	if c.Cache.MaxFileMB != 256 {
		t.Fatalf("没写的字段应保持默认，实际 %d", c.Cache.MaxFileMB)
	}
	if c.Registry.Upstream != "ghcr.io" {
		t.Fatalf("upstream: %s", c.Registry.Upstream)
	}
	if c.Registry.MaxFileMB != 4096 {
		t.Fatalf("registry 默认值被吃了: %d", c.Registry.MaxFileMB)
	}
}

func TestDurationForms(t *testing.T) {
	p := write(t, `
raw:
  ttl: 300
git:
  ttl: 1m30s
`)
	c, err := Load(p, true)
	if err != nil {
		t.Fatal(err)
	}
	if c.Raw.TTL.Duration != 300*time.Second {
		t.Fatalf("纯数字应按秒: %v", c.Raw.TTL)
	}
	if c.Git.TTL.Duration != 90*time.Second {
		t.Fatalf("1m30s: %v", c.Git.TTL)
	}
}

func TestInvalidValues(t *testing.T) {
	cases := map[string]string{
		"带协议的上游": "registry:\n  upstream: \"https://registry-1.docker.io\"\n",
		"带路径的上游": "registry:\n  upstream: \"registry-1.docker.io/v2\"\n",
		"负数容量":   "cache:\n  max_gb: -1\n",
		"负数 TTL": "git:\n  ttl: -5s\n",
		"空上游":    "registry:\n  enabled: true\n  upstream: \"\"\n",
	}
	for name, body := range cases {
		if _, err := Load(write(t, body), true); err == nil {
			t.Fatalf("%s 应该报错", name)
		}
	}
}

// 显式指定了路径却读不到，必须报错——打错文件名却静默用默认值最难查。
func TestMissingExplicitFileFails(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml"), true); err == nil {
		t.Fatal("显式路径不存在应报错")
	}
}

func TestRegistryDisabledSkipsUpstreamCheck(t *testing.T) {
	p := write(t, "registry:\n  enabled: false\n  upstream: \"\"\n")
	if _, err := Load(p, true); err != nil {
		t.Fatalf("关掉 Docker 加速时不该要求上游: %v", err)
	}
}
