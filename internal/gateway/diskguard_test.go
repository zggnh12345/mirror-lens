package gateway

import (
	"bytes"
	"testing"
)

// 磁盘剩余低于底线时必须「照样能下、但不落盘」：用户拿到正确内容，
// 盘上不留东西，下一次还是回源。这是硬约束——不能因为磁盘紧张就返回 5xx。
func TestLowDiskServesButDoesNotCache(t *testing.T) {
	f := newHTTPFixture(t, 0)
	f.gw.cacheMinFree = 1 << 62 // 任何真实磁盘都不可能剩这么多

	w := f.req(t, f.srv.URL+"/f.bin", nil)
	if w.Code != 200 {
		t.Fatalf("低于底线也要正常服务，实际 %d", w.Code)
	}
	if !bytes.Equal(w.Body.Bytes(), f.up.body) {
		t.Fatal("内容必须逐字节正确")
	}
	if f.cs.Has(sha(f.up.body)) {
		t.Fatal("低于底线还落了盘")
	}
	if !f.gw.DiskLow() {
		t.Fatal("DiskLow() 应为真")
	}

	// 第二遍仍然回源：缓存里没东西，自然没有命中。
	_ = f.req(t, f.srv.URL+"/f.bin", nil)
	if n := f.up.hits; n != 2 {
		t.Fatalf("应回源 2 次，实际 %d 次", n)
	}
}

// 没设底线（0）时行为必须与加这个功能前一致：正常落盘、第二遍命中。
func TestNoMinFreeKeepsCaching(t *testing.T) {
	f := newHTTPFixture(t, 0)
	if f.gw.DiskLow() {
		t.Fatal("未设底线时不该拦")
	}
	f.req(t, f.srv.URL+"/f.bin", nil)
	if !f.cs.Has(sha(f.up.body)) {
		t.Fatal("未设底线时应正常落盘")
	}
}
