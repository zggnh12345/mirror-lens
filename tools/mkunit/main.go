// 发布验证工具：用正式的 janitor 备份实现生成一份备份单元。
// 只对数据库副本运行，不接受生产路径。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/janitor"
	"github.com/zggnh12345/mirror-lens/internal/store"
	_ "modernc.org/sqlite"
)

func main() {
	db := flag.String("db", "", "数据库副本路径")
	key := flag.String("key", "", "secret.key 路径（随单元复制）")
	backup := flag.String("backup", "", "备份根目录")
	keep := flag.Int("keep", 3, "保留份数")
	flag.Parse()
	if *db == "" || *key == "" || *backup == "" {
		fmt.Fprintln(os.Stderr, "用法: mkunit -db <副本> -key <secret.key> -backup <目录>")
		os.Exit(2)
	}
	st, err := store.Open(*db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开副本失败:", err)
		os.Exit(1)
	}
	defer st.Close()
	j := janitor.New(st, nil, nil, janitor.Options{BackupDir: *backup, BackupKeep: *keep, BackupKey: *key})
	now := time.Now()
	if err := j.BackupNow(now); err != nil {
		fmt.Fprintln(os.Stderr, "生成备份单元失败:", err)
		os.Exit(1)
	}
	fmt.Println(filepath.Join(*backup, "units", "mirror-lens-"+now.Format("20060102")))
}
