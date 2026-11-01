package cache

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// blobs 是正式内容，tmp 是下载中的临时文件，partial 是续传用的半截文件。

// DiskUsage 按磁盘真实文件统计占用，不信索引里的 size：
// 文件可能被外部删掉而索引还挂着。WalkDir 不跟随符号链接。
func (s *Store) DiskUsage() (int64, error) {
	var total int64
	err := filepath.WalkDir(s.root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// ListBlobs 返回所有 blob 的 sha（文件名即哈希）。
func (s *Store) ListBlobs() ([]string, error) {
	root := filepath.Join(s.root, "blobs")
	shards, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, sh := range shards {
		if !sh.IsDir() || len(sh.Name()) != 2 {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, sh.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.Type()&os.ModeSymlink != 0 || f.IsDir() {
				continue
			}
			out = append(out, f.Name())
		}
	}
	return out, nil
}

// FSFree 返回文件系统剩余字节；这与配额允许用多少是两件事，面板分开显示。
func (s *Store) FSFree() (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.root, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

var flockWarn sync.Once

// lockFile 加排他锁，拿不到即别的进程在写。
// 文件系统不支持 flock 时返回 false 不报错：挂 NFS 不能让缓存整体失效。
func lockFile(f *os.File) (ok bool, err error) {
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		return false, err
	case errors.Is(err, syscall.ENOTSUP), errors.Is(err, syscall.ENOLCK), errors.Is(err, syscall.EINVAL):
		flockWarn.Do(func() {
			log.Printf("缓存目录不支持文件锁（%v），跨进程保护降级为仅按时间判断", err)
		})
		return false, nil
	}
	return false, err
}

// SweepPartial 清理久未续传的半截文件。.part 与 .json 必须配对删，
// 只删一个会让下次续传拿到对不上的长度。
// 判断在用不靠 mtime（刚打开还没写的任务 mtime 也旧），而是抢文件锁：
// 抢到才算没人写，并持锁删除。
func (s *Store) SweepPartial(olderThan int64) (files int, freed int64, err error) {
	dir := s.partialDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	// 按 base 归并：base.part + base.json
	bases := map[string]bool{}
	for _, e := range entries {
		n := e.Name()
		switch {
		case strings.HasSuffix(n, ".part"):
			bases[strings.TrimSuffix(n, ".part")] = true
		case strings.HasSuffix(n, ".json"):
			bases[strings.TrimSuffix(n, ".json")] = true
		case strings.HasSuffix(n, ".json.tmp"):
			// 写元信息时崩了留下的，直接清
			p := filepath.Join(dir, n)
			if fi, e2 := os.Stat(p); e2 == nil && fi.ModTime().Unix() < olderThan {
				if os.Remove(p) == nil {
					files++
				}
			}
		}
	}
	for base := range bases {
		part := filepath.Join(dir, base+".part")
		meta := filepath.Join(dir, base+".json")
		if !s.stalePartial(part, meta, olderThan) {
			continue
		}
		n, sz, ok := s.dropPartialLocked(part, meta)
		if ok {
			files += n
			freed += sz
		}
	}
	return files, freed, nil
}

// stalePartial 取两个文件里较新的 mtime，都超期才算 stale。
func (s *Store) stalePartial(part, meta string, olderThan int64) bool {
	newest := int64(0)
	for _, p := range []string{part, meta} {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if fi.ModTime().Unix() > newest {
			newest = fi.ModTime().Unix()
		}
	}
	return newest > 0 && newest < olderThan
}

// dropPartialLocked 抢到锁才删；抢不到说明有进程在续传，跳过。
func (s *Store) dropPartialLocked(part, meta string) (files int, freed int64, ok bool) {
	f, err := os.OpenFile(part, os.O_RDWR, 0o644)
	if err != nil {
		if !os.IsNotExist(err) {
			return 0, 0, false
		}
		// 数据没了，元信息留着没用
		if fi, e := os.Stat(meta); e == nil {
			if os.Remove(meta) == nil {
				return 1, fi.Size(), true
			}
		}
		return 0, 0, false
	}
	defer f.Close()

	locked, lerr := lockFile(f)
	if lerr != nil {
		return 0, 0, false // EWOULDBLOCK：有人在写
	}
	if locked {
		defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}
	// 不支持锁的文件系统退回只按时间判断

	var size int64
	for _, p := range []string{part, meta} {
		if fi, e := os.Stat(p); e == nil {
			size += fi.Size()
		}
		if os.Remove(p) == nil {
			files++
		}
	}
	return files, size, true
}
