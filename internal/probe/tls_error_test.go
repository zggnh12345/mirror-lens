package probe

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestClassifyVerifyErrorPreservesType(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
	}{
		{"untrusted", x509.UnknownAuthorityError{}, "TLS_CERT_UNTRUSTED"},
		{"hostname", x509.HostnameError{}, "TLS_HOSTNAME_MISMATCH"},
		{"expired", x509.CertificateInvalidError{Reason: x509.Expired}, "TLS_CERT_EXPIRED"},
		{"notyet", x509.CertificateInvalidError{Reason: x509.Expired, Cert: &x509.Certificate{NotBefore: time.Now().Add(time.Hour)}}, "TLS_CERT_NOT_YET_VALID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyVerifyError(tc.err)
			if got.Code != tc.code {
				t.Fatalf("code=%s 期望 %s", got.Code, tc.code)
			}
			var obj map[string]any
			if json.Unmarshal([]byte(got.Error()), &obj) != nil || obj["code"] != tc.code {
				t.Fatalf("Error() 不是含稳定 code 的结构化对象: %s", got.Error())
			}
			var te *TLSError
			if !errors.As(error(got), &te) {
				t.Fatal("errors.As 不能识别 TLSError")
			}
			if !errors.Is(got, tc.err) && got.Cause != tc.err {
				t.Fatal("cause 未保留")
			}
		})
	}
}

func TestTLSErrorUnwrapKeepsCause(t *testing.T) {
	cause := x509.UnknownAuthorityError{}
	e := classifyVerifyError(cause)
	var u x509.UnknownAuthorityError
	if !errors.As(error(e), &u) {
		t.Fatal("Unwrap 后 errors.As 找不到 x509 原始类型")
	}
}
