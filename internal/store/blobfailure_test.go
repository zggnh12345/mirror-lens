package store

import (
	"testing"
	"time"
)

// 失败事件只描述一次获取，不改内容身份：已经 verified 的 blob 不能被一次
// 摘要不符的下载拖成 mismatch。
func TestRecordBlobFailureDoesNotTouchBlobs(t *testing.T) {
	s := openBlobTest(t)
	if err := s.BlobUpsert(Blob{SHA256: "aaa", Size: 10, IntegrityStatus: BlobVerified, DigestSource: DigestSourceDocker}); err != nil {
		t.Fatal(err)
	}

	err := s.RecordBlobFailure(BlobFailure{
		ExpectedDigest: "bbb", ActualDigest: "aaa", DigestSource: DigestSourceGitHub,
		Protocol: "release", SourceID: 7, SourceName: "src-a",
		CacheKey: "gh:o/r/v1/a.tgz", ErrorCode: "digest_mismatch",
	})
	if err != nil {
		t.Fatal(err)
	}

	b, err := s.BlobGet("aaa")
	if err != nil {
		t.Fatal(err)
	}
	if b.IntegrityStatus != BlobVerified {
		t.Fatalf("失败事件污染了内容身份: %+v", b)
	}
	if n, _ := s.BlobFailureCount(0); n != 1 {
		t.Fatalf("失败事件应记 1 条，实际 %d", n)
	}
}

// 事件按 created_at 过滤，窗口外的旧事件不计数。
func TestBlobFailureCountWindow(t *testing.T) {
	s := openBlobTest(t)
	now := time.Now().Unix()
	for _, ts := range []int64{now - 10*86400, now - 3600} {
		if err := s.RecordBlobFailure(BlobFailure{
			ExpectedDigest: "e", ActualDigest: "a", DigestSource: "github_api",
			Protocol: "release", ErrorCode: "digest_mismatch", CreatedAt: ts,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := s.BlobFailureCount(now - 86400); n != 1 {
		t.Fatalf("24h 内应 1 条，实际 %d", n)
	}
	if n, _ := s.BlobFailureCount(0); n != 2 {
		t.Fatalf("全部应 2 条，实际 %d", n)
	}
}
