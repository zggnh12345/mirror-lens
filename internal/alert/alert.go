// Package alert 保存最近的 error 级日志，供面板铃铛显示。
//
// 只留内存：日志本来就是过程性的，重启后旧的错误没有展示价值，
// 也没必要为它写库。容量固定，最坏 200 条 × 一条消息。
package alert

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Entry 是一条告警。
type Entry struct {
	ID    int64  `json:"id"`
	At    int64  `json:"at"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

// Ring 是固定容量的告警环。
type Ring struct {
	mu   sync.Mutex
	next int64
	seen int64
	buf  []Entry
	max  int
}

// New 建一个容量 max 的环；max<=0 取 200。
func New(max int) *Ring {
	if max <= 0 {
		max = 200
	}
	return &Ring{max: max}
}

// Add 记录一条。超过容量时丢最旧的。
func (r *Ring) Add(level slog.Level, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	r.buf = append(r.buf, Entry{ID: r.next, At: time.Now().Unix(), Level: level.String(), Msg: msg})
	if len(r.buf) > r.max {
		r.buf = r.buf[len(r.buf)-r.max:]
	}
}

// Snapshot 返回最近 limit 条（新的在前）和未读数。
func (r *Ring) Snapshot(limit int) ([]Entry, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit <= 0 || limit > len(r.buf) {
		limit = len(r.buf)
	}
	out := make([]Entry, 0, limit)
	unread := 0
	for _, e := range r.buf {
		if e.ID > r.seen {
			unread++
		}
	}
	for i := len(r.buf) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, r.buf[i])
	}
	return out, unread
}

// Clear 清空全部条目，红点归零。
func (r *Ring) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = nil
	r.seen = r.next
}

// MarkSeen 把全部现有条目标为已读。
func (r *Ring) MarkSeen() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = r.next
}

// Handler 在写日志的同时把 error 级别以上的记录进环。
// 用 Embed 保持原有 Handler 的 Attr/Group 语义。
type Handler struct {
	slog.Handler
	ring *Ring
}

// Wrap 给已有 Handler 套一层采集。
func Wrap(h slog.Handler, r *Ring) slog.Handler {
	if r == nil {
		return h
	}
	return &Handler{Handler: h, ring: r}
}

func (h *Handler) Handle(ctx context.Context, rec slog.Record) error {
	if rec.Level >= slog.LevelError {
		h.ring.Add(rec.Level, rec.Message)
	}
	return h.Handler.Handle(ctx, rec)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{Handler: h.Handler.WithAttrs(attrs), ring: h.ring}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{Handler: h.Handler.WithGroup(name), ring: h.ring}
}
