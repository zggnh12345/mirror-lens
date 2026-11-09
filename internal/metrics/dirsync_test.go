package metrics

import (
	"strings"
	"testing"
)

// 目录 fsync 不支持时要能被运维看见，而不是悄悄当成成功。
func TestDirSyncUnsupportedGauge(t *testing.T) {
	r := Enable()
	defer Disable()

	if strings.Contains(string(r.render()), "cache_dir_fsync_unsupported_total") {
		t.Fatal("没发生时不应输出该指标")
	}
	DirSyncUnsupported()
	DirSyncUnsupported()
	out := string(r.render())
	if !strings.Contains(out, "cache_dir_fsync_unsupported_total 2") {
		t.Fatalf("计数不对:\n%s", out)
	}
}
