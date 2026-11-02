package store

import (
	"path/filepath"
	"testing"
	"time"
)

// 过期记录要真的删掉：审计表只增不删迟早把盘吃满。
func TestPurgeAuditKeepsRecent(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	old := time.Now().Add(-100 * 24 * time.Hour).Unix()
	recent := time.Now().Add(-time.Hour).Unix()
	for _, ts := range []int64{old, recent} {
		if _, err := st.db.Exec(
			`INSERT INTO audit(ts,ip,user,action,target,detail,ok) VALUES(?,?,?,?,?,?,?)`,
			ts, "127.0.0.1", "admin", "login", "", "ok", 1); err != nil {
			t.Fatal(err)
		}
	}

	n, err := st.PurgeAudit(time.Now().Add(-90 * 24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应删 1 条，实删 %d", n)
	}
	list, err := st.ListAudit(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].TS != recent {
		t.Fatalf("留下的不该是过期记录: %+v", list)
	}
}
