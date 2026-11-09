package store

import (
	"path/filepath"
	"testing"
)

func openBlobTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// 旧库 verify 的映射必须是显式的：历史 verified 只能留痕，不能继续算可信。
func TestMigrateBlobsLegacyVerifyMapping(t *testing.T) {
	s := openBlobTest(t)
	// 造三条历史数据，模拟迁移前状态。
	if _, err := s.db.Exec(`DELETE FROM meta WHERE k='blobs_migrated_v1'`); err != nil {
		t.Fatal(err)
	}
	rows := []struct{ key, sha, verify string }{
		{"raw:a", "aaa", ""},
		{"raw:b", "bbb", "verified"},
		{"raw:c", "ccc", "weird-old-value"},
	}
	for _, r := range rows {
		if _, err := s.db.Exec(`INSERT INTO cache_entries(key, sha256, size, created_at, verify)
			VALUES(?,?,100,1,?)`, r.key, r.sha, r.verify); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.migrateBlobs(); err != nil {
		t.Fatalf("迁移: %v", err)
	}

	for _, sha := range []string{"aaa", "bbb", "ccc"} {
		b, err := s.BlobGet(sha)
		if err != nil || b == nil {
			t.Fatalf("blob %s 未回填: %v", sha, err)
		}
		if b.IntegrityStatus != BlobUnknown {
			t.Fatalf("历史数据不能算已验证: %s=%s", sha, b.IntegrityStatus)
		}
	}
	legacy, _ := s.BlobGet("bbb")
	if legacy.DigestSource != DigestSourceLegacy || legacy.VerificationError != "legacy_verified_no_digest" {
		t.Fatalf("历史 verified 应留痕: %+v", legacy)
	}

	for key, want := range map[string]string{"raw:a": "unverified", "raw:b": "verified", "raw:c": "unverified"} {
		e, err := s.CacheGet(key)
		if err != nil || e == nil {
			t.Fatalf("%s 读取失败: %v", key, err)
		}
		if e.Verify != want {
			t.Fatalf("%s verify 映射错误: got %q want %q", key, e.Verify, want)
		}
	}

	// 幂等：再跑一次不应改变结果。
	if err := s.migrateBlobs(); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.BlobGet("bbb"); again.DigestSource != DigestSourceLegacy {
		t.Fatalf("重复迁移改动了历史标记: %+v", again)
	}
}

// 状态只能升不能降，mismatch 是终态。
func TestBlobUpsertPrecedence(t *testing.T) {
	s := openBlobTest(t)
	if err := s.BlobUpsert(Blob{SHA256: "x", Size: 10, IntegrityStatus: BlobUnknown}); err != nil {
		t.Fatal(err)
	}
	if err := s.BlobUpsert(Blob{SHA256: "x", Size: 10, IntegrityStatus: BlobVerified,
		ExpectedDigest: "x", DigestSource: DigestSourceDocker, VerifiedAt: 99}); err != nil {
		t.Fatal(err)
	}
	b, _ := s.BlobGet("x")
	if b.IntegrityStatus != BlobVerified || b.DigestSource != DigestSourceDocker {
		t.Fatalf("应升级为 verified: %+v", b)
	}

	// 普通内容写入不能把 verified 洗回 unknown。
	if err := s.BlobUpsert(Blob{SHA256: "x", Size: 10, IntegrityStatus: BlobUnknown}); err != nil {
		t.Fatal(err)
	}
	if b, _ = s.BlobGet("x"); b.IntegrityStatus != BlobVerified {
		t.Fatalf("verified 被降级: %+v", b)
	}

	if err := s.BlobUpsert(Blob{SHA256: "x", IntegrityStatus: BlobMismatch,
		ExpectedDigest: "expected", ActualDigest: "actual", DigestSource: DigestSourceGitHub}); err != nil {
		t.Fatal(err)
	}
	if b, _ = s.BlobGet("x"); b.IntegrityStatus != BlobMismatch {
		t.Fatalf("应记 mismatch: %+v", b)
	}
	// 后续任何写入都不能洗白 mismatch。
	if err := s.BlobUpsert(Blob{SHA256: "x", IntegrityStatus: BlobVerified, DigestSource: DigestSourceDocker}); err != nil {
		t.Fatal(err)
	}
	if b, _ = s.BlobGet("x"); b.IntegrityStatus != BlobMismatch {
		t.Fatalf("mismatch 被洗白: %+v", b)
	}
}

// 同一内容被多个 URL 引用时，计数按 blob 去重。
func TestCacheVerifyCountsByBlob(t *testing.T) {
	s := openBlobTest(t)
	for _, key := range []string{"raw:a", "gh:a", "http:a"} {
		if err := s.CachePut(CacheEntry{Key: key, SHA256: "same", Size: 5, CreatedAt: 1, TTL: 300, Verify: VerifyUnverified}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.CacheVerifyCounts()
	if err != nil {
		t.Fatal(err)
	}
	if got[BlobUnknown] != 1 || got[BlobVerified] != 0 {
		t.Fatalf("应按 blob 计数: %+v", got)
	}
	if n, _ := s.CacheCountSHA("same"); n != 3 {
		t.Fatalf("条目数应仍为 3: %d", n)
	}
}
