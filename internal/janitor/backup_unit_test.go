package janitor

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTestUnit(t *testing.T, units, day string) string {
	t.Helper()
	dir := filepath.Join(units, backupPrefix+day)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db := []byte("db-" + day)
	key := []byte("key-" + day)
	if err := os.WriteFile(filepath.Join(dir, unitDBName), db, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, unitDBName), 0o600); err != nil {
		t.Fatalf("设置数据库文件权限: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, unitKeyName), key, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, unitKeyName), 0o600); err != nil {
		t.Fatalf("设置密钥文件权限: %v", err)
	}
	dbSum, dbSize, err := hashFile(filepath.Join(dir, unitDBName))
	if err != nil {
		t.Fatal(err)
	}
	keySum, keySize, err := hashFile(filepath.Join(dir, unitKeyName))
	if err != nil {
		t.Fatal(err)
	}
	m := backupManifest{
		Schema:        manifestSchema,
		AppVersion:    "test",
		CreatedAt:     "2026-11-13T00:00:00+08:00",
		CreatedAtUnix: 1,
		DB:            manifestFile{File: unitDBName, SHA256: dbSum, Size: dbSize},
		Key:           manifestFile{File: unitKeyName, SHA256: keySum, Size: keySize},
	}
	body, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, manifestName), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, manifestName), 0o600); err != nil {
		t.Fatalf("设置 manifest 权限: %v", err)
	}
	return dir
}

