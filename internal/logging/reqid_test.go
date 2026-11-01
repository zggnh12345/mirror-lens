package logging

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMiddlewareGeneratesID(t *testing.T) {
	var seen string
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestID(r.Context())
		w.WriteHeader(204)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))

	id := rec.Header().Get(HeaderRequestID)
	if id == "" || len(id) != 16 {
		t.Fatalf("应生成 16 位 ID，得到 %q", id)
	}
	if seen != id {
		t.Fatalf("context 里的 ID 与响应头不一致: %q vs %q", seen, id)
	}
}

func TestMiddlewareKeepsUpstreamID(t *testing.T) {
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if RequestID(r.Context()) != "abc12345" {
			t.Fatalf("应沿用上游 ID，得到 %q", RequestID(r.Context()))
		}
	}))
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set(HeaderRequestID, "abc12345")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get(HeaderRequestID) != "abc12345" {
		t.Fatal("响应头应回写同一个 ID")
	}
}

// 客户端的值不可信：太长、带换行、带空格的都丢掉重生成。
func TestMiddlewareRejectsBadID(t *testing.T) {
	bad := []string{
		"short",
		strings.Repeat("a", 65),
		"abc\r\nX-Injected: 1",
		"abc def ghi",
		"../../etc/passwd",
	}
	for _, v := range bad {
		var got string
		h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = RequestID(r.Context())
		}))
		req := httptest.NewRequest("GET", "/x", nil)
		req.Header.Set(HeaderRequestID, v)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got == v {
			t.Errorf("非法 ID %q 不该被沿用", v)
		}
		if len(got) != 16 {
			t.Errorf("应重新生成 16 位 ID，得到 %q", got)
		}
	}
}

func TestRequestIDOnPlainContext(t *testing.T) {
	if RequestID(nil) != "" {
		t.Fatal("nil context 应返回空串")
	}
}
