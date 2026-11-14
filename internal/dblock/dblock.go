// Package dblock 提供数据目录级的单实例锁。
//
// 锁文件固定放在数据目录，而不是数据库文件本身：数据库切换会 rename 新文件，
// 依赖数据库 inode 的锁会随之失效。锁覆盖整个进程生命周期，服务、离线迁移和
// 恢复脚本必须使用同一个文件。
package dblock

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Name 是锁文件名。恢复脚本使用同一个名字。
const Name = ".mirror-lens.lock"

// Lock 持有数据目录锁。Close 释放并关闭文件描述符。
type Lock struct {
	f *os.File
}

// Acquire 以非阻塞方式取得 path 的排他锁。
//
// 返回的错误可以用 IsBusy 判断是否只是另一个进程持有锁；其他错误（权限、
// 文件系统不支持 flock）也必须让调用方 fail-closed。
func Acquire(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开数据库锁 %s: %w", path, err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("设置锁文件权限: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := readHolder(f)
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			if holder != "" {
				return nil, fmt.Errorf("数据库已被进程 %s 锁定（%s）", holder, path)
			}
			return nil, fmt.Errorf("数据库已被其他进程锁定（%s）", path)
		}
		return nil, fmt.Errorf("锁定数据库 %s: %w", path, err)
	}
	// 锁内容只是诊断信息，不参与正确性判断；写入失败不释放锁。
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	_ = f.Sync()
	return &Lock{f: f}, nil
}

// IsBusy 判断错误是否只是锁被另一个进程持有。
func IsBusy(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "已被进程") || strings.Contains(err.Error(), "已被其他进程"))
}

func readHolder(f *os.File) string {
	b, err := os.ReadFile(f.Name())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Close 释放锁。重复调用是安全的。
func (l *Lock) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	// 先解锁再关闭；关闭本身也会释放 flock，但显式解锁让错误可观察。
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return f.Close()
}
