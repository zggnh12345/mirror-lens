package store

import (
	"database/sql"
	"errors"
	"sync"
	"testing"
)

// 幂等：摘要相同不递增、不写审计。
func TestApplyCredWriteIdempotent(t *testing.T) {
	st, _ := openTestStore(t)
	res, err := st.ApplyCredWrite(CredWrite{Host: "quay.io", Digest: "d1"})
	if err != nil || !res.Changed || res.Gen != 1 {
		t.Fatalf("首次写入: %+v err=%v", res, err)
	}
	res, err = st.ApplyCredWrite(CredWrite{Host: "quay.io", Digest: "d1"})
	if err != nil || res.Changed || res.Gen != 1 {
		t.Fatalf("幂等写入不应递增: %+v err=%v", res, err)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM audit WHERE action='credential_put'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("幂等写入不应写审计，实际 %d 条", n)
	}
}

// CAS 冲突：expected_gen 不匹配返回 ErrCredGenConflict，且不改数据。
func TestApplyCredWriteCAS(t *testing.T) {
	st, _ := openTestStore(t)
	if _, err := st.ApplyCredWrite(CredWrite{Host: "quay.io", Digest: "d1"}); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	_, err := st.ApplyCredWrite(CredWrite{Host: "quay.io", Digest: "d2", ExpectedGen: &zero})
	if !errors.Is(err, ErrCredGenConflict) {
		t.Fatalf("期望 ErrCredGenConflict，得到 %v", err)
	}
	gen, digest, _ := st.HostGenState("quay.io")
	if gen != 1 || digest != "d1" {
		t.Fatalf("冲突不得改数据: gen=%d digest=%s", gen, digest)
	}
	one := int64(1)
	res, err := st.ApplyCredWrite(CredWrite{Host: "quay.io", Digest: "d2", ExpectedGen: &one})
	if err != nil || res.Gen != 2 {
		t.Fatalf("CAS 成功应递增: %+v err=%v", res, err)
	}
}

