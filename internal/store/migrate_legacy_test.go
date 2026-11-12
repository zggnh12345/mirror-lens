package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func seedLegacyRow(t *testing.T, s *Store, id int64, fails, okAt, lastAt int64) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO source_health(source_id,family,fails,ok_at,last_at)
		VALUES(?,?,?,?,?)`, id, "registry", fails, okAt, lastAt); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
}

func addLegacySource(t *testing.T, s *Store, name, repoHost, status string) int64 {
	t.Helper()
	res, err := s.db.Exec(`INSERT INTO sources(base_url,name,status,repo_host,created_at)
		VALUES(?,?,?,?,1)`, "https://"+name+".example", name, status, repoHost)
	if err != nil {
		t.Fatalf("add source: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func countRows(t *testing.T, s *Store, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// 覆盖口径：usable / archived / 空 repo_host / ghcr.io 四种源的 legacy 行都要归档，
// 且不按 registry_cap 过滤；其他 family 的行必须原地保留。
func TestArchiveLegacyRegistryCoversAllSources(t *testing.T) {
	st := openTestAt(t, filepath.Join(t.TempDir(), "a.db"))
	defer st.Close()

	usable := addLegacySource(t, st, "usable", "ghcr.example", "usable")
	archived := addLegacySource(t, st, "archived", "ghcr.example", "archived")
	emptyHost := addLegacySource(t, st, "empty", "", "usable")
	hub := addLegacySource(t, st, "hub", "ghcr.io", "usable")

	seedLegacyRow(t, st, usable, 2, 100, 200)
	seedLegacyRow(t, st, archived, 1, 0, 50)
	seedLegacyRow(t, st, emptyHost, 0, 300, 300)
	seedLegacyRow(t, st, hub, 5, 0, 10)
	if _, err := st.db.Exec(`INSERT INTO source_health(source_id,family,fails,ok_at,last_at)
		VALUES(?,?,?,?,?)`, usable, "docker", 9, 1, 2); err != nil {
		t.Fatal(err)
	}
	// registry_cap=0 不能成为跳过归档的理由。
	if _, err := st.db.Exec(`UPDATE sources SET registry_cap=0 WHERE id IN (?,?)`, usable, hub); err != nil {
		t.Fatal(err)
	}

	out, err := st.ArchiveLegacyRegistry(context.Background())
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if out.Archived != 4 || out.Removed != 4 {
		t.Fatalf("归档计数 = %d/%d, want 4/4", out.Archived, out.Removed)
	}
	if out.Candidates != 4 || out.Left != 0 {
		t.Fatalf("候选/剩余计数 = %d/%d, want 4/0", out.Candidates, out.Left)
	}
	if out.AlreadyDone {
		t.Fatal("首次迁移不应标记 already_done")
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM source_health WHERE family='registry'`); n != 0 {
		t.Fatalf("legacy 残留 %d 行", n)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM source_health WHERE family='docker'`); n != 1 {
		t.Fatalf("其他 family 被误伤: %d", n)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM source_health_archive WHERE batch=?`, legacyArchiveBatch); n != 4 {
		t.Fatalf("归档表行数 %d", n)
	}
	// 逐字段核对一条，防止 INSERT...SELECT 列错位。
	var fails, okAt, lastAt int64
	if err := st.db.QueryRow(`SELECT fails,ok_at,last_at FROM source_health_archive
		WHERE source_id=? AND batch=?`, usable, legacyArchiveBatch).Scan(&fails, &okAt, &lastAt); err != nil {
		t.Fatal(err)
	}
	if fails != 2 || okAt != 100 || lastAt != 200 {
		t.Fatalf("归档字段错位: %d/%d/%d", fails, okAt, lastAt)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM schema_migrations WHERE name=?`, legacyArchiveMarker); n != 1 {
		t.Fatal("缺迁移标记")
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM sources`); n != 4 {
		t.Fatalf("sources 被改动: %d", n)
	}
}

