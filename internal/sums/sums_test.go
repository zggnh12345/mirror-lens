package sums

import (
	"errors"
	"strings"
	"testing"
)

const h1 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
const h2 = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

func TestGNUFormat(t *testing.T) {
	data := h1 + "  app-linux-amd64.tar.gz\n" + h2 + "  other.zip\n"
	got, err := Lookup([]byte(data), "app-linux-amd64.tar.gz")
	if err != nil || got != h1 {
		t.Fatalf("GNU 解析失败: %q %v", got, err)
	}
}

func TestBinaryPrefix(t *testing.T) {
	data := h1 + " *app.exe\n"
	if got, err := Lookup([]byte(data), "app.exe"); err != nil || got != h1 {
		t.Fatalf("二进制前缀处理失败: %q %v", got, err)
	}
}

func TestBSDAndCRLF(t *testing.T) {
	data := "SHA256 (app.tgz) = " + strings.ToUpper(h2) + "\r\n"
	if got, err := Lookup([]byte(data), "app.tgz"); err != nil || got != h2 {
		t.Fatalf("BSD/CRLF 解析失败: %q %v", got, err)
	}
}

func TestPathInName(t *testing.T) {
	data := h1 + "  dist/app.tgz\n"
	if got, err := Lookup([]byte(data), "app.tgz"); err != nil || got != h1 {
		t.Fatalf("带路径的文件名应取最后一段: %q %v", got, err)
	}
}

func TestAmbiguousRejected(t *testing.T) {
	data := h1 + "  app.tgz\n" + h2 + "  app.tgz\n"
	if _, err := Lookup([]byte(data), "app.tgz"); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("同名不同摘要必须拒绝: %v", err)
	}
}

// 同名同摘要的重复记录不算歧义。
func TestDuplicateSameDigestOK(t *testing.T) {
	data := h1 + "  app.tgz\n" + h1 + "  app.tgz\n"
	if got, err := Lookup([]byte(data), "app.tgz"); err != nil || got != h1 {
		t.Fatalf("重复同摘要应通过: %q %v", got, err)
	}
}

func TestEmptyAndInvalid(t *testing.T) {
	if _, err := Lookup([]byte(""), "a"); !errors.Is(err, ErrEmpty) {
		t.Fatalf("空文件应报 ErrEmpty: %v", err)
	}
	if _, err := Lookup([]byte("this is not a checksum file\n"), "a"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("非法格式应报 ErrInvalid: %v", err)
	}
}

func TestMissingEntry(t *testing.T) {
	data := h1 + "  a.tgz\n"
	if _, err := Lookup([]byte(data), "b.tgz"); !errors.Is(err, ErrNoEntry) {
		t.Fatalf("目标不存在应报 ErrNoEntry: %v", err)
	}
}

// 63 位或非十六进制的行不能被当成摘要。
func TestRejectsMalformedHex(t *testing.T) {
	data := h1[:63] + "  a.tgz\n" + strings.Repeat("z", 64) + "  a.tgz\n"
	if _, err := Lookup([]byte(data), "a.tgz"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("畸形摘要不应被接受: %v", err)
	}
}

func TestBOMAndComments(t *testing.T) {
	data := "\xEF\xBB\xBF# generated\n\n" + h1 + "  app.tgz\n"
	if got, err := Lookup([]byte(data), "app.tgz"); err != nil || got != h1 {
		t.Fatalf("BOM/注释处理失败: %q %v", got, err)
	}
}
