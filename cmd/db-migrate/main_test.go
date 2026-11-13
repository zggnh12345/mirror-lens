package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 3.5.3 必须在打开输入、创建输出之前被拒绝；--force-baseline 不得绕过。
func TestMigrateRejects353BeforeWritingOutput(t *testing.T) {
	out := filepath.Join(t.TempDir(), "must-not-exist.db")
	err := runMigrate([]string{
		"--in", filepath.Join(t.TempDir(), "does-not-matter.db"),
		"--out", out,
		"--baseline", "3.5.3",
		"--force-baseline",
	})
	if err == nil {
		t.Fatal("3.5.3 + --force-baseline 应被拒绝，实际返回 nil")
	}
	if !strings.Contains(err.Error(), "3.5.3") || !strings.Contains(err.Error(), "不支持") {
		t.Fatalf("错误信息没有说明 3.5.3 不支持: %v", err)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatalf("拒绝发生在输出创建之后: stat=%v", statErr)
	}
}
