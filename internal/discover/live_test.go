package discover

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 联网冒烟：列表站结构变了会立刻在数量上掉下来。
func TestScanLive(t *testing.T) {
	if os.Getenv("ML_LIVE") == "" {
		t.Skip("设置 ML_LIVE=1 才跑联网发现")
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	n, err := Scan(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if n < 20 {
		t.Fatalf("只发现 %d 个候选，解析规则可能失效", n)
	}
	t.Logf("新增 %d 个候选", n)
}
