package gateway

import "testing"

// 换源看中位数：一条峰值不能把慢源送进换源目标，样本不足也不下结论。
func TestSwitchSpeedOKUsesMedian(t *testing.T) {
	cases := []struct {
		name string
		sp   []int64
		min  int64
		want bool
	}{
		{"单条峰值掩盖两条慢样本", []int64{30000, 500, 600}, 1024, false},
		{"三条都快", []int64{30000, 20000, 15000}, 1024, true},
		{"样本不足不换", []int64{5000, 5000}, 1024, false},
		{"中位数刚好达标", []int64{900, 1100, 20000}, 1024, true},
	}
	for _, c := range cases {
		if got := switchSpeedOK(c.sp, c.min); got != c.want {
			t.Fatalf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
