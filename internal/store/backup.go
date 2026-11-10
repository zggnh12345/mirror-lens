package store

import (
	"fmt"
	"os"
	"strings"
)

// BackupTo 用 VACUUM INTO 生成一份一致性的单文件快照。
//
// 相比 cp 数据库文件，VACUUM INTO 不需要停写、不需要处理 -wal/-shm，
// 产物也已整理过（没有空洞、没有回放日志），恢复时直接替换即可。
// 目标必须不存在，否则 SQLite 会报错——调用方用临时名 + rename 保证原子。
func (s *Store) BackupTo(path string) error {
	if path == "" {
		return fmt.Errorf("备份路径为空")
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("备份目标已存在: %s", path)
	}
	q := `VACUUM INTO '` + strings.ReplaceAll(path, "'", "''") + `'`
	if _, err := s.db.Exec(q); err != nil {
		return fmt.Errorf("VACUUM INTO: %w", err)
	}
	return nil
}
