package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func newMigratePath(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func backupsIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.Contains(e.Name(), backupInfix) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

func requireMigrationError(t *testing.T, err error, stage string) *MigrationError {
	t.Helper()
	var me *MigrationError
	if !errors.As(err, &me) {
		t.Fatalf("期望 *MigrationError，得到 %T: %v", err, err)
	}
	if stage != "" && me.Stage != stage {
		t.Fatalf("阶段 = %q，期望 %q", me.Stage, stage)
	}
	return me
}

func TestMigrateFileNotExist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	rep, err := MigrateFile(path)
	if err != nil {
		t.Fatalf("文件不存在不应报错: %v", err)
	}
	if rep.Migrated {
		t.Fatal("文件不存在不应标记为已迁移")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("文件不存在时不应创建文件")
	}
	if got := backupsIn(t, dir); len(got) != 0 {
		t.Fatalf("不应创建备份: %v", got)
	}
}

func TestMigrateNoLegacyKey(t *testing.T) {
	content := "parallel:\n  switch_min_file_mib: 30\n"
	path := newMigratePath(t, content, 0o600)

	rep, err := MigrateFile(path)
	if err != nil {
		t.Fatalf("无旧键不应报错: %v", err)
	}
	if rep.Migrated {
		t.Fatal("无旧键不应标记为已迁移")
	}
	if got := mustRead(t, path); got != content {
		t.Fatalf("无旧键不应改写文件:\n%s", got)
	}
	if backups := backupsIn(t, filepath.Dir(path)); len(backups) != 0 {
		t.Fatalf("无旧键不应备份: %v", backups)
	}
}

func TestMigrateOnlyLegacy(t *testing.T) {
	content := "parallel:\n  switch_min_mbps: 8\n  switch_min_file_mib: 30\n"
	path := newMigratePath(t, content, 0o600)

	rep, err := MigrateFile(path)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if !rep.Migrated {
		t.Fatal("应标记为已迁移")
	}
	got := mustRead(t, path)
	if strings.Contains(got, "switch_min_mbps") {
		t.Fatalf("旧键未删除:\n%s", got)
	}
	if !strings.Contains(got, "switch_floor_mib_per_sec: 1") {
		t.Fatalf("新键未写入默认值 1:\n%s", got)
	}
	if rep.BackupPath == "" {
		t.Fatal("应返回备份路径")
	}
	if backup := mustRead(t, rep.BackupPath); backup != content {
		t.Fatalf("备份字节与原文件不一致:\n%s", backup)
	}
	fi, err := os.Stat(rep.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("备份权限 = %o，期望 600", fi.Mode().Perm())
	}
}

func TestMigrateOnlyNew(t *testing.T) {
	content := "parallel:\n  switch_floor_mib_per_sec: 2\n"
	path := newMigratePath(t, content, 0o600)

	rep, err := MigrateFile(path)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if rep.Migrated {
		t.Fatal("只有新键时不应标记为已迁移")
	}
	if got := mustRead(t, path); got != content {
		t.Fatalf("只有新键时不应改写文件:\n%s", got)
	}
	if backups := backupsIn(t, filepath.Dir(path)); len(backups) != 0 {
		t.Fatalf("只有新键时不应备份: %v", backups)
	}
}

func TestMigrateBothKeys(t *testing.T) {
	content := "parallel:\n  switch_min_mbps: 8\n  switch_floor_mib_per_sec: 2\n"
	path := newMigratePath(t, content, 0o600)

	rep, err := MigrateFile(path)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if !rep.Migrated {
		t.Fatal("新旧共存应执行迁移")
	}
	got := mustRead(t, path)
	if strings.Contains(got, "switch_min_mbps") {
		t.Fatalf("旧键未删除:\n%s", got)
	}
	if !strings.Contains(got, "switch_floor_mib_per_sec: 2") {
		t.Fatalf("新键值 2 被改写:\n%s", got)
	}
}

func TestMigrateLegacyZero(t *testing.T) {
	content := "parallel:\n  switch_min_mbps: 0\n"
	path := newMigratePath(t, content, 0o600)

	if _, err := MigrateFile(path); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	got := mustRead(t, path)
	if !strings.Contains(got, "switch_floor_mib_per_sec: 1") {
		t.Fatalf("旧键 0 应写新键 1:\n%s", got)
	}
}

func TestMigrateIdempotent(t *testing.T) {
	content := "parallel:\n  switch_min_mbps: 8\n"
	path := newMigratePath(t, content, 0o600)

	if _, err := MigrateFile(path); err != nil {
		t.Fatal(err)
	}
	first := mustRead(t, path)
	backups := backupsIn(t, filepath.Dir(path))
	if len(backups) != 1 {
		t.Fatalf("第一次迁移应只有 1 份备份，得到 %d", len(backups))
	}

	rep, err := MigrateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Migrated {
		t.Fatal("第二次不应重复迁移")
	}
	if got := mustRead(t, path); got != first {
		t.Fatal("第二次不应改变文件")
	}
	if got := backupsIn(t, filepath.Dir(path)); len(got) != 1 {
		t.Fatalf("第二次不应新增备份，得到 %d", len(got))
	}
}

