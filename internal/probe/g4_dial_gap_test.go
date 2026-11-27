package probe

import (
	"errors"
	"net"
	"testing"
	"time"
)

// §15.5 classifyDialError 四类：timeout / dial / 其他（握手）/ DNS 包装。
// 错误码必须稳定，且 cause 通过 Unwrap 保留。
func TestG4ClassifyDialError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
	}{
		{"超时", &net.OpError{Op: "dial", Err: &timeoutErr{}}, "TIMEOUT"},
		{"拨号拒绝", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, "CONNECT_FAILED"},
		{"握手失败", errors.New("remote error: tls: handshake failure"), "TLS_HANDSHAKE_FAILED"},
		{"DNS 包装", &net.DNSError{Err: "no such host", Name: "example.com"}, "DNS_FAILED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyDialError(tc.err, "tls_dial")
			if got == nil || got.Code != tc.code {
				t.Fatalf("code=%v，期望 %s", got, tc.code)
			}
			if got.Stage != "tls_dial" || !got.Retryable || !got.TLSApply {
				t.Fatalf("分类元数据错误: %+v", got)
			}
			if !errors.Is(got, tc.err) {
				t.Fatal("cause 未通过 Unwrap 保留")
			}
		})
	}
}

type timeoutErr struct{}

func (*timeoutErr) Error() string   { return "i/o timeout" }
func (*timeoutErr) Timeout() bool   { return true }
func (*timeoutErr) Temporary() bool { return true }

var _ net.Error = (*timeoutErr)(nil)

// §15.5 技术详情不泄露凭据：TLSError.Error() 不含 token/password/authorization。
func TestG4TLSErrorNoCredentialLeak(t *testing.T) {
	e := &TLSError{
		Code: "TLS_CERT_UNTRUSTED", Stage: "verify", Message: "x509: certificate signed by unknown authority",
		Retryable: false, TLSApply: true,
		Cause: errors.New("secret=supersecrettoken authorization=Bearer abc"),
	}
	s := e.Error()
	for _, needle := range []string{"supersecrettoken", "Bearer abc", "authorization="} {
		if contains(s, needle) {
			t.Fatalf("TLSError.Error() 泄露凭据: %s", s)
		}
	}
}

func contains(s, sub string) bool {
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

var _ = time.Second
