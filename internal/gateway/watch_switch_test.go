package gateway

import (
	"net/http"
	"testing"
	"time"
)

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

// 换源边界：起点、总长、长度、验证器缺一不可。
func TestSwitchAcceptable(t *testing.T) {
	const (
		sent  = int64(100)
		total = int64(1000)
	)
	cases := []struct {
		name     string
		cur      http.Header
		newH     http.Header
		status   int
		clen     int64
		expect   string
		wantOK   bool
		verified bool
	}{
		{
			name: "206 全对", cur: hdr("ETag", `"v1"`), newH: hdr("ETag", `"v1"`, "Content-Range", "bytes 100-999/1000"),
			status: 206, clen: 900, wantOK: true, verified: true,
		},
		{
			name: "206 起点错", cur: hdr("ETag", `"v1"`), newH: hdr("ETag", `"v1"`, "Content-Range", "bytes 99-999/1000"),
			status: 206, clen: 901,
		},
		{
			name: "206 总长错", cur: hdr("ETag", `"v1"`), newH: hdr("ETag", `"v1"`, "Content-Range", "bytes 100-999/1001"),
			status: 206, clen: 900,
		},
		{
			name: "206 体长不符", cur: hdr("ETag", `"v1"`), newH: hdr("ETag", `"v1"`, "Content-Range", "bytes 100-999/1000"),
			status: 206, clen: 800,
		},
		{
			name: "206 ETag 不同", cur: hdr("ETag", `"v1"`), newH: hdr("ETag", `"v2"`, "Content-Range", "bytes 100-999/1000"),
			status: 206, clen: 900,
		},
		{
			name: "206 只有一方有验证器", cur: hdr("ETag", `"v1"`), newH: hdr("Content-Range", "bytes 100-999/1000"),
			status: 206, clen: 900,
		},
		{
			name: "206 无验证器但有摘要", cur: hdr(), newH: hdr("Content-Range", "bytes 100-999/1000"),
			status: 206, clen: 900, expect: "abc", wantOK: true,
		},
		{
			// 一方带验证器、另一方没有：即使有摘要也不能换，拿不到同版本的证据。
			name: "206 只有一方有验证器但有摘要", cur: hdr("ETag", `"v1"`), newH: hdr("Content-Range", "bytes 100-999/1000"),
			status: 206, clen: 900, expect: "abc",
		},
		{
			// 类型不同不能互相比较：旧 ETag 对新 Last-Modified。
			name: "206 验证器类型不同", cur: hdr("ETag", `"v1"`), newH: hdr("Last-Modified", "Wed, 01 Jan 2026 00:00:00 GMT", "Content-Range", "bytes 100-999/1000"),
			status: 206, clen: 900, expect: "abc",
		},
		{
			// Last-Modified 只有秒级精度：两个相同时间戳不能当字节级证据，
			// 无强 ETag 时按无可靠验证器处理。
			name: "206 Last-Modified 一致也不放行", cur: hdr("Last-Modified", "Wed, 01 Jan 2026 00:00:00 GMT"), newH: hdr("Last-Modified", "Wed, 01 Jan 2026 00:00:00 GMT", "Content-Range", "bytes 100-999/1000"),
			status: 206, clen: 900,
		},
		{
			name: "206 Last-Modified 一致且有摘要", cur: hdr("Last-Modified", "Wed, 01 Jan 2026 00:00:00 GMT"), newH: hdr("Last-Modified", "Wed, 01 Jan 2026 00:00:00 GMT", "Content-Range", "bytes 100-999/1000"),
			status: 206, clen: 900, expect: "abc", wantOK: true,
		},
		{
			// 两边都是相同的弱 ETag：语义等价不等于字节一致，不能直接放行。
			name: "206 两边相同弱 ETag", cur: hdr("ETag", `W/"v1"`), newH: hdr("ETag", `W/"v1"`, "Content-Range", "bytes 100-999/1000"),
			status: 206, clen: 900,
		},
		{
			name: "206 两边相同弱 ETag 有摘要", cur: hdr("ETag", `W/"v1"`), newH: hdr("ETag", `W/"v1"`, "Content-Range", "bytes 100-999/1000"),
			status: 206, clen: 900, expect: "abc", wantOK: true,
		},
		{
			// 一强一弱：弱的那侧证明不了字节一致，即使有摘要也不能换。
			name: "206 一强一弱有摘要", cur: hdr("ETag", `"v1"`), newH: hdr("ETag", `W/"v1"`, "Content-Range", "bytes 100-999/1000"),
			status: 206, clen: 900, expect: "abc",
		},
		{
			// /http 这类没有整份摘要的路径：两边都无强验证器时必须拒绝。
			name: "206 双方无强验证器且无摘要", cur: hdr(), newH: hdr("Content-Range", "bytes 100-999/1000"),
			status: 206, clen: 900,
		},
		{
			name: "206 Last-Modified 不同", cur: hdr("Last-Modified", "Wed, 01 Jan 2026 00:00:00 GMT"), newH: hdr("Last-Modified", "Thu, 02 Jan 2026 00:00:00 GMT", "Content-Range", "bytes 100-999/1000"),
			status: 206, clen: 900,
		},
		{
			name: "200 长度一致", cur: hdr("ETag", `"v1"`), newH: hdr("ETag", `"v1"`),
			status: 200, clen: 1000, wantOK: true, verified: true,
		},
		{
			name: "200 长度不同", cur: hdr("ETag", `"v1"`), newH: hdr("ETag", `"v1"`),
			status: 200, clen: 999,
		},
		{
			name: "200 长度未知", cur: hdr("ETag", `"v1"`), newH: hdr("ETag", `"v1"`),
			status: 200, clen: -1,
		},
		{
			name: "200 验证器不同", cur: hdr("ETag", `"v1"`), newH: hdr("ETag", `"v2"`),
			status: 200, clen: 1000,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			verified, ok := switchAcceptable(c.cur, c.newH, c.status, c.clen, sent, total, c.expect)
			if ok != c.wantOK || verified != c.verified {
				t.Fatalf("ok=%v verified=%v，期望 ok=%v verified=%v", ok, verified, c.wantOK, c.verified)
			}
		})
	}
}

