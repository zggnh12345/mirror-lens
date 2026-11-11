//go:build !unix

package secret

import "os"

// 非 unix 平台没有 flock；进程内锁仍然生效。
func flockExclusive(*os.File) error { return nil }

func funlock(*os.File) error { return nil }
