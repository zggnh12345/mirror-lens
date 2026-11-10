package janitor

import (
	"github.com/zggnh12345/mirror-lens/internal/logging"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const backupPrefix = "mirror-lens-"

// backupDaily 每天留一份快照，失败只记日志：备份坏了不能影响主流程。
func (j *Janitor) backupDaily(now time.Time) {
	dir := j.opt.BackupDir
	keep := j.opt.BackupKeep
	if dir == "" || keep <= 0 {
		return
	}
	name := backupPrefix + now.Format("20060102") + ".db"
	final := filepath.Join(dir, name)
	if _, err := os.Stat(final); err == nil {
		j.pruneBackups(dir, keep)
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		logging.Errorf("janitor: 建备份目录失败: %v", err)
		return
	}
	tmp := final + ".tmp"
	_ = os.Remove(tmp)
	if err := j.st.BackupTo(tmp); err != nil {
		_ = os.Remove(tmp)
		logging.Errorf("janitor: 数据库快照失败: %v", err)
		return
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		logging.Errorf("janitor: 快照改名失败: %v", err)
		return
	}
	// VACUUM INTO 建的文件按 0666&umask 落权限，库里是加密后的源凭据，收紧到 0600。
	if err := os.Chmod(final, 0o600); err != nil {
		logging.Errorf("janitor: 收紧快照权限失败: %v", err)
	}
	// 配置里的源凭据用 secret.key 加密，数据库单独恢复解不开。
	j.copyKey(dir)
	j.pruneBackups(dir, keep)
}

func (j *Janitor) copyKey(dir string) {
	if j.opt.BackupKey == "" {
		return
	}
	src, err := os.Open(j.opt.BackupKey)
	if err != nil {
		return
	}
	defer src.Close()
	dst, err := os.OpenFile(filepath.Join(dir, "secret.key"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		logging.Errorf("janitor: 备份 secret.key 失败: %v", err)
		return
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		logging.Errorf("janitor: 写 secret.key 失败: %v", err)
	}
	// 实测创建时的 mode 在这块盘上不生效（落到 705），收尾显式收紧。
	if err := os.Chmod(dst.Name(), 0o600); err != nil {
		logging.Errorf("janitor: 收紧 secret.key 权限失败: %v", err)
	}
}

// pruneBackups 按文件名（日期）排序，只留最近 keep 份。
func (j *Janitor) pruneBackups(dir string, keep int) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, backupPrefix) || !strings.HasSuffix(n, ".db") {
			continue
		}
		names = append(names, n)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	if len(names) <= keep {
		return
	}
	for _, n := range names[keep:] {
		if err := os.Remove(filepath.Join(dir, n)); err != nil {
			logging.Errorf("janitor: 删除旧快照失败: %v", err)
		}
	}
}
