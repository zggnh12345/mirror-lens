// Package tuning 收敛运行期默认参数。
//
// 设置页（internal/api）和探测循环（internal/runner）都要用到同一组默认值：
// 前者决定页面初次显示什么，后者决定 settings 表为空时按什么节奏跑。
// 两处各写一份数字时曾经漂移过（页面显示 15/60，实际按 3/15 跑），所以集中到这里。
package tuning

const (
	// L1Minutes 是 tier1 存活探测的默认间隔（分钟）。
	L1Minutes = 15
	// L2Minutes 是 tier2 可用性探测的默认间隔（分钟）。
	L2Minutes = 60
	// L3Minutes 是 tier3 测速的默认间隔（分钟）。
	L3Minutes = 360
)
