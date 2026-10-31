package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "tx.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestTxRollsBack(t *testing.T) {
	st := openTest(t)
	if err := st.SetSetting("k", "old"); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("boom")
	err := st.Tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO settings(key,value) VALUES('k','new') ON CONFLICT(key) DO UPDATE SET value='new'`); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO settings(key,value) VALUES('k2','v2')`); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("应把 fn 的错误原样返回，得到 %v", err)
	}
	if got := st.GetSetting("k", ""); got != "old" {
		t.Fatalf("回滚后 k 应仍是 old，实际 %q", got)
	}
	if got := st.GetSetting("k2", "missing"); got != "missing" {
		t.Fatalf("回滚后 k2 不该存在，实际 %q", got)
	}
}

func TestTxCommits(t *testing.T) {
	st := openTest(t)
	if err := st.Tx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO settings(key,value) VALUES('k','v') ON CONFLICT(key) DO UPDATE SET value='v'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := st.GetSetting("k", ""); got != "v" {
		t.Fatalf("提交后应读到 v，实际 %q", got)
	}
}
