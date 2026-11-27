package config

import (
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
)

func readExample(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// §15.1：config.example.yaml 的 9 行映射必须与 Default() 逐键逐值一致，
// 不能只检查 key 出现——值被改坏仍应失败。
func TestG4ExampleMapMatchesDefaultValues(t *testing.T) {
	c := Default()
	b := readExample(t)
	s := string(b)
	for host, repo := range c.Probe.HostProbeRepos {
		line := fmt.Sprintf("%s: %s", host, repo)
		// 示例可能带引号，逐一检查 host 与 repo 同时出现在映射段。
		if !strings.Contains(s, host) || !strings.Contains(s, repo) {
			t.Errorf("config.example.yaml 与 Default() 不一致: %s", line)
		}
	}
}

// §15.1：周期 ≤0 或超 time.Duration 上限必须明确报错。
func TestG4ProbeIntervalOverflow(t *testing.T) {
	base := "addr: ':8080'\ndata: /data\nprobe:\n  host_probe_repos: {}\n  host_probe_interval_sec: %d\n  host_probe_scan_sec: 30\n"
	for _, v := range []int64{math.MaxInt64, math.MinInt64} {
		body := fmt.Sprintf(base, v)
		if _, err := Load(writeCfg(t, body), false); err == nil {
			t.Fatalf("周期溢出 %d 应报错", v)
		}
	}
}

// §15.3：更多非法配置键——冲突、带端口/路径/userinfo、空白/点键。
func TestG4HostProbeKeyValidation(t *testing.T) {
	base := "addr: ':8080'\ndata: /data\nprobe:\n  host_probe_repos: %s\n  host_probe_interval_sec: 21600\n  host_probe_scan_sec: 30\n"
	for _, tc := range []struct{ name, repos string }{
		{"大小写冲突", `{"QUAY.IO":"a","quay.io":"b"}`},
		{"带端口", `{"quay.io:5000":"repo"}`},
		{"带路径", `{"quay.io/x":"repo"}`},
		{"带 userinfo", `{"u@quay.io":"repo"}`},
		{"空白键", `{"   ":"repo"}`},
		{"单点键", `{".":"repo"}`},
		{"三点键", `{"...":"repo"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(base, tc.repos)
			if _, err := Load(writeCfg(t, body), false); err == nil {
				t.Fatal("非法键应报错")
			}
		})
	}
}
