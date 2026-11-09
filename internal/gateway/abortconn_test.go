package gateway

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/metrics"
)

// abortProbe 让服务端写一半响应后调用 abort，客户端用裸 TCP 读，
// 以便区分 RST（ECONNRESET）和正常的 FIN。
func abortProbe(t *testing.T, wrap func(http.Handler) http.Handler) error {
	t.Helper()
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("0123456789"))
		if f, ok := abortResponse(w); ok {
			f()
		}
	})
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(srv.URL, "http://"), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = conn.Write([]byte("GET / HTTP/1.1\r\nHost: probe\r\nConnection: close\r\n\r\n"))
	_, err = io.Copy(io.Discard, conn)
	return err
}

// 原始 ResponseWriter：必须让客户端看到连接被重置，而不是正常结束。
func TestAbortResponseResetsConnection(t *testing.T) {
	err := abortProbe(t, nil)
	if !isReset(err) {
		t.Fatalf("应观察到连接重置，实际: %v", err)
	}
}

// 生产链路（metrics.Middleware 包一层 recorder）同样要能重置。
// 这条是 P0 回归：直接做 Hijacker 类型断言时这里会退化成正常收尾。
func TestAbortResponseUnderMetricsMiddleware(t *testing.T) {
	metrics.Enable()
	defer metrics.Disable()
	err := abortProbe(t, metrics.Middleware)
	if !isReset(err) {
		t.Fatalf("经过 metrics.Middleware 后应观察到连接重置，实际: %v", err)
	}
}

// 不支持 Hijack 的 writer 必须报告 ok=false，调用方据此拒绝接管。
func TestAbortResponseWithoutHijacker(t *testing.T) {
	if f, ok := abortResponse(httptest.NewRecorder()); ok || f != nil {
		t.Fatal("Recorder 不支持 Hijack，应返回 ok=false")
	}
}

// HTTP/2 没有 Hijack：必须识别为不支持，midfill 不能接管。
func TestAbortResponseHTTP2Unsupported(t *testing.T) {
	metrics.Disable()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("期望 HTTP/2，实际 %s", r.Proto)
		}
		_, ok := abortResponse(w)
		_, _ = w.Write([]byte(map[bool]string{true: "ok", false: "no"}[ok]))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "no" {
		t.Fatalf("HTTP/2 下不应支持 Hijack，实际返回 %q", body)
	}
}

// killConn 为 nil 时 midFill 必须直接拒绝，不能接管后再失败。
func TestMidFillRefusesWithoutAbort(t *testing.T) {
	g := &Gateway{}
	if taken, ok := g.midFill(io.Discard, nil, nil, 0, 0, nil, nil, nil); taken || ok {
		t.Fatal("无法异常终止连接时不应接管 midfill")
	}
}

// 重复调用不能二次关闭连接（所有权唯一）。
func TestAbortResponseCloseOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("x"))
		f, ok := abortResponse(w)
		if !ok {
			t.Error("应支持 Hijack")
			return
		}
		f()
		f() // 第二次必须是无操作
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func isReset(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	// 某些内核/回环路径下读端先看到 EOF，再报 reset。
	return strings.Contains(strings.ToLower(err.Error()), "reset")
}
