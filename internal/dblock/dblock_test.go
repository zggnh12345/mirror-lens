package dblock

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireExcludesSecondHolder(t *testing.T) {
	p := filepath.Join(t.TempDir(), Name)
	a, err := Acquire(p)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := Acquire(p); err == nil {
		t.Fatal("第二个锁应当失败")
	} else if !IsBusy(err) {
		t.Fatalf("期望 busy，得到 %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := Acquire(p)
	if err != nil {
		t.Fatalf("释放后应能重新取得: %v", err)
	}
	b.Close()
}

func TestLockFilePermissions(t *testing.T) {
	p := filepath.Join(t.TempDir(), Name)
	l, err := Acquire(p)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("权限=%o，期望 600", fi.Mode().Perm())
	}
}
