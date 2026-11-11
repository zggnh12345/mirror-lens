//go:build unix

package secret

import (
	"os"
	"syscall"
)

// flockExclusive 用 flock 做跨进程互斥：同机另一个进程改同一份凭据文件时
// 会阻塞，避免两边各自读旧文件再互相覆盖（Lost Update）。
func flockExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

func funlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
