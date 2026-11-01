// Package logging 提供统一的日志初始化。
//
// 只依赖标准库 log/slog：不引入 Zap、Zerolog 这类框架。目标是让
// 所有模块输出同一种结构，字段名固定，日志能直接进 Loki/ELK。
package logging

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/url"
	"os"
	"strings"
)

// Config 对应配置文件里的 logging 段。
type Config struct {
	// Level: debug / info / warn / error，默认 info。
	Level string `yaml:"level"`
	// Format: text / json，默认 text（Docker 日志里人眼看）。
	Format string `yaml:"format"`
}

// Setup 初始化全局 logger，并把标准库 log 的输出接进来。
//
// 接管标准库这一步是有意的：仓库里还有大量 log.Printf 没迁移，接管之后
// 它们立刻按同一格式输出，不必为了统一格式去做一次性大改。
func Setup(c Config) error { return setupTo(c, os.Stdout) }

func setupTo(c Config, w io.Writer) error {
	lvl, err := parseLevel(c.Level)
	if err != nil {
		return err
	}
	format := strings.ToLower(strings.TrimSpace(c.Format))
	if format == "" {
		format = "text"
	}

	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	switch format {
	case "text":
		h = slog.NewTextHandler(w, opts)
	case "json":
		h = slog.NewJSONHandler(w, opts)
	default:
		return fmt.Errorf("logging.format 只支持 text/json，收到 %q", c.Format)
	}

	logger := slog.New(h)
	slog.SetDefault(logger)

	// 标准库 log 默认带时间前缀，接管后由 slog 统一加时间，避免重复。
	log.SetFlags(0)
	log.SetPrefix("")
	log.SetOutput(&stdWriter{logger: logger})
	return nil
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("logging.level 只支持 debug/info/warn/error，收到 %q", s)
	}
}

// stdWriter 把 log.Printf 的整行当成 msg 交给 slog。
type stdWriter struct{ logger *slog.Logger }

func (w *stdWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\r\n")
	if msg != "" {
		w.logger.Log(context.Background(), slog.LevelInfo, msg)
	}
	return len(p), nil
}

// Writer 返回一个把写入内容当普通消息记录的 io.Writer，
// 给 http.Server.ErrorLog 这类只认 io.Writer 的地方用。
func Writer(l *slog.Logger, level slog.Level) io.Writer {
	if l == nil {
		l = slog.Default()
	}
	return &levelWriter{logger: l, level: level}
}

type levelWriter struct {
	logger *slog.Logger
	level  slog.Level
}

func (w *levelWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\r\n")
	if msg != "" {
		w.logger.Log(context.Background(), w.level, msg)
	}
	return len(p), nil
}

// SanitizeKey 处理缓存键：http 类键里嵌着完整 URL，查询串同样要砍掉。
func SanitizeKey(k string) string {
	if i := strings.IndexByte(k, '?'); i >= 0 {
		return k[:i] + "?REDACTED"
	}
	return k
}

// SanitizeURL 把 URL 里可能带凭据的部分去掉，只留下可用于排查的骨架。
//
// 不能直接把原始 URL 写进日志：私有仓库、预签名链接、带 token 的回源地址
// 都会原样泄进去。
func SanitizeURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		// 解析不了就别猜，整段丢掉，只留长度方便判断是不是空。
		return "<unparsable-url>"
	}
	u.User = nil
	// 查询串整段丢掉，而不是逐个打码：回源地址里的查询参数往往就是签名，
	// 与其判断哪个敏感，不如一个都不留。路径保留，够定位了。
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
