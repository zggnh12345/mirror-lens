// Package logging 提供统一的日志初始化。
//
// 只用标准库 log/slog，不引框架：统一结构和字段名，日志能直接进 Loki/ELK。
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

// Config 对应 logging 配置段。
type Config struct {
	// Level: debug / info / warn / error，默认 info。
	Level string `yaml:"level"`
	// Format: text / json，默认 text（Docker 日志里人眼看）。
	Format string `yaml:"format"`
}

// Setup 初始化全局 logger 并接管标准库 log。
// 仓库里还有大量 log.Printf 没迁移，接管后立刻统一格式，不必一次性大改。
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

	// 清掉标准库的时间前缀，由 slog 统一加，避免重复。
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

// stdWriter 把 log.Printf 的整行当 msg 交给 slog。
type stdWriter struct{ logger *slog.Logger }

func (w *stdWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\r\n")
	if msg != "" {
		w.logger.Log(context.Background(), slog.LevelInfo, msg)
	}
	return len(p), nil
}

// Writer 把写入内容当普通消息记录，给 http.Server.ErrorLog 这类地方用。
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

// SanitizeKey 砍掉缓存键里的查询串：http 键嵌着完整 URL。
func SanitizeKey(k string) string {
	if i := strings.IndexByte(k, '?'); i >= 0 {
		return k[:i] + "?REDACTED"
	}
	return k
}

// SanitizeURL 去掉 URL 里可能带凭据的部分，只留排查用的骨架。
// 原始 URL 里有私有仓库、预签名链接和 token，不能直接进日志。
func SanitizeURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		// 解析不了就不猜，换成固定占位符。
		return "<unparsable-url>"
	}
	u.User = nil
	// 查询串整段丢掉而不是逐个打码：里面往往就是签名。
	// 路径保留，够定位。
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
