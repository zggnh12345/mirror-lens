package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// runStep 的业务写入与标记必须同生共死。
func TestRunStepAtomic(t *testing.T) {
	st := openTestAt(t, filepath.Join(t.TempDir(), "step.db"))
	defer st.Close()
	ctx := context.Background()

	var calls int
	ran, err := st.runStep(ctx, "t-step", func(ctx context.Context, tx dbtx) error {
		calls++
		_, err := tx.ExecContext(ctx, `INSERT INTO meta(k, v) VALUES('k1', 'v1')`)
		return err
	})
	if err != nil || !ran {
		t.Fatalf("首次执行: ran=%v err=%v", ran, err)
	}
	if calls != 1 {
		t.Fatalf("调用次数 %d", calls)
	}

	ran, err = st.runStep(ctx, "t-step", func(ctx context.Context, tx dbtx) error {
		calls++
		return nil
	})
	if err != nil || ran {
		t.Fatalf("重复执行应跳过: ran=%v err=%v", ran, err)
	}
	if calls != 1 {
		t.Fatalf("已登记步骤被重复执行，调用次数 %d", calls)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM meta WHERE k='k1'`); n != 1 {
		t.Fatalf("业务写入丢失: %d", n)
	}
}

func TestRunStepRollback(t *testing.T) {
	st := openTestAt(t, filepath.Join(t.TempDir(), "step.db"))
	defer st.Close()
	ctx := context.Background()

	_, err := st.runStep(ctx, "t-fail", func(ctx context.Context, tx dbtx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta(k, v) VALUES('k2', 'v2')`); err != nil {
			return err
		}
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("预期失败")
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM meta WHERE k='k2'`); n != 0 {
		t.Fatalf("失败步骤的写入未回滚: %d", n)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM schema_migrations WHERE name='t-fail'`); n != 0 {
		t.Fatalf("失败步骤不应留下标记: %d", n)
	}

	// 失败后可以重试成功。
	ran, err := st.runStep(ctx, "t-fail", func(ctx context.Context, tx dbtx) error { return nil })
	if err != nil || !ran {
		t.Fatalf("重试: ran=%v err=%v", ran, err)
	}
}

// COMMIT 阶段失败（延迟外键在提交时才校验）：必须回滚干净、不留标记，
// 且连接要还回池里，后续步骤仍能正常读写。
func TestRunStepCommitFailureRollsBack(t *testing.T) {
	st := openTestAt(t, filepath.Join(t.TempDir(), "commit.db"))
	defer st.Close()
	ctx := context.Background()

	// DEFERRABLE INITIALLY DEFERRED：插入时不校验，COMMIT 时才报错。
	for _, q := range []string{
		`CREATE TABLE cfp(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE cfc(id INTEGER PRIMARY KEY, pid INTEGER REFERENCES cfp(id) DEFERRABLE INITIALLY DEFERRED)`,
	} {
		if _, err := st.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}

	ran, err := st.runStep(ctx, "t-commit-fail", func(ctx context.Context, tx dbtx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO cfc(id, pid) VALUES(1, 999)`)
		return err
	})
	if err == nil {
		t.Fatal("COMMIT 应因外键失败")
	}
	if ran {
		t.Fatal("失败的步骤不应返回 ran=true")
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM cfc`); n != 0 {
		t.Fatalf("半提交残留: %d", n)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM schema_migrations WHERE name='t-commit-fail'`); n != 0 {
		t.Fatalf("失败步骤留下标记: %d", n)
	}

	// 连接池仍可用：写走同一个 *sql.Conn，坏连接不该把后续步骤卡死。
	ran, err = st.runStep(ctx, "t-after", func(ctx context.Context, tx dbtx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO meta(k, v) VALUES('k3', 'v3')`)
		return err
	})
	if err != nil || !ran {
		t.Fatalf("提交失败后连接不可用: ran=%v err=%v", ran, err)
	}
}
