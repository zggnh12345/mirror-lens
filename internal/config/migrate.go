package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	legacySwitchMinKey = "switch_min_mbps"
	switchFloorKey     = "switch_floor_mib_per_sec"
	backupInfix        = ".bak-pre-3.5.22-"
)

// MigrateReport 描述一次迁移的结果。Migrated 为 false 表示文件不存在或
// 不含旧键，未做任何写入。
type MigrateReport struct {
	Migrated   bool
	BackupPath string
	// Warning 非空表示迁移已完成，但存在需要用户知晓的降级（例如目录
	// fsync 返回 EINVAL/ENOTSUP）。它不是错误，调用方应记录日志后继续启动。
	Warning string
}

// MigrationError 是可操作的迁移失败错误。调用方应阻止启动。
type MigrationError struct {
	Path      string
	LegacyKey string
	Stage     string
	Err       error
}

func (e *MigrationError) Error() string {
	dir := filepath.Dir(e.Path)
	return fmt.Sprintf(
		"配置迁移失败：无法写入配置目录 %s。\n"+
			"检测到旧配置项 parallel.%s，但无法完成迁移（阶段：%s，原因：%v）。\n"+
			"请检查目录挂载权限和磁盘空间，确保 %s 可写；\n"+
			"也可以手动将旧配置项替换为 parallel.%s。\n"+
			"服务已停止启动，以避免使用未迁移配置。",
		dir, e.LegacyKey, e.Stage, e.Err, dir, switchFloorKey)
}

func (e *MigrationError) Unwrap() error { return e.Err }

// migrateOps 把会产生副作用或需要故障注入的文件系统操作集中起来。
type migrateOps struct {
	lstat       func(string) (os.FileInfo, error)
	rename      func(string, string) error
	syncDir     func(string) error
	now         func() time.Time
	writeBackup func(*os.File, []byte) error
}

func defaultMigrateOps() migrateOps {
	return migrateOps{
		lstat:   os.Lstat,
		rename:  os.Rename,
		syncDir: syncDir,
		now:     time.Now,
		writeBackup: func(f *os.File, b []byte) error {
			n, err := f.Write(b)
			if err == nil && n != len(b) {
				return io.ErrShortWrite
			}
			return err
		},
	}
}

// MigrateFile 在启动时把旧键 parallel.switch_min_mbps 迁移为新键
// parallel.switch_floor_mib_per_sec。它不创建不存在的文件，也不承担
// 配置读取职责；config.Load 仍保持纯读取。
func MigrateFile(path string) (MigrateReport, error) {
	return migrateFile(path, defaultMigrateOps())
}

