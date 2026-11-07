package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/config"
)

// 面板保存信任中转参数后要能落盘并被重新加载，别只改了内存。
func TestSaveRelayConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	s := newCfgServer(t, path)

	if _, _, err := s.saveConfig(map[string]string{
		"relay.pin_grace_hours":  "48",
		"relay.resolver_timeout": "8s",
	}); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, want := range []string{"relay:", "pin_grace_hours: 48", "resolver_timeout: 8s"} {
		if !strings.Contains(text, want) {
			t.Fatalf("落盘内容缺少 %q：\n%s", want, text)
		}
	}

	c, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if c.Relay.PinGraceHours != 48 {
		t.Fatalf("重新加载后 pin_grace_hours=%d", c.Relay.PinGraceHours)
	}
	if c.Relay.ResolverTimeout.Duration != 8*time.Second {
		t.Fatalf("重新加载后 resolver_timeout=%s", c.Relay.ResolverTimeout.Duration)
	}
}

// 越界的迁移窗口要在保存时被拒，而不是写进文件等重启才报错。
func TestSaveRelayRejectsOutOfRange(t *testing.T) {
	s := newCfgServer(t, filepath.Join(t.TempDir(), "config.yaml"))
	if _, _, err := s.saveConfig(map[string]string{"relay.pin_grace_hours": "9999"}); err == nil {
		t.Fatal("超过上限应报错")
	}
}
