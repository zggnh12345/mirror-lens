package gateway

import (
	"path/filepath"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/store"
	"github.com/zggnh12345/mirror-lens/internal/v2auth"
)

// testGatewayPolicy 只给单元测试使用，复刻 11a 的按 Host 查表行为。
// 生产路径必须显式注入 store.NewStorePolicy；Gateway 默认只允许官方直连。
var testGatewayPolicy = v2auth.NewLegacyHostPolicy(nil, true)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
