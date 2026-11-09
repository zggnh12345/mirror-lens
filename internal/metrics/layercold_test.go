package metrics

import (
	"strings"
	"testing"
)

// 冷层指标是观测契约：面板和告警都读这几个名字，改名要一起改。
func TestLayerColdGauges(t *testing.T) {
	r := New()
	prev := current.Load()
	current.Store(r)
	defer current.Store(prev)

	SetLayerCold(1500, 1800, 2, 3)
	out := string(r.render())

	for _, want := range []string{
		"mirrorlens_layer_cold_bytes 1500",
		"mirrorlens_layer_total_bytes 1800",
		"mirrorlens_layer_cold_entries 2",
		"mirrorlens_layer_total_entries 3",
		"mirrorlens_layer_cold_ratio 0.8333",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺指标 %q\n%s", want, out)
		}
	}
	if cold, total, _, _, ok := LayerColdView(); !ok || cold != 1500 || total != 1800 {
		t.Fatalf("视图快照不对: %d/%d ok=%v", cold, total, ok)
	}
}
