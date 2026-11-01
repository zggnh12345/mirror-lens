package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// PartialMeta 记录没下完的文件。续传前要核对地址、
// 上游版本（ETag/Last-Modified）和总长都没变。
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

// ReadPartial 读元信息，并核对数据文件字节数吻合。
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
	// 元信息与实际文件字节数必须一致，否则上次写坏了
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

// OpenPartial 打开（必要时创建）数据文件，指针定位到末尾，同时加排他锁，
// Close 时自动释放；Janitor 靠这把锁判断是否还在被写。
// 拿不到锁说明另一进程在下同一文件，直接报错，上层降级为透传。
func (s *Store) OpenPartial(key string) (*os.File, error) {
	part, _ := s.PartialPaths(key)
	if err := os.MkdirAll(s.partialDir(), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(part, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	// 不支持 flock 的文件系统返回 (false, nil)，照常继续
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
