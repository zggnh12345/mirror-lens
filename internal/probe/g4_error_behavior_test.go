package probe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// §15.5 证书错误不被后续连接错误覆盖（行为级，非静态扫描）：
// 自签 HTTPS 源上，tlsInfo 先写入 TLS_CERT_UNTRUSTED；
// 后续 /v2/ 请求同样因证书失败，但不得覆盖更具体的 TLS 根因。
func TestG4TLSErrorNotOverwrittenByHTTP(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	res := Docker(context.Background(), srv.URL, 2, "")
	if res.Error == "" {
		t.Fatal("自签证书应产生错误")
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(res.Error), &obj); err != nil {
		t.Fatalf("错误不是结构化 JSON: %s", res.Error)
	}
	code, _ := obj["code"].(string)
	if code != "TLS_CERT_UNTRUSTED" {
		t.Fatalf("最终错误码=%q，期望 TLS_CERT_UNTRUSTED（被 HTTP 错误覆盖）: %s", code, res.Error)
	}
}