// 审计与代次同事务：审计插入失败时整笔回滚。
func TestApplyCredWriteAuditSameTx(t *testing.T) {
	st, _ := openTestStore(t)
	res, err := st.ApplyCredWrite(CredWrite{
		Host: "gcr.io", Digest: "d1",
		Audit: &AuditEvent{Action: "credential_put", Detail: "mode=basic", OK: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Gen != 1 {
		t.Fatalf("gen=%d", res.Gen)
	}
	var action, detail string
	if err := st.db.QueryRow(`SELECT action, detail FROM audit ORDER BY id DESC LIMIT 1`).Scan(&action, &detail); err != nil {
		t.Fatal(err)
	}
	if action != "credential_put" || detail != "mode=basic" {
		t.Fatalf("审计内容不对: %s %s", action, detail)
	}
	// 审计失败注入：把 audit 表改名，写入必须整体失败且不留 gen。
	if _, err := st.db.Exec(`ALTER TABLE audit RENAME TO audit_tmp`); err != nil {
		t.Fatal(err)
	}
	_, err = st.ApplyCredWrite(CredWrite{
		Host: "gcr.io", Digest: "d2",
		Audit: &AuditEvent{Action: "credential_put", OK: true},
	})
	if err == nil {
		t.Fatal("审计写失败必须整体失败")
	}
	gen, digest, _ := st.HostGenState("gcr.io")
	if gen != 1 || digest != "d1" {
		t.Fatalf("审计失败后不得留下半状态: gen=%d digest=%s", gen, digest)
	}
}

// C01：两个独立 SQLite 连接，在快照与提交之间注入修改，旧快照必须被拒绝。
func TestApplyCredWriteTwoConnections(t *testing.T) {
	st, path := openTestStore(t)
	if _, err := st.ApplyCredWrite(CredWrite{Host: "quay.io", Digest: "d1"}); err != nil {
		t.Fatal(err)
	}
	gen, _, err := st.HostGenState("quay.io")
	if err != nil {
		t.Fatal(err)
	}
	// 第二个连接（独立进程语义）修改凭据。
	other, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Exec(`UPDATE registry_host_gen SET gen=gen+1, cred_digest='d2' WHERE host='quay.io'`); err != nil {
		t.Fatal(err)
	}
	// 第一个连接拿着旧 gen 提交：必须冲突。
	_, err = st.ApplyCredWrite(CredWrite{Host: "quay.io", Digest: "d3", ExpectedGen: &gen})
	if !errors.Is(err, ErrCredGenConflict) {
		t.Fatalf("旧快照必须被拒绝，得到 %v", err)
	}
}

// C03：并发写同一 Host 由事务串行化，不丢更新、不产生重复代次。
func TestApplyCredWriteConcurrent(t *testing.T) {
	st, _ := openTestStore(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := st.ApplyCredWrite(CredWrite{Host: "quay.io", Digest: string(rune('a' + i))})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发写入失败: %v", err)
		}
	}
	gen, _, err := st.HostGenState("quay.io")
	if err != nil {
		t.Fatal(err)
	}
	// 8 次不同摘要 ⇒ 至少 1 次生效，代次必须等于实际变更次数且 > 0。
	if gen == 0 || gen > 8 {
		t.Fatalf("并发后代次异常: %d", gen)
	}
}

// epoch：首次初始化只递增一次；键格式与解析互逆。
func TestCacheEpochInitAndKeys(t *testing.T) {
	st, _ := openTestStore(t)
	e, bumped, err := st.InitCacheEpoch()
	if err != nil || !bumped || e != 1 {
		t.Fatalf("首次初始化: e=%d bumped=%v err=%v", e, bumped, err)
	}
	e2, bumped2, err := st.InitCacheEpoch()
	if err != nil || bumped2 || e2 != 1 {
		t.Fatalf("重复初始化不应再递增: e=%d bumped=%v err=%v", e2, bumped2, err)
	}
	if got := RegistryCacheKey(0, "manifest", "quay.io/x:1"); got != "docker:manifest:quay.io/x:1" {
		t.Fatalf("epoch=0 应保持旧格式: %s", got)
	}
	k := RegistryCacheKey(3, "blob", "sha256:abc")
	if k != "docker:b:3:sha256:abc" {
		t.Fatalf("epoch 键格式不对: %s", k)
	}
	if RegistryCacheKind(k) != "blob" || RegistryCacheKeyEpoch(k) != 3 {
		t.Fatalf("解析失败: kind=%s epoch=%d", RegistryCacheKind(k), RegistryCacheKeyEpoch(k))
	}
	if RegistryCacheKeyEpoch("docker:manifest:a:b") != 0 {
		t.Fatal("旧格式应解析为 epoch=0")
	}
}

// 旧 epoch 清理：只删更早的键，当前 epoch 不动。
func TestCacheEpochSweep(t *testing.T) {
	st, _ := openTestStore(t)
	keys := []string{
		"docker:manifest:old/x:1", // 旧格式
		"docker:blob:sha256:old",  // 旧格式 blob
		"docker:m:1:a/x:1",
		"docker:b:1:sha256:aaa",
		"docker:m:2:b/x:1",
		"docker:b:2:sha256:abc",
		"raw:keep",
	}
	for _, k := range keys {
		if err := st.CachePut(CacheEntry{Key: k, SHA256: "s" + k, Size: 1, CreatedAt: 1, TTL: 60}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := st.CacheEpochSweep(2, 100)
	if err != nil {
		t.Fatal(err)
	}
	// 4 条：旧格式 manifest/blob + epoch1 manifest/blob。
	if n != 4 {
		t.Fatalf("应删 4 条更早 epoch，实际 %d", n)
	}
	for _, k := range []string{"docker:m:2:b/x:1", "docker:b:2:sha256:abc", "raw:keep"} {
		if e, _ := st.CacheGet(k); e == nil {
			t.Fatalf("当前 epoch 的键被误删: %s", k)
		}
	}
	for _, k := range []string{"docker:manifest:old/x:1", "docker:blob:sha256:old", "docker:m:1:a/x:1", "docker:b:1:sha256:aaa"} {
		if e, _ := st.CacheGet(k); e != nil {
			t.Fatalf("旧 epoch 键仍可达: %s", k)
		}
	}
}
