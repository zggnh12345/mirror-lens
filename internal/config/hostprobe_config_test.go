package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostProbeDefaultsAndCanonical(t *testing.T) {
	c := Default()
	if len(c.Probe.HostProbeRepos) != 9 {
		t.Fatalf("默认映射 9 行，得到 %d", len(c.Probe.HostProbeRepos))
	}
	if c.Probe.HostProbeIntervalSec != 21600 || c.Probe.HostProbeScanSec != 30 {
		t.Fatalf("默认周期=(%d,%d)", c.Probe.HostProbeIntervalSec, c.Probe.HostProbeScanSec)
	}
	if _, ok := c.Probe.HostProbeRepos["*.pkg.dev"]; !ok {
		t.Fatal("默认映射缺 *.pkg.dev")
	}
	found := false
	for _, h := range c.Registry.MultiOriginHosts {
		if h == "lscr.io" {
			found = true
		}
	}
	if !found {
		t.Fatal("multi_origin_hosts 默认缺 lscr.io")
	}
}

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHostProbeFailFast(t *testing.T) {
	// 缺任一必需字段：启动报错，不从 Default 静默补齐。
	for _, body := range []string{
		"probe:\n  host_probe_interval_sec: 21600\n  host_probe_scan_sec: 30\n",
		"probe:\n  host_probe_repos: {}\n  host_probe_scan_sec: 30\n",
		"probe:\n  host_probe_repos: {}\n  host_probe_interval_sec: 21600\n",
	} {
		if _, err := Load(writeCfg(t, body), false); err == nil {
			t.Fatalf("缺字段应 fail-fast，body=%q", body)
		}
	}
}

func TestHostProbeExplicitEmptyMap(t *testing.T) {
	body := "addr: ':8080'\ndata: /data\nprobe:\n  host_probe_repos: {}\n  host_probe_interval_sec: 21600\n  host_probe_scan_sec: 30\n"
	c, err := Load(writeCfg(t, body), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Probe.HostProbeRepos) != 0 {
		t.Fatalf("显式空 map 不应回退默认，得到 %d 行", len(c.Probe.HostProbeRepos))
	}
}

func TestHostProbeValidation(t *testing.T) {
	base := "addr: ':8080'\ndata: /data\nprobe:\n  host_probe_repos: %s\n  host_probe_interval_sec: %d\n  host_probe_scan_sec: %d\n"
	cases := []struct {
		name           string
		repos          string
		interval, scan int
	}{
		{"空键", `{"": "repo"}`, 21600, 30},
		{"其他通配", `{"*.*.pkg.dev": "repo"}`, 21600, 30},
		{"空仓库", `{"quay.io": ""}`, 21600, 30},
		{"周期零", `{}`, 0, 30},
		{"周期负", `{}`, 21600, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(base, tc.repos, tc.interval, tc.scan)
			if _, err := Load(writeCfg(t, body), false); err == nil {
				t.Fatal("非法配置应报错")
			}
		})
	}
}

func TestHostProbeExampleMatchesDefault(t *testing.T) {
	b, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, key := range []string{"host_probe_repos:", "host_probe_interval_sec: 21600", "host_probe_scan_sec: 30"} {
		if !strings.Contains(s, key) {
			t.Fatalf("config.example.yaml 缺 %s", key)
		}
	}
}

func TestRequireHostProbeFieldsExported(t *testing.T) {
	if err := RequireHostProbeFields([]byte("probe:\n  host_probe_repos: {}\n  host_probe_interval_sec: 21600\n  host_probe_scan_sec: 30\n")); err != nil {
		t.Fatalf("完整配置应通过: %v", err)
	}
	if err := RequireHostProbeFields([]byte("probe:\n  host_probe_repos: {}\n")); err == nil {
		t.Fatal("缺字段应报错")
	}
}
