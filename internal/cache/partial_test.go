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
