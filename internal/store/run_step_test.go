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
