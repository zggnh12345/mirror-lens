package alert

import (
	"log/slog"
	"testing"
)

func TestRingCapAndUnread(t *testing.T) {
	r := New(3)
	r.Add(slog.LevelError, "a")
	r.Add(slog.LevelError, "b")
	if _, unread := r.Snapshot(10); unread != 2 {
		t.Fatalf("未读数应为 2，得到 %d", unread)
	}
	r.Add(slog.LevelError, "c")
	r.Add(slog.LevelError, "d") // 挤掉 a
	items, unread := r.Snapshot(10)
	// 被挤掉的条目不再计数：点开铃铛要能看到每一条未读。
	if len(items) != 3 || unread != 3 {
		t.Fatalf("容量应为 3、未读 3，得到 %d/%d", len(items), unread)
	}
	if items[0].Msg != "d" {
		t.Fatalf("应新条目在前，得到 %q", items[0].Msg)
	}
	r.MarkSeen()
	if _, unread := r.Snapshot(10); unread != 0 {
		t.Fatalf("标记已读后应为 0，得到 %d", unread)
	}
}

func TestHandlerOnlyCapturesError(t *testing.T) {
	r := New(10)
	h := Wrap(slog.NewTextHandler(discard{}, &slog.HandlerOptions{Level: slog.LevelInfo}), r)
	l := slog.New(h)
	l.Warn("warn 不进")
	l.Error("error 进")
	items, _ := r.Snapshot(10)
	if len(items) != 1 || items[0].Msg != "error 进" {
		t.Fatalf("只应收 error 级，得到 %+v", items)
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
