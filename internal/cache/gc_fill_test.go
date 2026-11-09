package cache

import (
	"os"
	"testing"
)

// 有活动填充时，即使 .part 已经超过 TTL 且文件锁空闲，也不能删：
// 填充方可能刚拿到 Lease、还没打开文件。
func TestSweepPartialSkipsActiveFill(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "http:https://example.com/active.bin"
	part, meta := s.PartialPaths(key)
	if err := os.WriteFile(part, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.WritePartial(key, PartialMeta{URL: "https://example.com/active.bin", Size: 8192, Have: 4096}); err != nil {
		t.Fatal(err)
	}

	end := s.BeginFill(key)
	if files, _, err := s.SweepPartial(1 << 40); err != nil || files != 0 {
		t.Fatalf("活动填充期间不应删除: files=%d err=%v", files, err)
	}
	if _, err := os.Stat(part); err != nil {
		t.Fatalf(".part 被误删: %v", err)
	}

	end()
	if files, _, err := s.SweepPartial(1 << 40); err != nil || files != 2 {
		t.Fatalf("结束填充后应删除 part+meta: files=%d err=%v", files, err)
	}
	if _, err := os.Stat(meta); !os.IsNotExist(err) {
		t.Fatalf("meta 仍在: %v", err)
	}
}

// BeginFill 可重入：多个并发填充同一 key 时，最后一个结束才解除保护。
func TestBeginFillRefCount(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "http:https://example.com/ref.bin"
	part, _ := s.PartialPaths(key)
	if err := os.WriteFile(part, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := s.BeginFill(key)
	b := s.BeginFill(key)
	a()
	if files, _, _ := s.SweepPartial(1 << 40); files != 0 {
		t.Fatal("还有填充未结束，不应删除")
	}
	b()
	b() // 重复调用必须无副作用
	if files, _, _ := s.SweepPartial(1 << 40); files != 1 {
		t.Fatalf("全部结束后应删除: files=%d", files)
	}
}
