package auth

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

func openAuthStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// 初始化中途失败必须整体回滚：不能留下「有 hash 但没有 must_change」的状态。
func TestEnsureAdminRollsBackOnFailure(t *testing.T) {
	st := openAuthStore(t)

	boom := errors.New("注入失败")
	err := st.Tx(func(tx *sql.Tx) error {
		if err := ensureAdminTx(tx); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("应返回注入的错误，得到 %v", err)
	}
	if got := st.GetSetting(KeyHash, ""); got != "" {
		t.Fatalf("回滚后不该留下密码 hash，实际长度 %d", len(got))
	}
	if got := st.GetSetting(KeyMustChg, ""); got != "" {
		t.Fatalf("回滚后不该留下 must_change，实际 %q", got)
	}

	// 重新启动：仍然按首次初始化处理，默认密码必须要求改密。
	if err := EnsureAdmin(st); err != nil {
		t.Fatal(err)
	}
	if st.GetSetting(KeyHash, "") == "" {
		t.Fatal("重新初始化应写入默认密码 hash")
	}
	if got := st.GetSetting(KeyMustChg, ""); got != "1" {
		t.Fatalf("首次初始化后必须强制改密，must_change=%q", got)
	}
}

// 旧版本崩溃留下的半成品：hash 在、must_change 缺失，必须补上强制改密。
func TestEnsureAdminRepairsMissingMustChange(t *testing.T) {
	st := openAuthStore(t)
	if err := st.SetSetting(KeyUser, DefaultUser); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(KeyHash, "legacy-hash"); err != nil {
		t.Fatal(err)
	}

	if err := EnsureAdmin(st); err != nil {
		t.Fatal(err)
	}
	if got := st.GetSetting(KeyHash, ""); got != "legacy-hash" {
		t.Fatalf("不该覆盖已有密码，实际 %q", got)
	}
	if got := st.GetSetting(KeyMustChg, ""); got != "1" {
		t.Fatalf("缺失的 must_change 应补成 1，实际 %q", got)
	}
}

// 正常改过密码的实例（must_change=0）不能被重新武装。
func TestEnsureAdminKeepsCompletedState(t *testing.T) {
	st := openAuthStore(t)
	for _, kv := range [][2]string{
		{KeyUser, "alex"},
		{KeyHash, "changed-hash"},
		{KeyMustChg, "0"},
	} {
		if err := st.SetSetting(kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}

	if err := EnsureAdmin(st); err != nil {
		t.Fatal(err)
	}
	if got := st.GetSetting(KeyMustChg, ""); got != "0" {
		t.Fatalf("已完成初始化的实例不该再要求改密，实际 %q", got)
	}
	if got := st.GetSetting(KeyUser, ""); got != "alex" {
		t.Fatalf("不该覆盖用户名，实际 %q", got)
	}
}