func TestMigrateBackupExclusive(t *testing.T) {
	content := "parallel:\n  switch_min_mbps: 8\n"
	path := newMigratePath(t, content, 0o600)
	dir := filepath.Dir(path)

	fixed := time.Unix(1700000000, 0)
	pre := path + backupInfix + "1700000000"
	if err := os.WriteFile(pre, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}

	ops := defaultMigrateOps()
	ops.now = func() time.Time { return fixed }
	rep, err := migrateFile(path, ops)
	if err != nil {
		t.Fatal(err)
	}
	if rep.BackupPath == pre {
		t.Fatal("不得覆盖已有备份")
	}
	if rep.BackupPath != pre+"-1" {
		t.Fatalf("备份路径 = %q，期望 %q", rep.BackupPath, pre+"-1")
	}
	if got := mustRead(t, pre); got != "existing" {
		t.Fatalf("已有备份被改写: %q", got)
	}
	if len(backupsIn(t, dir)) != 2 {
		t.Fatal("应保留已有备份并新建一份")
	}
}

func TestMigrateDirSyncEINVAL(t *testing.T) {
	content := "parallel:\n  switch_min_mbps: 8\n"
	path := newMigratePath(t, content, 0o600)

	calls := 0
	ops := defaultMigrateOps()
	ops.syncDir = func(string) error {
		calls++
		return syscall.EINVAL
	}
	rep, err := migrateFile(path, ops)
	if err != nil {
		t.Fatalf("EINVAL 应继续: %v", err)
	}
	if calls != 2 {
		t.Fatalf("目录 fsync 次数 = %d，期望 2", calls)
	}
	if rep.Warning == "" || !strings.Contains(rep.Warning, "不支持") {
		t.Fatalf("应记录 EINVAL 降级警告，得到 %q", rep.Warning)
	}
	if strings.Contains(mustRead(t, path), "switch_min_mbps") {
		t.Fatal("迁移应已完成")
	}
}

func TestMigrateDirSyncENOTSUP(t *testing.T) {
	content := "parallel:\n  switch_min_mbps: 8\n"
	path := newMigratePath(t, content, 0o600)

	ops := defaultMigrateOps()
	ops.syncDir = func(string) error { return syscall.ENOTSUP }
	rep, err := migrateFile(path, ops)
	if err != nil {
		t.Fatalf("ENOTSUP 应继续: %v", err)
	}
	if rep.Warning == "" {
		t.Fatal("应记录 ENOTSUP 降级警告")
	}
}

func TestMigrateDirSyncEIO(t *testing.T) {
	content := "parallel:\n  switch_min_mbps: 8\n"
	path := newMigratePath(t, content, 0o600)

	calls := 0
	ops := defaultMigrateOps()
	ops.syncDir = func(string) error {
		calls++
		if calls == 1 {
			return syscall.EIO
		}
		return nil
	}
	_, err := migrateFile(path, ops)
	requireMigrationError(t, err, "dirsync_backup")
	if calls != 1 {
		t.Fatalf("第一次目录 fsync 失败后不得继续，调用次数 = %d", calls)
	}
	if got := mustRead(t, path); got != content {
		t.Fatal("备份目录 fsync 失败时不得改动原配置")
	}
	if len(backupsIn(t, filepath.Dir(path))) != 1 {
		t.Fatal("失败尝试留下的备份应保留")
	}
}

func TestMigrateDirSyncWrappedEINVAL(t *testing.T) {
	content := "parallel:\n  switch_min_mbps: 8\n"
	path := newMigratePath(t, content, 0o600)

	ops := defaultMigrateOps()
	ops.syncDir = func(string) error {
		return &os.PathError{Op: "fsync", Path: filepath.Dir(path), Err: syscall.EINVAL}
	}
	rep, err := migrateFile(path, ops)
	if err != nil {
		t.Fatalf("*os.PathError 包裹的 EINVAL 应继续: %v", err)
	}
	if rep.Warning == "" {
		t.Fatal("应记录降级警告")
	}
}

func TestMigrateRenameFailure(t *testing.T) {
	content := "parallel:\n  switch_min_mbps: 8\n"
	path := newMigratePath(t, content, 0o600)

	ops := defaultMigrateOps()
	ops.rename = func(string, string) error { return syscall.EIO }
	_, err := migrateFile(path, ops)
	requireMigrationError(t, err, "rename")
	if got := mustRead(t, path); got != content {
		t.Fatal("rename 失败时原配置字节必须不变")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("失败后应清理临时文件，残留 %s", e.Name())
		}
	}
}

