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

// TestAcquireTightensExistingLockPermissions 覆盖已有锁文件权限过宽的情况：
// Acquire 必须在 Flock 前把已有文件收紧到 0600，而不是只依赖创建时的 mode。
func TestAcquireTightensExistingLockPermissions(t *testing.T) {
	p := filepath.Join(t.TempDir(), Name)
	if err := os.WriteFile(p, []byte("old-lock-holder\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o705); err != nil {
		t.Fatalf("设置已有锁文件权限失败: %v", err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o705 {
		t.Fatalf("前置条件失败: 权限=%o，期望 705", fi.Mode().Perm())
	}

	l, err := Acquire(p)
	if err != nil {
		t.Fatalf("已有锁文件应能获取锁: %v", err)
	}
	defer l.Close()

	fi, err = os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("已有锁文件未收紧: 权限=%o，期望 600", fi.Mode().Perm())
	}
}
