package janitor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/logging"
	"github.com/zggnh12345/mirror-lens/internal/version"
)

const (
	backupPrefix   = "mirror-lens-"
	backupUnitsDir = "units"
	manifestName   = "manifest.json"
	manifestSchema = 1
	unitDBName     = "mirror-lens.db"
	unitKeyName    = "secret.key"
)

// manifestChmod 是 manifest 权限设置的测试注入点；生产默认行为与 os.Chmod 一致。
var manifestChmod = os.Chmod

// manifestFile 是备份单元里单个文件的完整性记录。
type manifestFile struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// backupManifest 是备份单元的有效性标记：没有它的目录一律视为不完整。
// 它是最后发布的文件，因此「manifest 存在且校验通过」等价于整份备份已落盘。
type backupManifest struct {
	Schema        int          `json:"schema"`
	AppVersion    string       `json:"app_version"`
	CreatedAt     string       `json:"created_at"`
	CreatedAtUnix int64        `json:"created_at_unix"`
	DB            manifestFile `json:"db"`
	Key           manifestFile `json:"key"`
}

// backupDaily 每天留一份完整备份单元（DB + key + manifest），失败只记日志：
// 备份坏了不能影响主流程。
func (j *Janitor) backupDaily(now time.Time) {
	if err := j.BackupNow(now); err != nil {
		logging.Errorf("janitor: 备份失败: %v", err)
	}
}

// BackupNow 立即生成一份备份单元。备份目录或保留份数未配置时是空操作。
// 返回错误供需要确认结果的调用方（发布验证、运维脚本）使用。
func (j *Janitor) BackupNow(now time.Time) error {
	dir := j.opt.BackupDir
	keep := j.opt.BackupKeep
	if dir == "" || keep <= 0 {
		return nil
	}
	units := filepath.Join(dir, backupUnitsDir)
	if err := os.MkdirAll(units, 0o700); err != nil {
		return fmt.Errorf("建备份目录: %w", err)
	}
	final := filepath.Join(units, backupPrefix+now.Format("20060102"))
	if ok, _ := validBackupUnit(final); ok {
		j.pruneBackups(units, keep)
		return nil
	}
	stage, err := os.MkdirTemp(units, ".tmp-"+backupPrefix+now.Format("20060102")+"-")
	if err != nil {
		return fmt.Errorf("建备份暂存目录: %w", err)
	}
	// rename 成功后 stage 已不存在，RemoveAll 是空操作。
	defer os.RemoveAll(stage)
	if err := j.buildBackupUnit(stage, now); err != nil {
		return err
	}
	// 同名目录存在但校验不通过：是上次中断的半成品，整目录替换。
	if _, err := os.Stat(final); err == nil {
		if err := os.RemoveAll(final); err != nil {
			return fmt.Errorf("清理半成品备份: %w", err)
		}
	}
	if err := os.Rename(stage, final); err != nil {
		return fmt.Errorf("发布备份单元: %w", err)
	}
	if err := syncDir(units); err != nil {
		return fmt.Errorf("同步备份目录: %w", err)
	}
	j.pruneBackups(units, keep)
	return nil
}

// buildBackupUnit 在暂存目录里按「数据文件 → key → manifest」的顺序落盘。
// manifest 放最后，崩溃时最多留下没有 manifest 的暂存目录，不会被当成有效备份。
func (j *Janitor) buildBackupUnit(stage string, now time.Time) error {
	if j.opt.BackupKey == "" {
		return errors.New("未配置 secret.key，无法生成完整备份单元")
	}
	dbPath := filepath.Join(stage, unitDBName)
	if err := j.st.BackupTo(dbPath); err != nil {
		return fmt.Errorf("数据库快照: %w", err)
	}
	if err := os.Chmod(dbPath, 0o600); err != nil {
		return fmt.Errorf("收紧快照权限: %w", err)
	}
	if err := syncFile(dbPath); err != nil {
		return fmt.Errorf("同步快照: %w", err)
	}
	keyPath := filepath.Join(stage, unitKeyName)
	if err := copyFileSync(j.opt.BackupKey, keyPath, 0o600); err != nil {
		return fmt.Errorf("复制 secret.key: %w", err)
	}
	dbSum, dbSize, err := hashFile(dbPath)
	if err != nil {
		return err
	}
	keySum, keySize, err := hashFile(keyPath)
	if err != nil {
		return err
	}
	m := backupManifest{
		Schema:        manifestSchema,
		AppVersion:    version.Version,
		CreatedAt:     now.Format(time.RFC3339),
		CreatedAtUnix: now.Unix(),
		DB:            manifestFile{File: unitDBName, SHA256: dbSum, Size: dbSize},
		Key:           manifestFile{File: unitKeyName, SHA256: keySum, Size: keySize},
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	tmp := filepath.Join(stage, manifestName+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := manifestChmod(tmp, 0o600); err != nil {
		return fmt.Errorf("设置 manifest 权限: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(stage, manifestName)); err != nil {
		return err
	}
	return syncDir(stage)
}

// validBackupUnit 校验 manifest 结构、文件大小与 SHA-256。
func validBackupUnit(dir string) (bool, error) {
	raw, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return false, err
	}
	var m backupManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return false, err
	}
	if m.Schema != manifestSchema || m.AppVersion == "" || m.DB.File != unitDBName || m.Key.File != unitKeyName {
		return false, errors.New("manifest 字段不合法")
	}
	for _, f := range []struct {
		meta manifestFile
		name string
	}{{m.DB, unitDBName}, {m.Key, unitKeyName}} {
		p := filepath.Join(dir, f.name)
		fi, err := os.Stat(p)
		if err != nil {
			return false, err
		}
		if fi.Size() != f.meta.Size || fi.Mode().Perm()&0o077 != 0 {
			return false, errors.New("备份文件大小或权限不符")
		}
		sum, _, err := hashFile(p)
		if err != nil {
			return false, err
		}
		if !strings.EqualFold(sum, f.meta.SHA256) {
			return false, errors.New("备份文件校验和不符")
		}
	}
	return true, nil
}

// pruneBackups 只按完整备份单元（目录）清理，保留最近 keep 份；
// 同时清掉中断留下的暂存目录，避免孤立 DB/key 长期占位。
func (j *Janitor) pruneBackups(units string, keep int) {
	ents, err := os.ReadDir(units)
	if err != nil {
		return
	}
	var names []string
	for _, e := range ents {
		n := e.Name()
		if !e.IsDir() || !strings.HasPrefix(n, backupPrefix) {
			continue
		}
		p := filepath.Join(units, n)
		if ok, _ := validBackupUnit(p); !ok {
			logging.Errorf("janitor: 备份单元 %s 校验未通过，保留但不算入保留份数", n)
			continue
		}
		names = append(names, n)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	if len(names) > keep {
		for _, n := range names[keep:] {
			if err := os.RemoveAll(filepath.Join(units, n)); err != nil {
				logging.Errorf("janitor: 删除旧备份单元失败: %v", err)
			}
		}
	}
	cutoff := time.Now().Add(-tmpTTL).Unix()
	for _, e := range ents {
		n := e.Name()
		if !e.IsDir() || !strings.HasPrefix(n, ".tmp-") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().Unix() > cutoff {
			continue
		}
		_ = os.RemoveAll(filepath.Join(units, n))
	}
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func copyFileSync(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Chmod(mode); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
