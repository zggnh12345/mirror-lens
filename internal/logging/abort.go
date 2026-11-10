package logging

import (
	"context"
	"errors"
	"net"
	"strings"
)

// IsAbort 判断错误是不是「我们自己主动掐掉」的那一类：
// 双栈竞速里输掉的那条连接、换源后旧连接被关闭、客户端先走导致的取消。
// 这些不是源故障，不该进错误日志，更不该进告警与源健康统计。
func IsAbort(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed) {
		return true
	}
	s := err.Error()
	for _, m := range []string{
		"operation was canceled",
		"use of closed network connection",
		"connection reset by peer",
		"broken pipe",
	} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