func TestMigrateSymlinkRejected(t *testing.T) {
	content := "parallel:\n  switch_min_mbps: 8\n"
	dir := t.TempDir()
	target := filepath.Join(dir, "real.yaml")
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	_, err := migrateFile(link, defaultMigrateOps())
	requireMigrationError(t, err, "lstat")
	if got := mustRead(t, target); got != content {
		t.Fatal("拒绝迁移时不得改动符号链接目标")
	}
}

func TestMigratePreservesAnchorAliasUnknownAndComments(t *testing.T) {
	content := `# top comment
parallel:
  # legacy comment
  switch_min_mbps: 8
  switch_min_file_mib: 30
  # new key comment
  switch_floor_mib_per_sec: 2
other:
  anchor: &a
    x: 1
  alias: *a
  unknown_key: hello
`
	path := newMigratePath(t, content, 0o600)

	if _, err := MigrateFile(path); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	got := mustRead(t, path)
	if !strings.Contains(got, "# top comment") {
		t.Fatal("顶层注释未保留")
	}
	if !strings.Contains(got, "# new key comment") {
		t.Fatal("新键注释未保留")
	}
	if strings.Contains(got, "switch_min_mbps") {
		t.Fatal("旧键未删除")
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("迁移后 YAML 无法解析: %v", err)
	}
	parallel, ok := doc["parallel"].(map[string]any)
	if !ok {
		t.Fatal("parallel 节点丢失")
	}
	if got := parallel["switch_floor_mib_per_sec"]; got != 2 {
		t.Fatalf("新键值 = %v，期望 2", got)
	}
	other, ok := doc["other"].(map[string]any)
	if !ok {
		t.Fatal("other 节点丢失")
	}
	if got := other["unknown_key"]; got != "hello" {
		t.Fatalf("未知键 = %v", got)
	}
	alias, ok := other["alias"].(map[string]any)
	if !ok {
		t.Fatal("别名未解析为映射")
	}
	if got := alias["x"]; got != 1 {
		t.Fatalf("别名引用值 = %v，期望 1", got)
	}
}

func TestMigrateBackupIncomplete(t *testing.T) {
	content := "parallel:\n  switch_min_mbps: 8\n"
	path := newMigratePath(t, content, 0o600)

	ops := defaultMigrateOps()
	ops.writeBackup = func(f *os.File, b []byte) error {
		if _, err := f.Write(b[:len(b)/2]); err != nil {
			return err
		}
		return syscall.ENOSPC
	}
	rep, err := migrateFile(path, ops)
	requireMigrationError(t, err, "backup")
	if !strings.Contains(rep.Warning, "backup_incomplete") {
		t.Fatalf("应标记 backup_incomplete，得到 %q", rep.Warning)
	}
	if got := mustRead(t, path); got != content {
		t.Fatal("备份失败时不得改动原配置")
	}
	backups := backupsIn(t, filepath.Dir(path))
	if len(backups) != 1 {
		t.Fatalf("应保留失败的部分备份，得到 %d", len(backups))
	}
	if got := mustRead(t, backups[0]); len(got) >= len(content) {
		t.Fatalf("部分备份应短于原文件，得到 %d 字节", len(got))
	}
}

func TestMigrateModePreserved(t *testing.T) {
	content := "parallel:\n  switch_min_mbps: 8\n"
	path := newMigratePath(t, content, 0o640)

	if _, err := MigrateFile(path); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Fatalf("迁移后权限 = %o，期望 640", fi.Mode().Perm())
	}
}

func TestMigrateLoadFloor(t *testing.T) {
	path := newMigratePath(t, "parallel:\n  switch_min_mbps: 8\n", 0o600)
	if _, err := MigrateFile(path); err != nil {
		t.Fatal(err)
	}
		// 3.5.25 fail-fast：补上必需字段后再加载。
	if b, e := os.ReadFile(path); e == nil && !strings.Contains(string(b), "probe:") {
		os.WriteFile(path, append(b, []byte("\nprobe:\n  host_probe_repos: {}\n  host_probe_interval_sec: 21600\n  host_probe_scan_sec: 30\n")...), 0o600)
	}
cfg, err := Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Parallel.SwitchFloorMiBPerSec != 1 {
		t.Fatalf("新键 = %d，期望 1", cfg.Parallel.SwitchFloorMiBPerSec)
	}
	if got := cfg.Parallel.SwitchFloorMiBPerSec * 1024 * 1024; got != 1048576 {
		t.Fatalf("F = %d B/s，期望 1048576", got)
	}
}

func TestNormalizeMaxSentMiB(t *testing.T) {
	cases := []struct{ in, want int64 }{
		{-5, 150}, {0, 150}, {1, 1}, {150, 150}, {200, 150}, {102400, 150},
	}
	for _, c := range cases {
		if got := NormalizeMaxSentMiB(c.in); got != c.want {
			t.Fatalf("NormalizeMaxSentMiB(%d) = %d，期望 %d", c.in, got, c.want)
		}
	}
}
