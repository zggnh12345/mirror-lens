package cache

import (
	"os"
	"testing"
)

// 弱 ETag 不能当续传依据：只有它的半截文件要删掉重下。
func TestReadPartialDropsWeakETagOnly(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "private:x"
	part, meta := s.PartialPaths(key)
	if err := os.WriteFile(part, []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.WritePartial(key, PartialMeta{
		URL: "https://relay.example/f", ETag: `W/"abc"`, Size: 10, Have: 4,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.ReadPartial(key); ok {
		t.Fatal("弱 ETag 不该被当成有效续传依据")
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatal("半截文件应被删除")
	}
	if _, err := os.Stat(meta); !os.IsNotExist(err) {
		t.Fatal("元信息应被删除")
	}
}

// 有强 Last-Modified 时可以降级用它，别白扔已经下好的字节。
func TestReadPartialWeakETagFallsBackToLastModified(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "private:y"
	part, _ := s.PartialPaths(key)
	if err := os.WriteFile(part, []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.WritePartial(key, PartialMeta{
		URL: "https://relay.example/f", ETag: `W/"abc"`,
		LastModified: "Wed, 21 Oct 2026 07:28:00 GMT", Size: 10, Have: 4,
	}); err != nil {
		t.Fatal(err)
	}
	m, ok := s.ReadPartial(key)
	if !ok {
		t.Fatal("有 Last-Modified 时应可续传")
	}
	if m.ETag != "" {
		t.Fatalf("弱 ETag 应被清空，实际 %q", m.ETag)
	}
}

// 强 ETag 原样保留。
func TestReadPartialKeepsStrongETag(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "private:z"
	part, _ := s.PartialPaths(key)
	if err := os.WriteFile(part, []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.WritePartial(key, PartialMeta{
		URL: "https://relay.example/f", ETag: `"abc"`, Size: 10, Have: 4,
	}); err != nil {
		t.Fatal(err)
	}
	m, ok := s.ReadPartial(key)
	if !ok || m.ETag != `"abc"` {
		t.Fatalf("强 ETag 应保留：%+v ok=%v", m, ok)
	}
}

// 只有 .part 没有 .json 的半截文件永远续不上（并行填充不写 meta），
// 不管多新都该当场清掉，不能等 partial_ttl。
func TestSweepPartialDropsOrphanPart(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "http:https://example.com/orphan.bin"
	part, meta := s.PartialPaths(key)
	if err := os.WriteFile(part, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(meta); !os.IsNotExist(err) {
		t.Fatalf("前提不成立：meta 不该存在: %v", err)
	}
	// olderThan 取「很久以后」，确保按时间它还没到期
	files, freed, err := s.SweepPartial(2)
	if err != nil {
		t.Fatal(err)
	}
	if files != 1 || freed != 4096 {
		t.Fatalf("孤儿 .part 未被清: files=%d freed=%d", files, freed)
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatalf(".part 仍在: %v", err)
	}
}

// 配对的 .part + .json 没到期时不能动，否则会把还能续传的半截删掉。
func TestSweepPartialKeepsFreshPair(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "http:https://example.com/fresh.bin"
	part, _ := s.PartialPaths(key)
	if err := os.WriteFile(part, make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.WritePartial(key, PartialMeta{
		URL: "https://example.com/fresh.bin", ETag: `"strong"`, Size: 2048, Have: 1024,
	}); err != nil {
		t.Fatal(err)
	}
	files, _, err := s.SweepPartial(2)
	if err != nil {
		t.Fatal(err)
	}
	if files != 0 {
		t.Fatalf("未到期的配对半截被误删: files=%d", files)
	}
	if _, err := os.Stat(part); err != nil {
		t.Fatalf(".part 不该被删: %v", err)
	}
}
