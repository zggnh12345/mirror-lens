package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// PartialMeta 记录一个没下完的文件，够不够继续下由它说了算。
// 续传前必须核对：地址没变、上游版本没变（ETag/Last-Modified）、总长没变。
type PartialMeta struct {
	URL          string `json:"url"`
	ETag         string `json:"etag"`
	LastModified string `json:"last_modified"`
	ContentType  string `json:"content_type"`
	Size         int64  `json:"size"` // -1 表示上游没给长度，不能续传
	Have         int64  `json:"have"`
	Updated      int64  `json:"updated"`
}

func (s *Store) partialDir() string { return filepath.Join(s.root, "partial") }

// PartialPaths 返回 (数据文件, 元信息文件)。
func (s *Store) PartialPaths(key string) (string, string) {
	sum := sha256.Sum256([]byte(key))
	base := hex.EncodeToString(sum[:])[:40]
	return filepath.Join(s.partialDir(), base+".part"),
		filepath.Join(s.partialDir(), base+".json")
}

// ReadPartial 读取元信息，并核对数据文件确实有那么多字节。
func (s *Store) ReadPartial(key string) (*PartialMeta, bool) {
	part, meta := s.PartialPaths(key)
	fi, err := os.Stat(part)
	if err != nil || fi.Size() == 0 {
		return nil, false
	}
	b, err := os.ReadFile(meta)
	if err != nil {
		return nil, false
	}
	var m PartialMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, false
	}
	// 元信息说下了 N 字节，实际文件必须正好 N 字节，否则说明上次写坏了
	if m.Have != fi.Size() || m.Size < 0 || m.Have > m.Size {
		return nil, false
	}
	if m.ETag == "" && m.LastModified == "" {
		return nil, false // 没有验证依据，不敢续
	}
	return &m, true
}

func (s *Store) WritePartial(key string, m PartialMeta) error {
	_, meta := s.PartialPaths(key)
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := meta + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, meta)
}

func (s *Store) DropPartial(key string) {
	part, meta := s.PartialPaths(key)
	_ = os.Remove(part)
	_ = os.Remove(meta)
}

// OpenPartial 打开（必要时创建）数据文件，返回的文件指针已定位到末尾。
//
// 打开的同时加排他锁，Close 时自动释放：Janitor 靠这把锁判断"这份半截文件
// 是不是还在被写"。拿不到锁说明另一个进程正在下同一个文件，这里直接报错，
// 上层降级为透传——两个进程往同一个 .part 里追加会写出脏数据。
func (s *Store) OpenPartial(key string) (*os.File, error) {
	part, _ := s.PartialPaths(key)
	if err := os.MkdirAll(s.partialDir(), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(part, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	// 抢不到锁说明另一个进程正在写；不支持 flock 的文件系统返回 (false, nil)，继续用
	if _, lerr := lockFile(f); lerr != nil {
		f.Close()
		return nil, fmt.Errorf("半截文件被另一个进程占用: %w", lerr)
	}
	if _, err := f.Seek(0, 2); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// ResetPartial 清空数据文件，从头开始。
func (s *Store) ResetPartial(key string) error {
	part, _ := s.PartialPaths(key)
	if err := os.MkdirAll(s.partialDir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(part, nil, 0o644)
}