// 非整数秒不能被截断：3.9 秒传 3MiB 约 787KB/s，按 3 秒算会虚高到约 1024KB/s。
func TestWatchIrregularIntervalUsesRealTime(t *testing.T) {
	w := newStreamWatch(ParOptions{WatchWindow: 5 * time.Second, WatchMBps: 3})
	w.winStart = time.Now().Add(-3900 * time.Millisecond)
	w.winBytes = 3 << 20
	w.roll(time.Now())

	kb, sec := w.sample()
	// 独立数学预期，不引用生产代码里的公式。
	size := float64(3 << 20)
	want := int64(size / 3.9 / 1024)
	if kb < want-5 || kb > want+5 {
		t.Fatalf("3.9s/3MiB 应约 %d KB/s，实际 %d（按整秒算会得约 1024）", want, kb)
	}
	if sec != 4 {
		t.Fatalf("3.9s 存储用秒数应向上取整为 4，实际 %d", sec)
	}
}

// 完全没数据时窗口也要前进，卡死必须能判慢。
func TestWatchStallTriggersSlow(t *testing.T) {
	w := newStreamWatch(ParOptions{WatchWindow: 2 * time.Second, WatchMBps: 3, SwitchMinFileMiB: 1, SwitchMaxSentMiB: 48, SwitchMax: 2})
	w.winStart = time.Now().Add(-2 * time.Second)
	w.tick()
	if !w.slow(1<<20, 64<<20) {
		t.Fatal("2 秒零字节应判慢")
	}
}

// 写客户端阻塞不能算源站慢。
func TestWatchWriteBlockNotCountedAsSlow(t *testing.T) {
	w := newStreamWatch(ParOptions{WatchWindow: 2 * time.Second, WatchMBps: 3, SwitchMinFileMiB: 1, SwitchMaxSentMiB: 48, SwitchMax: 2})
	w.winStart = time.Now().Add(-2 * time.Second)
	w.winBytes = 2 << 20
	w.addWrite(1900 * time.Millisecond)
	w.tick()
	if w.slow(1<<20, 64<<20) {
		t.Fatal("阻塞写不该被判成源站慢")
	}
}
