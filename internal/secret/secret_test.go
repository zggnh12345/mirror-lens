package secret

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	key, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	enc, err := Encrypt(key, "ghp_abcdefghijklmnopqrstuvwxyz0123456789")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(enc, "ghp_") {
		t.Fatal("密文里出现了明文")
	}
	got, err := Decrypt(key, enc)
	if err != nil {
		t.Fatal(err)
	}
	if got != "ghp_abcdefghijklmnopqrstuvwxyz0123456789" {
		t.Fatalf("解出 %q", got)
	}
}

func TestKeyPersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secret.key")
	k1, err := LoadOrCreateKey(p)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := LoadOrCreateKey(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(k1) != string(k2) {
		t.Fatal("两次加载拿到不同密钥")
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("权限 %o，应为 600", fi.Mode().Perm())
	}
}

func TestWrongKeyRejected(t *testing.T) {
	dir := t.TempDir()
	k1, _ := LoadOrCreateKey(filepath.Join(dir, "a.key"))
	k2, _ := LoadOrCreateKey(filepath.Join(dir, "b.key"))
	enc, err := Encrypt(k1, "secret-value")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decrypt(k2, enc); err == nil {
		t.Fatal("换密钥仍能解开")
	}
}

func TestTamperedRejected(t *testing.T) {
	key, _ := LoadOrCreateKey(filepath.Join(t.TempDir(), "secret.key"))
	enc, err := Encrypt(key, "secret-value")
	if err != nil {
		t.Fatal(err)
	}
	// 改最后一个字符，GCM 必须报错而不是返回垃圾。
	last := enc[len(enc)-1]
	bad := enc[:len(enc)-1] + string(byte(last)^1)
	if _, err := Decrypt(key, bad); err == nil {
		t.Fatal("篡改密文未被发现")
	}
}
