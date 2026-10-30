package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/api"
	"github.com/zggnh12345/mirror-lens/internal/discover"
	"github.com/zggnh12345/mirror-lens/internal/runner"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

func main() {
	addr := flag.String("addr", envOr("ADDR", ":8080"), "监听地址")
	dataDir := flag.String("data", envOr("DATA_DIR", "/data"), "数据目录")
	noSeed := flag.Bool("no-seed", false, "不写入内置候选池")
	flag.Parse()

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatalf("创建数据目录: %v", err)
	}
	st, err := store.Open(filepath.Join(*dataDir, "mirror-lens.db"))
	if err != nil {
		log.Fatalf("打开数据库: %v", err)
	}
	defer st.Close()

	if !*noSeed {
		if n, err := discover.Seed(st); err != nil {
			log.Printf("写入候选池失败: %v", err)
		} else if n > 0 {
			log.Printf("已写入 %d 个内置候选源", n)
		}
	}

	eng := runner.New(st)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go eng.Loop(ctx)

	// 每天扫一次公开列表，新域名进 pending 池
	go func() {
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n, err := discover.Scan(ctx, st); err != nil {
					log.Printf("发现新源失败: %v", err)
				} else if n > 0 {
					log.Printf("发现 %d 个新候选", n)
					eng.Run("docker", 2)
					eng.Run("github", 2)
				}
			}
		}
	}()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           api.New(st, eng),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("Mirror Lens 已启动: http://%s", *addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP: %v", err)
		}
	}()

	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	log.Print("已退出")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
