package api

import (
	"context"
	"net/http"
	"os"
	"time"
)

// readyCheck 是单项检查结果。error 只在失败时出现，避免把内部路径
// 和 SQL 错误无谓地暴露在公网探针响应里——部署在内网也会被截图贴出去。
type readyCheck struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Elapsed int64  `json:"elapsed_ms"`
}

// readyz 回答「现在能不能正常干活」。
//
// 只看本机依赖：数据库可写、缓存目录可用。
// 刻意不检查公网源站——源站挂了 Mirror Lens 依然能服务缓存、依然能
// 回源到别的源，把它算成就绪失败只会让编排器把好好的实例摘掉。
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	checks := map[string]readyCheck{}
	allOK := true

	t0 := time.Now()
	dbErr := s.st.Ready(ctx)
	dbCheck := readyCheck{OK: dbErr == nil, Elapsed: time.Since(t0).Milliseconds()}
	if dbErr != nil {
		dbCheck.Error = "数据库不可写"
		allOK = false
	}
	checks["db"] = dbCheck

	t0 = time.Now()
	cacheErr := s.cacheReady()
	cacheCheck := readyCheck{OK: cacheErr == nil, Elapsed: time.Since(t0).Milliseconds()}
	if cacheErr != nil {
		cacheCheck.Error = cacheErr.Error()
		allOK = false
	}
	checks["cache"] = cacheCheck

	code := http.StatusOK
	status := "ok"
	if !allOK {
		code = http.StatusServiceUnavailable
		status = "unavailable"
	}
	writeJSON(w, code, map[string]any{
		"status": status,
		"checks": checks,
		"ts":     time.Now().Unix(),
	})
}

// cacheReady 只做目录级检查：stat + 剩余空间，不建文件。
// 每次探针都写一个临时文件的话，K8s 的 1s 探针会把缓存目录刷满小文件。
func (s *Server) cacheReady() error {
	root := s.gw.CacheRoot()
	if root == "" {
		return errCacheNotConfigured
	}
	fi, err := os.Stat(root)
	if err != nil {
		return errCacheUnreadable
	}
	if !fi.IsDir() {
		return errCacheNotDir
	}
	free, err := s.gw.FSFree()
	if err != nil {
		return errCacheUnreadable
	}
	if free <= 0 {
		return errCacheFull
	}
	return nil
}

type readyError string

func (e readyError) Error() string { return string(e) }

const (
	errCacheNotConfigured readyError = "缓存目录未配置"
	errCacheUnreadable    readyError = "缓存目录不可访问"
	errCacheNotDir        readyError = "缓存路径不是目录"
	errCacheFull          readyError = "缓存磁盘已满"
)
