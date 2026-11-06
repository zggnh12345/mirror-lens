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
			name: "206 Last-Modified 一致", cur: hdr("Last-Modified", "Wed, 01 Jan 2026 00:00:00 GMT"), newH: hdr("Last-Modified", "Wed, 01 Jan 2026 00:00:00 GMT", "Content-Range", "bytes 100-999/1000"),
			status: 206, clen: 900, wantOK: true, verified: true,
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

// 不规则读取间隔不能被当成 1 秒：3 秒 3MB 是 1MB/s，不是 3MB/s。
func TestWatchIrregularIntervalUsesRealTime(t *testing.T) {
	w := newStreamWatch(ParOptions{WatchWindow: 4 * time.Second, WatchMBps: 3})
	w.winStart = time.Now().Add(-3 * time.Second)
	w.winBytes = 3 << 20
	w.roll(time.Now())

	sum, dur := w.windowStats()
	if dur < 2*time.Second || dur > 4*time.Second {
		t.Fatalf("真实时长应接近 3s，实际 %v", dur)
	}
	kb := sum / int64(dur/time.Second) / 1000
	if kb < 900 || kb > 1200 {
		t.Fatalf("3 秒 3MB 应算出约 1MB/s，实际 %d KB/s", kb)
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