// 同进程重跑与重新 Open 都必须幂等。
func TestArchiveLegacyRegistryIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.db")
	st := openTestAt(t, path)
	id := addLegacySource(t, st, "s", "ghcr.example", "usable")
	seedLegacyRow(t, st, id, 3, 1, 2)

	if _, err := st.ArchiveLegacyRegistry(context.Background()); err != nil {
		t.Fatal(err)
	}
	out, err := st.ArchiveLegacyRegistry(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !out.AlreadyDone || out.Archived != 0 || out.Removed != 0 {
		t.Fatalf("第二次执行不是空操作: %+v", out)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM source_health_archive`); n != 1 {
		t.Fatalf("重复归档: %d", n)
	}
	st.Close()

	// 重新 Open 走启动迁移路径，仍不得重复写入。
	st2 := openTestAt(t, path)
	defer st2.Close()
	if n := countRows(t, st2, `SELECT COUNT(*) FROM source_health_archive`); n != 1 {
		t.Fatalf("启动迁移重复归档: %d", n)
	}
}

// 启动路径：老库已有 legacy 行，Open 后必须自动归档。
func TestOpenArchivesLegacyRegistry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.db")
	st := openTestAt(t, path)
	id := addLegacySource(t, st, "s", "ghcr.example", "usable")
	seedLegacyRow(t, st, id, 1, 10, 20)
	st.Close()

	st2 := openTestAt(t, path)
	defer st2.Close()
	if n := countRows(t, st2, `SELECT COUNT(*) FROM source_health WHERE family='registry'`); n != 0 {
		t.Fatalf("启动未清理: %d", n)
	}
	if n := countRows(t, st2, `SELECT COUNT(*) FROM source_health_archive`); n != 1 {
		t.Fatalf("启动未归档: %d", n)
	}
}

// 删除失败时归档必须整体回滚：归档表为空、运行表原样、无标记。
func TestArchiveLegacyRegistryRollsBack(t *testing.T) {
	st := openTestAt(t, filepath.Join(t.TempDir(), "d.db"))
	defer st.Close()
	id := addLegacySource(t, st, "s", "ghcr.example", "usable")
	seedLegacyRow(t, st, id, 7, 1, 2)
	// 清掉 Open 时写入的空批次标记，模拟「首次迁移就失败」。
	if _, err := st.db.Exec(`DELETE FROM schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`CREATE TRIGGER boom BEFORE DELETE ON source_health
		WHEN old.family='registry' BEGIN SELECT RAISE(ABORT,'boom'); END`); err != nil {
		t.Fatal(err)
	}

	if _, err := st.ArchiveLegacyRegistry(context.Background()); err == nil {
		t.Fatal("删除被触发器拒绝时应返回错误")
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM source_health_archive`); n != 0 {
		t.Fatalf("归档未回滚: %d", n)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM source_health WHERE family='registry'`); n != 1 {
		t.Fatalf("运行表被破坏: %d", n)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM schema_migrations`); n != 0 {
		t.Fatalf("失败后写了标记: %d", n)
	}
}

// 回滚：归档行补回运行表，字段逐项相等；再次回滚全部按冲突跳过。
func TestUnarchiveLegacyRegistryRoundTrip(t *testing.T) {
	st := openTestAt(t, filepath.Join(t.TempDir(), "e.db"))
	defer st.Close()
	id := addLegacySource(t, st, "s", "ghcr.example", "usable")
	seedLegacyRow(t, st, id, 4, 11, 22)
	if _, err := st.ArchiveLegacyRegistry(context.Background()); err != nil {
		t.Fatal(err)
	}

	out, err := st.UnarchiveLegacyRegistry(context.Background(), legacyArchiveBatch)
	if err != nil {
		t.Fatal(err)
	}
	if out.Restored != 1 || out.Skipped != 0 {
		t.Fatalf("回滚计数 %+v", out)
	}
	var fails, okAt, lastAt int64
	if err := st.db.QueryRow(`SELECT fails,ok_at,last_at FROM source_health
		WHERE source_id=? AND family='registry'`, id).Scan(&fails, &okAt, &lastAt); err != nil {
		t.Fatalf("回滚后缺行: %v", err)
	}
	if fails != 4 || okAt != 11 || lastAt != 22 {
		t.Fatalf("回滚字段不等: %d/%d/%d", fails, okAt, lastAt)
	}
	again, err := st.UnarchiveLegacyRegistry(context.Background(), legacyArchiveBatch)
	if err != nil {
		t.Fatal(err)
	}
	if again.Restored != 0 || again.Skipped != 1 {
		t.Fatalf("冲突策略失效: %+v", again)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM source_health_archive`); n != 1 {
		t.Fatal("回滚不该删除归档行")
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM schema_migrations WHERE name='`+legacyArchiveMarker+`'`); n != 0 {
		t.Fatalf("回滚后迁移标记应被撤销，实际 %d 条", n)
	}

	// 撤销标记后重跑迁移：数据应再次归档并清理，计数不能把归档表旧行算成新归档。
	res, err := st.ArchiveLegacyRegistry(context.Background())
	if err != nil {
		t.Fatalf("重跑迁移: %v", err)
	}
	if res.Candidates != 1 || res.Archived != 0 || res.Removed != 1 || res.Left != 0 {
		t.Fatalf("重跑计数 %+v", res)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM source_health WHERE family='registry'`); n != 0 {
		t.Fatalf("重跑后仍有 %d 行", n)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM schema_migrations WHERE name='`+legacyArchiveMarker+`'`); n != 1 {
		t.Fatalf("重跑后标记应恢复，实际 %d 条", n)
	}
}

// 冲突：目标行已存在时跳过、不覆盖现行数据。
func TestUnarchiveLegacyRegistrySkipsConflict(t *testing.T) {
	st := openTestAt(t, filepath.Join(t.TempDir(), "f.db"))
	defer st.Close()
	id := addLegacySource(t, st, "s", "ghcr.example", "usable")
	seedLegacyRow(t, st, id, 8, 1, 2)
	if _, err := st.ArchiveLegacyRegistry(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO source_health(source_id,family,fails,ok_at,last_at)
		VALUES(?,?,?,?,?)`, id, "registry", 0, 99, 99); err != nil {
		t.Fatal(err)
	}

	out, err := st.UnarchiveLegacyRegistry(context.Background(), legacyArchiveBatch)
	if err != nil {
		t.Fatal(err)
	}
	if out.Restored != 0 || out.Skipped != 1 {
		t.Fatalf("冲突计数 %+v", out)
	}
	var fails, okAt int64
	if err := st.db.QueryRow(`SELECT fails,ok_at FROM source_health
		WHERE source_id=? AND family='registry'`, id).Scan(&fails, &okAt); err != nil {
		t.Fatal(err)
	}
	if fails != 0 || okAt != 99 {
		t.Fatalf("现行数据被覆盖: %d/%d", fails, okAt)
	}
}

// 事务必须以 BEGIN IMMEDIATE 开始：持锁期间另一个连接拿不到写锁，
// 释放后立刻可以拿到。DEFERRED 在首次写之前不会持有 RESERVED 锁，
// 这个用例会直接失败。
func TestBeginImmediateTakesWriteLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock.db")
	st := openTestAt(t, path)
	defer st.Close()

	_, done, err := st.beginImmediate(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	released := false
	defer func() {
		if !released {
			_ = done(false)
		}
	}()

	ext, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer ext.Close()

	if _, err := ext.Exec("BEGIN IMMEDIATE"); err == nil {
		_, _ = ext.Exec("ROLLBACK")
		t.Fatal("写事务持锁期间，第二个连接不应拿到写锁")
	}

	if err := done(false); err != nil {
		t.Fatalf("release: %v", err)
	}
	released = true

	if _, err := ext.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("释放后仍拿不到写锁: %v", err)
	}
	_, _ = ext.Exec("ROLLBACK")
}