func migrateFile(path string, ops migrateOps) (MigrateReport, error) {
	rep := MigrateReport{}
	if path == "" {
		return rep, nil
	}

	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return rep, nil
		}
		return rep, &MigrationError{Path: path, LegacyKey: legacySwitchMinKey, Stage: "read", Err: err}
	}

	var root yaml.Node
	if err := yaml.Unmarshal(b, &root); err != nil {
		return rep, &MigrationError{Path: path, LegacyKey: legacySwitchMinKey, Stage: "parse", Err: err}
	}
	var doc *yaml.Node
	if len(root.Content) > 0 {
		doc = root.Content[0]
	}
	parallel := findMapping(doc, "parallel")
	if parallel == nil {
		return rep, nil
	}
	idx := findKeyIndex(parallel, legacySwitchMinKey)
	if idx < 0 {
		return rep, nil
	}

	origInfo, err := ops.lstat(path)
	if err != nil {
		return rep, &MigrationError{Path: path, LegacyKey: legacySwitchMinKey, Stage: "lstat", Err: err}
	}
	if origInfo.Mode()&os.ModeSymlink != 0 {
		return rep, &MigrationError{Path: path, LegacyKey: legacySwitchMinKey, Stage: "lstat", Err: errors.New("配置文件是符号链接，拒绝自动迁移")}
	}

	dir := filepath.Dir(path)
	backupPath, err := createBackup(path, b, ops, &rep)
	if err != nil {
		return rep, &MigrationError{Path: path, LegacyKey: legacySwitchMinKey, Stage: "backup", Err: err}
	}
	rep.BackupPath = backupPath

	var warnings []string
	if err := ops.syncDir(dir); err != nil {
		if isDirSyncUnsupported(err) {
			warnings = append(warnings, fmt.Sprintf("备份目录不支持 fsync（%v），继续迁移", err))
		} else {
			return rep, &MigrationError{Path: path, LegacyKey: legacySwitchMinKey, Stage: "dirsync_backup", Err: err}
		}
	}

	// 新键已存在时只删除旧键，保留新键的值与注释；否则删除旧键并写入默认值。
	if newIdx := findKeyIndex(parallel, switchFloorKey); newIdx >= 0 {
		parallel.Content = append(parallel.Content[:idx], parallel.Content[idx+2:]...)
	} else {
		parallel.Content = append(parallel.Content[:idx], parallel.Content[idx+2:]...)
		key := &yaml.Node{
			Kind:        yaml.ScalarNode,
			Tag:         "!!str",
			Value:       switchFloorKey,
			HeadComment: "动态换源阈值的绝对下限 F（MiB/s）；0 或负数按 150 MiB/s 硬上限处理。",
		}
		val := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: "1"}
		parallel.Content = append(parallel.Content, key, val)
	}

	out, err := yaml.Marshal(&root)
	if err != nil {
		return rep, &MigrationError{Path: path, LegacyKey: legacySwitchMinKey, Stage: "marshal", Err: err}
	}

	tmp, err := os.CreateTemp(dir, ".config.yaml.tmp-*")
	if err != nil {
		return rep, &MigrationError{Path: path, LegacyKey: legacySwitchMinKey, Stage: "temp", Err: err}
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	// 优先保留原 UID/GID；运行用户即原属主时无需 chown。
	if st, ok := origInfo.Sys().(*syscall.Stat_t); ok {
		if int(st.Uid) != os.Geteuid() || int(st.Gid) != os.Getegid() {
			if err := tmp.Chown(int(st.Uid), int(st.Gid)); err != nil {
				warnings = append(warnings, fmt.Sprintf("无法保留原属主 %d:%d（%v）", st.Uid, st.Gid, err))
			}
		}
	}
	if err := tmp.Chmod(origInfo.Mode().Perm()); err != nil {
		return rep, &MigrationError{Path: path, LegacyKey: legacySwitchMinKey, Stage: "temp", Err: err}
	}
	if _, err := tmp.Write(out); err != nil {
		return rep, &MigrationError{Path: path, LegacyKey: legacySwitchMinKey, Stage: "temp", Err: err}
	}
	if err := tmp.Sync(); err != nil {
		return rep, &MigrationError{Path: path, LegacyKey: legacySwitchMinKey, Stage: "temp", Err: err}
	}
	if err := tmp.Close(); err != nil {
		return rep, &MigrationError{Path: path, LegacyKey: legacySwitchMinKey, Stage: "temp", Err: err}
	}

	// rename 前再查一次，缩小 symlink TOCTOU 窗口。
	if fi, err := ops.lstat(path); err != nil {
		return rep, &MigrationError{Path: path, LegacyKey: legacySwitchMinKey, Stage: "lstat", Err: err}
	} else if fi.Mode()&os.ModeSymlink != 0 {
		return rep, &MigrationError{Path: path, LegacyKey: legacySwitchMinKey, Stage: "lstat", Err: errors.New("rename 前配置文件变为符号链接")}
	}

	if err := ops.rename(tmpPath, path); err != nil {
		return rep, &MigrationError{Path: path, LegacyKey: legacySwitchMinKey, Stage: "rename", Err: err}
	}
	cleanup = false

	if err := ops.syncDir(dir); err != nil {
		if isDirSyncUnsupported(err) {
			warnings = append(warnings, fmt.Sprintf(
				"配置迁移已完成，配置文件已原子替换并校验通过，但当前文件系统不支持目录 fsync（%v）。迁移结果已生效，但断电情况下目录项持久性无法保证。", err))
		} else {
			return rep, &MigrationError{Path: path, LegacyKey: legacySwitchMinKey, Stage: "dirsync_final", Err: err}
		}
	}

	finalInfo, err := ops.lstat(path)
	if err != nil {
		return rep, &MigrationError{Path: path, LegacyKey: legacySwitchMinKey, Stage: "verify", Err: err}
	}
	if finalInfo.Mode()&os.ModeSymlink != 0 {
		return rep, &MigrationError{Path: path, LegacyKey: legacySwitchMinKey, Stage: "verify", Err: errors.New("迁移后配置文件是符号链接")}
	}
	if finalInfo.Mode().Perm() != origInfo.Mode().Perm() {
		warnings = append(warnings, fmt.Sprintf("迁移后权限 %o 与原权限 %o 不一致",
			finalInfo.Mode().Perm(), origInfo.Mode().Perm()))
	}
	if !sameOwner(origInfo, finalInfo) {
		warnings = append(warnings, "迁移后文件属主发生变化")
		// 属主变化本身不必然破坏服务，但必须确认运行用户仍能读写配置。
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return rep, &MigrationError{Path: path, LegacyKey: legacySwitchMinKey, Stage: "verify",
				Err: fmt.Errorf("属主变化后服务无法读写配置: %w", err)}
		}
		_ = f.Close()
	}

	rep.Migrated = true
	rep.Warning = strings.Join(warnings, "；")
	return rep, nil
}

func createBackup(path string, b []byte, ops migrateOps, rep *MigrateReport) (string, error) {
	base := path + backupInfix + strconv.FormatInt(ops.now().Unix(), 10)
	candidate := base
	for i := 1; ; i++ {
		f, err := os.OpenFile(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			if os.IsExist(err) {
				// 同一秒内多次启动：不覆盖已有备份，追加序号。
				candidate = base + "-" + strconv.Itoa(i)
				continue
			}
			return "", err
		}
		// 某些网络/共享挂载会忽略 open 的 mode（实测新建文件为 0755），
		// 备份必须显式 chmod 到 0600；失败则删除半成品并阻止迁移。
		if err := f.Chmod(0o600); err != nil {
			_ = f.Close()
			_ = os.Remove(candidate)
			return "", err
		}
		werr := ops.writeBackup(f, b)
		if werr == nil {
			werr = f.Sync()
		}
		cerr := f.Close()
		if werr == nil {
			werr = cerr
		}
		if werr != nil {
			// 部分写入的备份不可用于回退，但保留现场供排查。
			rep.Warning = "backup_incomplete: " + candidate
			return candidate, werr
		}
		return candidate, nil
	}
}

func findMapping(n *yaml.Node, key string) *yaml.Node {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil
		}
		n = n.Content[0]
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func findKeyIndex(m *yaml.Node, key string) int {
	if m == nil || m.Kind != yaml.MappingNode {
		return -1
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i
		}
	}
	return -1
}

func sameOwner(a, b os.FileInfo) bool {
	as, ok1 := a.Sys().(*syscall.Stat_t)
	bs, ok2 := b.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 {
		return true
	}
	return as.Uid == bs.Uid && as.Gid == bs.Gid
}

// isDirSyncUnsupported 判断目录 fsync 是否只是「文件系统不支持」。
// 只认 EINVAL/ENOTSUP；EIO/ENOSPC/EACCES 等必须视为真实故障。
func isDirSyncUnsupported(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
