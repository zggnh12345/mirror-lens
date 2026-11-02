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

// 关闭时必须把已入队的事件写完，再关数据库。
func TestAuditFlushesOnClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	const n = 200
	for i := 0; i < n; i++ {
		st.Audit(AuditEvent{IP: "127.0.0.1", User: "admin", Action: "login", OK: true})
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	list, err := st2.ListAudit(500)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != n {
		t.Fatalf("关闭时应排空队列：实存 %d 条，预期 %d 条", len(list), n)
	}

	// 关闭后再写是空操作：不 panic，也不该悄悄往别处写。
	st.Audit(AuditEvent{Action: "after-close"})
}

// 审计协程随 Open 启动，不等第一条事件：启动清理在没有任何事件时也要跑。
func TestAuditPurgeOnStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-200 * 24 * time.Hour).Unix()
	if _, err := st.db.Exec(
		`INSERT INTO audit(ts,ip,user,action,target,detail,ok) VALUES(?,?,?,?,?,?,?)`,
		old, "127.0.0.1", "admin", "login", "", "", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	// 启动清理在审计协程里跑，给它一点时间；超过 3s 就是没跑。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		list, err := st2.ListAudit(10)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("启动后 3s 内过期审计记录仍未清理")
}
