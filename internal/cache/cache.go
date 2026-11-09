// Package cache 是内容寻址缓存：按 sha256 存一份，索引在 SQLite，
// 同一文件不管来自哪个上游都只占一份空间。
package cache

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/zggnh12345/mirror-lens/internal/keymutex"
	"github.com/zggnh12345/mirror-lens/internal/metrics"
)

type Store struct {
	root string

	// blobLocks 串起发布与回收：GC 在锁内查引用并删，
	// 提交方在锁内 rename + 写索引，两者不交叉。
	blobLocks *keymutex.Map

	// tempActive 记录正在写的临时文件，SweepTemp 跳过。
	// 只靠 mtime 不够：连接卡住时文件一小时都不增长。
	tempActive sync.Map

	// fills 记录正在填充的 .part（按文件名）。文件锁只能覆盖「已经打开
	// 并持锁」的写入者；从拿到 Lease 到打开文件之间还有窗口。清理时
	// 必须在同一把互斥锁下先看这里再删，否则会把刚要续传的文件删掉。
	fillMu sync.Mutex
	fills  map[string]int
}

// BeginFill 标记某个 key 的 .part 正在被填充，返回结束标记。
// 必须在打开文件之前调用：先登记，再动文件，清理侧才不会在窗口期误删。
func (s *Store) BeginFill(key string) (end func()) {
	part, _ := s.PartialPaths(key)
	base := filepath.Base(part)
	s.fillMu.Lock()
	if s.fills == nil {
		s.fills = map[string]int{}
	}
	s.fills[base]++
	s.fillMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.fillMu.Lock()
			if n := s.fills[base]; n <= 1 {
				delete(s.fills, base)
			} else {
				s.fills[base] = n - 1
			}
			s.fillMu.Unlock()
		})
	}
}

// fillActiveLocked 在 fillMu 内判断是否有活动填充。调用方负责加锁。
func (s *Store) fillActiveLocked(base string) bool {
	return s.fills[base] > 0
}

func New(root string) (*Store, error) {
	if root == "" {
		return nil, fmt.Errorf("缓存目录为空")
	}
	for _, d := range []string{root, filepath.Join(root, "blobs"), filepath.Join(root, "tmp"), filepath.Join(root, "partial")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("创建缓存目录 %s: %w", d, err)
		}
	}
	// 能写才算可用，别等第一次下载才发现权限不对
	f, err := os.CreateTemp(filepath.Join(root, "tmp"), "probe-*")
	if err != nil {
		return nil, fmt.Errorf("缓存目录不可写（%s）：容器内以 uid 10001 运行，请先 chown", err)
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return &Store{root: root, blobLocks: keymutex.New()}, nil
}

func (s *Store) Root() string { return s.root }

// validSha 只认 64 位小写十六进制。路径由哈希拼成，
// 不校验字符集的话 "../../x" 这种串能爬出 blobs 目录。
func validSha(sha string) bool {
	if len(sha) != 64 {
		return false
	}
	for i := 0; i < len(sha); i++ {
		c := sha[i]
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				return false
			}
		}
	}
	return true
}

// BlobPath 按哈希前两位分桶，避免单目录几十万文件。
func (s *Store) BlobPath(sha string) string {
	if !validSha(sha) {
		return ""
	}
	return filepath.Join(s.root, "blobs", sha[:2], sha)
}

func (s *Store) Has(sha string) bool {
	if s.BlobPath(sha) == "" {
		return false
	}
	st, err := os.Stat(s.BlobPath(sha))
	return err == nil && st.Mode().IsRegular()
}

func (s *Store) CreateTemp() (*os.File, error) {
	f, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "dl-*")
	if err != nil {
		return nil, err
	}
	s.tempActive.Store(filepath.Base(f.Name()), struct{}{})
	return f, nil
}

// ForgetTemp 注销临时文件保护（已落位或已删）。
// 不调用就不会被 SweepTemp 回收，CreateTemp 后应立刻 defer。
func (s *Store) ForgetTemp(name string) {
	s.tempActive.Delete(filepath.Base(name))
}

// LockBlob 锁住某哈希的发布/回收，返回解锁函数。
// 只在进程内有效；janitor 与网关同进程，正是要互斥的两方。
func (s *Store) LockBlob(sha string) func() {
	return s.blobLocks.Lock(sha)
}

// Commit 按哈希落位临时文件；已存在同内容就丢弃临时文件，
// 去重靠这里：两个上游同样的字节只占一份。
//
// 提交顺序固定：文件 fsync → 关闭 → rename → 目录 fsync，之后调用方才写 SQLite。
// 少了目录 fsync，断电后可能出现「库里有记录、盘上没文件」。
func (s *Store) Commit(tmp, sha string) (string, error) {
	dst := s.BlobPath(sha)
	if dst == "" {
		return "", fmt.Errorf("非法哈希 %q", sha)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	if st, err := os.Stat(dst); err == nil && st.Mode().IsRegular() {
		os.Remove(tmp)
		return dst, nil
	}
	if err := syncFile(tmp); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	_ = os.Chmod(dst, 0o444)
	if err := syncDir(filepath.Dir(dst)); err != nil {
		return "", err
	}
	return dst, nil
}

// syncFile 把数据刷到盘；调用方可能还持有同一个文件句柄，这里另开只读句柄。
func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

var dirSyncWarned sync.Once

// syncDir 刷目录项。部分文件系统不支持对目录 fsync（EINVAL/ENOTSUP）：
// 这时不阻断缓存提交，但也不能记成「已经持久化」——目录项没有持久性保证，
// 掉电后 rename 可能回退。用日志和计数器把这种降级暴露给运维。
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
			dirSyncWarned.Do(func() {
				log.Printf("警告: 文件系统不支持目录 fsync（%v），缓存提交缺少目录项持久性保证: %s", err, dir)
			})
			metrics.DirSyncUnsupported()
			return nil
		}
		return err
	}
	return nil
}

func (s *Store) Open(sha string) (*os.File, error) {
	p := s.BlobPath(sha)
	if p == "" {
		return nil, fmt.Errorf("非法哈希 %q", sha)
	}
	return os.Open(p)
}

func (s *Store) Remove(sha string) error {
	p := s.BlobPath(sha)
	if p == "" {
		return nil
	}
	err := os.Remove(p)
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

// TotalSize 遍历磁盘，用于和索引对账。
func (s *Store) TotalSize() (int64, error) {
	var total int64
	err := filepath.Walk(filepath.Join(s.root, "blobs"), func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.Mode().IsRegular() && !strings.HasPrefix(info.Name(), ".") {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// SweepTemp 清掉崩溃留下的临时文件。
func (s *Store) SweepTemp(olderThan int64) (int, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, "tmp"))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if _, active := s.tempActive.Load(e.Name()); active {
			continue // 正在下载，哪怕 mtime 很旧也不能动
		}
		if info.ModTime().Unix() < olderThan {
			if os.Remove(filepath.Join(s.root, "tmp", e.Name())) == nil {
				n++
			}
		}
	}
	return n, nil
}
