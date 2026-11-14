package main

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func fileSHA(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

func assertNoOutputOrStaging(t *testing.T, out string) {
	t.Helper()
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("失败后仍存在目标: %v", err)
	}
	matches, err := filepath.Glob(out + ".tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("失败后仍存在 staging: %v", matches)
	}
}

// C1：非空 -wal / -shm 必须在任何输出创建前拒绝。
func TestRejectDirtySidecars(t *testing.T) {
	for _, kind := range []struct {
		name, suffix string
	}{
		{"wal", "-wal"},
		{"shm", "-shm"},
	} {
		t.Run("migrate-"+kind.name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "src.db")
			build355Fixture(t, src)
			if err := os.WriteFile(src+kind.suffix, []byte("dirty"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := fileSHA(t, src)
			out := filepath.Join(t.TempDir(), "out.db")
			if err := runMigrate([]string{"--in", src, "--out", out, "--baseline", "3.5.5"}); err == nil {
				t.Fatal("非空 sidecar 时迁移应失败")
			}
			assertNoOutputOrStaging(t, out)
			if fileSHA(t, src) != before {
				t.Fatal("拒绝路径修改了源文件")
			}
		})
		t.Run("upgrade-"+kind.name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "src.db")
			build356Fixture(t, src)
			if err := os.WriteFile(src+kind.suffix, []byte("dirty"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := fileSHA(t, src)
			out := filepath.Join(t.TempDir(), "out.db")
			if err := runUpgrade356([]string{"--in", src, "--out", out}); err == nil {
				t.Fatal("非空 sidecar 时升级应失败")
			}
			assertNoOutputOrStaging(t, out)
			if fileSHA(t, src) != before {
				t.Fatal("拒绝路径修改了源文件")
			}
		})
	}
}

// C2：成功发布后 staging/目标不得留下 -wal/-shm，也不得留下 tmp 名。
func TestStagingHasNoWALOrSHM(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(*testing.T, string)
		run   func(string, string) error
	}{
		{"migrate-355", build355Fixture, func(in, out string) error {
			return runMigrate([]string{"--in", in, "--out", out, "--baseline", "3.5.5"})
		}},
		{"upgrade-356", build356Fixture, func(in, out string) error {
			return runUpgrade356([]string{"--in", in, "--out", out})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "src.db")
			tc.build(t, src)
			out := filepath.Join(t.TempDir(), "out.db")
			if err := tc.run(src, out); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(out); err != nil {
				t.Fatalf("目标不存在: %v", err)
			}
			for _, suffix := range []string{"-wal", "-shm"} {
				if _, err := os.Stat(out + suffix); !os.IsNotExist(err) {
					t.Fatalf("目标不应存在 %s: %v", suffix, err)
				}
				if _, err := os.Stat(src + suffix); !os.IsNotExist(err) {
					t.Fatalf("源库不应被创建 %s: %v", suffix, err)
				}
			}
			matches, err := filepath.Glob(out + ".tmp-*")
			if err != nil {
				t.Fatal(err)
			}
			if len(matches) != 0 {
				t.Fatalf("成功后仍残留 staging: %v", matches)
			}
		})
	}
}

// C3：硬链接发布必须具备真正的 no-clobber 语义。
func TestPublishNoClobber(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "tmp.db")
	out := filepath.Join(dir, "out.db")
	if err := os.WriteFile(tmp, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := publishNoClobber(tmp, out); err == nil {
		t.Fatal("目标已存在时必须拒绝发布")
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old" {
		t.Fatalf("发布失败覆盖了已有目标: %q", got)
	}
	if err := os.Remove(out); err != nil {
		t.Fatal(err)
	}
	if err := publishNoClobber(tmp, out); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("发布内容错误: %q", got)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("发布成功后 staging 应删除: %v", err)
	}
}

// C4：提交、校验、发布阶段故障都必须清理 staging、保留源文件。
func TestUpgradeStageFailuresCleanStaging(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(func() error)
	}{
		{"commit", func(f func() error) {
			upgradeCommitHook = func(*sql.Tx) error { return f() }
		}},
		{"verify", func(f func() error) {
			targetVerifyHook = func(*sql.DB) error { return f() }
		}},
		{"publish", func(f func() error) {
			targetPublishHook = func() error { return f() }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "src.db")
			build356Fixture(t, src)
			before := fileSHA(t, src)
			out := filepath.Join(t.TempDir(), "out.db")
			tc.set(func() error { return errors.New("injected " + tc.name) })
			err := runUpgrade356([]string{"--in", src, "--out", out})
			upgradeCommitHook = nil
			targetVerifyHook = nil
			targetPublishHook = nil
			if err == nil {
				t.Fatalf("%s 故障未返回错误", tc.name)
			}
			assertNoOutputOrStaging(t, out)
			if fileSHA(t, src) != before {
				t.Fatalf("%s 故障修改了源文件", tc.name)
			}
		})
	}
}

// runMigrate 的校验收尾也必须可注入并清理。
func TestMigrateStageFailuresCleanStaging(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(func() error)
	}{
		{"verify", func(f func() error) {
			targetVerifyHook = func(*sql.DB) error { return f() }
		}},
		{"publish", func(f func() error) {
			targetPublishHook = func() error { return f() }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "src.db")
			build355Fixture(t, src)
			before := fileSHA(t, src)
			out := filepath.Join(t.TempDir(), "out.db")
			tc.set(func() error { return errors.New("injected " + tc.name) })
			err := runMigrate([]string{"--in", src, "--out", out, "--baseline", "3.5.5"})
			targetVerifyHook = nil
			targetPublishHook = nil
			if err == nil {
				t.Fatalf("%s 故障未返回错误", tc.name)
			}
			assertNoOutputOrStaging(t, out)
			if fileSHA(t, src) != before {
				t.Fatalf("%s 故障修改了源文件", tc.name)
			}
		})
	}
}
