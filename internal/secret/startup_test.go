package secret

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestStartupFreshInstallCreatesKey(t *testing.T) {
	dir := t.TempDir()
	key, err := LoadKeyForStartup(dir, filepath.Join(dir, "mirror-lens.db"))
	if err != nil {
		t.Fatalf("首次安装应生成密钥: %v", err)
	}
	if len(key) != KeySize {
		t.Fatalf("密钥长度 %d", len(key))
	}
	if fi, err := os.Stat(filepath.Join(dir, "secret.key")); err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("密钥文件缺失或权限过宽: %v %v", err, fi)
	}
}

func TestStartupExistingDBWithoutKeyFailsClosed(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "mirror-lens.db")
	if err := os.WriteFile(db, []byte("sqlite-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadKeyForStartup(dir, db)
	if !errors.Is(err, ErrKeyMissingWithDB) {
		t.Fatalf("应返回 ErrKeyMissingWithDB，得到 %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "secret.key")); !os.IsNotExist(serr) {
		t.Fatal("拒绝启动时绝不能生成新密钥")
	}
	if b, _ := os.ReadFile(db); string(b) != "sqlite-bytes" {
		t.Fatal("拒绝启动时不应改写数据库")
	}
}

func TestStartupNormalAndAfterRestore(t *testing.T) {
	src := t.TempDir()
	orig, err := LoadKeyForStartup(src, filepath.Join(src, "mirror-lens.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "mirror-lens.db"), []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadKeyForStartup(src, filepath.Join(src, "mirror-lens.db"))
	if err != nil || string(got) != string(orig) {
		t.Fatalf("正常启动应复用同一密钥: %v", err)
	}

	// 恢复场景：DB 与 key 一起从备份回到新目录。
	restored := t.TempDir()
	kb, err := os.ReadFile(filepath.Join(src, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(restored, "secret.key"), kb, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(restored, "mirror-lens.db"), []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = LoadKeyForStartup(restored, filepath.Join(restored, "mirror-lens.db"))
	if err != nil || string(got) != string(orig) {
		t.Fatalf("恢复后应得到备份里的密钥: %v", err)
	}
}

func TestStartupConcurrentFirstStart(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "mirror-lens.db")
	const n = 8
	keys := make([][]byte, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			keys[i], errs[i] = LoadKeyForStartup(dir, db)
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("并发首次启动失败: %v", errs[i])
		}
		if string(keys[i]) != string(keys[0]) {
			t.Fatal("并发首次启动生成了不同密钥")
		}
	}
}

func TestStartupEmptyDBFileTreatedAsFresh(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "mirror-lens.db")
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyForStartup(dir, db); err != nil {
		t.Fatalf("0 字节残留库应按首次安装处理: %v", err)
	}
}
