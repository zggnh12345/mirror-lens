package gateway

import (
	"testing"
	"time"
)

func TestStreamWatchSlowAndGap(t *testing.T) {
	w := newStreamWatch(ParOptions{
		WatchMBps:        3,
		WatchWindow:      3 * time.Second,
		SwitchMinFileMiB: 1,
		SwitchMaxSentMiB: 10,
		SwitchMax:        2,
		SwitchGap:        50 * time.Millisecond,
	})
	total := int64(100 << 20)

	// 窗口还没攒满时不能判慢。
	if w.slow(1<<20, total) {
		t.Fatal("窗口未满就判慢了")
	}
	for i := 0; i < 3; i++ {
		time.Sleep(1050 * time.Millisecond)
		w.add(1 << 20) // ≈1MB/s
	}
	if !w.slow(3<<20, total) {
		t.Fatal("1MB/s 连续 3 秒应该判慢")
	}
	w.afterSwitch()
	if w.slow(3<<20, total) {
		t.Fatal("换源后间隔内不该再换")
	}

	// 换源次数用完就停手。
	w2 := newStreamWatch(ParOptions{
		WatchMBps: 3, WatchWindow: 3 * time.Second, SwitchMinFileMiB: 1,
		SwitchMaxSentMiB: 10, SwitchMax: 1, SwitchGap: 0,
	})
	for i := 0; i < 3; i++ {
		time.Sleep(1050 * time.Millisecond)
		w2.add(1 << 20)
	}
	if !w2.slow(3<<20, total) {
		t.Fatal("第一次应该判慢")
	}
	w2.afterSwitch()
	if w2.slow(3<<20, total) {
		t.Fatal("次数用完还在换源")
	}
}

func TestStreamWatchFastNotSlow(t *testing.T) {
	w := newStreamWatch(ParOptions{
		WatchMBps: 3, WatchWindow: 3 * time.Second, SwitchMinFileMiB: 1,
		SwitchMaxSentMiB: 100, SwitchMax: 2, SwitchGap: 0,
	})
	for i := 0; i < 3; i++ {
		time.Sleep(1050 * time.Millisecond)
		w.add(20 << 20) // ≈20MB/s
	}
	if w.slow(60<<20, 1<<30) {
		t.Fatal("20MB/s 不该判慢")
	}
}

func TestStreamWatchSkipsSmallFile(t *testing.T) {
	w := newStreamWatch(ParOptions{
		WatchMBps: 3, WatchWindow: 2 * time.Second, SwitchMinFileMiB: 60,
		SwitchMaxSentMiB: 100, SwitchMax: 2,
	})
	for i := 0; i < 2; i++ {
		time.Sleep(1050 * time.Millisecond)
		w.add(1 << 20)
	}
	if w.slow(2<<20, 20<<20) {
		t.Fatal("小于最小尺寸不该换源")
	}
}
