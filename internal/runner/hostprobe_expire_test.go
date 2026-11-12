package runner

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
	"time"
)

type recordingExpirer struct {
	now []time.Time
	n   int
	err error
}

func (f *recordingExpirer) MaterializeExpired(now time.Time) (int, error) {
	f.now = append(f.now, now)
	return f.n, f.err
}

// 物化必须发生在扫描之前，且两者收到同一个 now。
func TestOnceMaterializesBeforeScanWithSameNow(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	f := &recordingExpirer{n: 2}
	var scanNow time.Time
	scanned := false

	r := NewHostProbeRunner(HostProbeOptions{
		Now:     func() time.Time { return base },
		Expirer: f,
		JobsAt: func(_ context.Context, now time.Time) ([]HostProbeJob, error) {
			scanned = true
			scanNow = now
			return nil, nil
		},
	})

	r.Once(context.Background())

	if len(f.now) != 1 {
		t.Fatalf("物化应只调用一次，实际 %d 次", len(f.now))
	}
	if !f.now[0].Equal(base) {
		t.Fatalf("物化收到 now=%v，期望 %v", f.now[0], base)
	}
	if !scanned {
		t.Fatal("扫描未执行")
	}
	if !scanNow.Equal(f.now[0]) {
		t.Fatalf("物化与扫描的 now 不一致：%v vs %v", f.now[0], scanNow)
	}
}

// 物化失败必须记录错误，并继续本轮扫描（不能返回或跳过）。
func TestOnceContinuesWhenMaterializeFails(t *testing.T) {
	f := &recordingExpirer{err: errors.New("database is locked")}
	scanned := false

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	r := NewHostProbeRunner(HostProbeOptions{
		Now:     func() time.Time { return time.Unix(1_700_000_000, 0) },
		Expirer: f,
		JobsAt: func(_ context.Context, _ time.Time) ([]HostProbeJob, error) {
			scanned = true
			return nil, nil
		},
	})

	r.Once(context.Background())

	if !scanned {
		t.Fatal("物化失败后仍应调用 ListDueHostProbes / 扫描入口")
	}
	if !strings.Contains(buf.String(), "物化失败") {
		t.Fatalf("应记录物化失败日志，实际日志: %q", buf.String())
	}
}