func TestBackupUnitCreatedWithManifest(t *testing.T) {
	_, _, _, j := setup(t, 1<<30)
	root := t.TempDir()
	keySrc := filepath.Join(root, "secret.key")
	if err := os.WriteFile(keySrc, []byte("test-key-material"), 0o600); err != nil {
		t.Fatal(err)
	}
	j.opt.BackupDir = filepath.Join(root, "backup")
	j.opt.BackupKeep = 3
	j.opt.BackupKey = keySrc

	day := time.Date(2026, 11, 13, 3, 0, 0, 0, time.Local)
	j.backupDaily(day)

	unit := filepath.Join(j.opt.BackupDir, backupUnitsDir, backupPrefix+"20261113")
	ok, err := validBackupUnit(unit)
	if err != nil || !ok {
		t.Fatalf("备份单元无效: ok=%v err=%v", ok, err)
	}
	for _, n := range []string{unitDBName, unitKeyName, manifestName} {
		fi, err := os.Stat(filepath.Join(unit, n))
		if err != nil {
			t.Fatalf("缺少 %s: %v", n, err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s 权限过宽: %v", n, fi.Mode())
		}
	}
	// 单元目录之外不应出现裸 DB/key，否则 prune 会留下孤立文件。
	ents, _ := os.ReadDir(filepath.Join(j.opt.BackupDir, backupUnitsDir))
	for _, e := range ents {
		if !e.IsDir() {
			t.Fatalf("units 目录出现裸文件: %s", e.Name())
		}
	}
	// 同日重复执行应复用已有效的单元。
	j.backupDaily(day)
	if ok, _ := validBackupUnit(unit); !ok {
		t.Fatal("重复备份破坏了已有单元")
	}
}

func TestBackupUnitRebuiltAfterCorruption(t *testing.T) {
	_, _, _, j := setup(t, 1<<30)
	root := t.TempDir()
	keySrc := filepath.Join(root, "secret.key")
	if err := os.WriteFile(keySrc, []byte("test-key-material"), 0o600); err != nil {
		t.Fatal(err)
	}
	j.opt.BackupDir = filepath.Join(root, "backup")
	j.opt.BackupKeep = 3
	j.opt.BackupKey = keySrc
	day := time.Date(2026, 11, 13, 3, 0, 0, 0, time.Local)
	j.backupDaily(day)

	unit := filepath.Join(j.opt.BackupDir, backupUnitsDir, backupPrefix+"20261113")
	if err := os.WriteFile(filepath.Join(unit, unitDBName), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, _ := validBackupUnit(unit); ok {
		t.Fatal("被改坏的单元不应通过校验")
	}
	j.backupDaily(day)
	if ok, err := validBackupUnit(unit); err != nil || !ok {
		t.Fatalf("同名半成品应被整目录替换: ok=%v err=%v", ok, err)
	}
}

func TestBackupUnitRequiresKey(t *testing.T) {
	_, _, _, j := setup(t, 1<<30)
	root := t.TempDir()
	j.opt.BackupDir = filepath.Join(root, "backup")
	j.opt.BackupKeep = 3
	// BackupKey 为空：没有 key 的单元不是完整备份，不允许生成。
	j.backupDaily(time.Date(2026, 11, 13, 3, 0, 0, 0, time.Local))
	ents, _ := os.ReadDir(filepath.Join(j.opt.BackupDir, backupUnitsDir))
	if len(ents) != 0 {
		t.Fatalf("缺 key 时不应生成备份单元，得到 %v", ents)
	}
}

func TestBuildBackupUnitManifestChmodFailure(t *testing.T) {
	_, _, _, j := setup(t, 1<<30)
	root := t.TempDir()
	keySrc := filepath.Join(root, "secret.key")
	if err := os.WriteFile(keySrc, []byte("test-key-material"), 0o600); err != nil {
		t.Fatal(err)
	}
	j.opt.BackupDir = filepath.Join(root, "backup")
	j.opt.BackupKeep = 3
	j.opt.BackupKey = keySrc

	stage := t.TempDir()
	injected := errors.New("injected manifest chmod failure")
	old := manifestChmod
	manifestChmod = func(string, os.FileMode) error { return injected }
	defer func() { manifestChmod = old }()

	err := j.buildBackupUnit(stage, time.Date(2026, 11, 13, 3, 0, 0, 0, time.Local))
	if !errors.Is(err, injected) {
		t.Fatalf("应返回注入的 manifest 权限错误，实际 %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(stage, manifestName)); !os.IsNotExist(statErr) {
		t.Fatalf("Chmod 失败后不应存在正式 manifest，stat=%v", statErr)
	}
}

func TestBackupNowCleansStageOnManifestChmodFailure(t *testing.T) {
	_, _, _, j := setup(t, 1<<30)
	root := t.TempDir()
	keySrc := filepath.Join(root, "secret.key")
	if err := os.WriteFile(keySrc, []byte("test-key-material"), 0o600); err != nil {
		t.Fatal(err)
	}
	j.opt.BackupDir = filepath.Join(root, "backup")
	j.opt.BackupKeep = 3
	j.opt.BackupKey = keySrc

	existingDay := time.Date(2026, 11, 12, 3, 0, 0, 0, time.Local)
	if err := j.BackupNow(existingDay); err != nil {
		t.Fatalf("生成既有备份单元失败: %v", err)
	}
	units := filepath.Join(j.opt.BackupDir, backupUnitsDir)
	existing := filepath.Join(units, backupPrefix+"20261112")
	if ok, err := validBackupUnit(existing); err != nil || !ok {
		t.Fatalf("既有备份单元无效: ok=%v err=%v", ok, err)
	}

	injected := errors.New("injected manifest chmod failure")
	old := manifestChmod
	manifestChmod = func(string, os.FileMode) error { return injected }
	defer func() { manifestChmod = old }()

	err := j.BackupNow(time.Date(2026, 11, 13, 3, 0, 0, 0, time.Local))
	if !errors.Is(err, injected) {
		t.Fatalf("应返回注入的 manifest 权限错误，实际 %v", err)
	}

	ents, err := os.ReadDir(units)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != backupPrefix+"20261112" {
		t.Fatalf("失败后暂存目录应清理且既有备份保留，得到 %v", ents)
	}
	if ok, err := validBackupUnit(existing); err != nil || !ok {
		t.Fatalf("既有备份单元被破坏: ok=%v err=%v", ok, err)
	}
	if _, statErr := os.Stat(filepath.Join(units, backupPrefix+"20261113")); !os.IsNotExist(statErr) {
		t.Fatalf("失败后不应发布新的正式备份单元，stat=%v", statErr)
	}
}

func TestPartialStageIsNotValidUnit(t *testing.T) {
	dir := t.TempDir()
	unit := filepath.Join(dir, backupPrefix+"20261113")
	if err := os.MkdirAll(unit, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unit, unitDBName), []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unit, unitKeyName), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 备份在 manifest 落盘前被 SIGKILL：DB/key 都在，但没有 manifest。
	if ok, _ := validBackupUnit(unit); ok {
		t.Fatal("缺 manifest 的半成品不能算有效备份")
	}
}

func TestPruneRemovesStaleTmpAndKeepsInvalidUnit(t *testing.T) {
	units := t.TempDir()
	writeTestUnit(t, units, "20261112")
	stale := filepath.Join(units, ".tmp-"+backupPrefix+"20261113-123")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(units, backupPrefix+"20261111")
	if err := os.MkdirAll(broken, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, unitDBName), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}

	j := &Janitor{opt: Options{BackupKeep: 5}}
	j.pruneBackups(units, 5)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("过期 .tmp 目录应被清理")
	}
	if _, err := os.Stat(broken); err != nil {
		t.Error("无 manifest 的正式目录应保留待人工判断，不应静默删除")
	}
	if ok, _ := validBackupUnit(filepath.Join(units, backupPrefix+"20261112")); !ok {
		t.Error("有效单元不应被误删")
	}
}
