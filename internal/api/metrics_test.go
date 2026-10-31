package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/auth"
	"github.com/zggnh12345/mirror-lens/internal/metrics"
)

func TestMetricsEndpointPublic(t *testing.T) {
	metrics.Enable()
	t.Cleanup(metrics.Disable)
	_, h := newTestServerOpts(t, WithMetrics(true, false))
	h = metrics.Middleware(h) // 生产里就是这个顺序

	// 先制造一条请求，指标里才有东西
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/healthz", nil))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("公开模式应 200，得到 %d", rec.Code)
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("Content-Type: %q", rec.Header().Get("Content-Type"))
	}
	// 刚才这次 /healthz 探活式的请求会进统计，说明中间件与注册表接上了
	if !strings.Contains(rec.Body.String(), "mirrorlens_") {
		t.Fatalf("输出里没有指标:\n%s", rec.Body.String())
	}
}

func TestMetricsEndpointDisabled(t *testing.T) {
	metrics.Enable()
	t.Cleanup(metrics.Disable)
	_, h := newTestServerOpts(t, WithMetrics(false, false))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	// 关闭后没有这个端点，落到面板的登录跳转上；关键是拿不到指标。
	if rec.Code == 200 && strings.Contains(rec.Body.String(), "mirrorlens_") {
		t.Fatalf("关闭后仍能抓到指标：%d", rec.Code)
	}
}

func TestMetricsEndpointTokenGate(t *testing.T) {
	metrics.Enable()
	t.Cleanup(metrics.Disable)
	st, h := newTestServerOpts(t, WithMetrics(true, true))
	tok, _, err := st.CreateToken("prometheus")
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("没带凭据应 401，得到 %d", rec.Code)
	}

	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("带正确 Token 应 200，得到 %d：%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("错误 Token 应 401，得到 %d", rec.Code)
	}
}

// 默认密码状态下，面板会话不能当作抓取凭据——admin/admin 谁都知道。
func TestMetricsSessionIgnoredWithDefaultPassword(t *testing.T) {
	metrics.Enable()
	t.Cleanup(metrics.Disable)
	st, h := newTestServerOpts(t, WithMetrics(true, true))

	if err := st.SetSetting(auth.KeyMustChg, "1"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession("sess-default-pw", time.Hour); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/metrics", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: "sess-default-pw"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("默认密码状态下 cookie 应被拒，得到 %d", rec.Code)
	}

	if err := st.SetSetting(auth.KeyMustChg, "0"); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("改密后 cookie 应放行，得到 %d：%s", rec.Code, rec.Body.String())
	}
}
