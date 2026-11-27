package probe

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"
)

// 构造一张由 CA 签发的叶子证书；tpl 可覆盖 SAN / 有效期。
func issueCert(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, tpl *x509.Certificate) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl.SerialNumber = big.NewInt(time.Now().UnixNano())
	tpl.KeyUsage = x509.KeyUsageDigitalSignature
	tpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	tpl.BasicConstraintsValid = true
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c, key
}

func testCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c, key
}

// §15.5 SAN 负向 + 过期/未生效/未知 CA/缺中间证书：
// 这些场景都必须被 verifyChain 拒绝，分类由 classifyVerifyError 给出稳定 code。
func TestG4TLSChainRejections(t *testing.T) {
	ca, caKey := testCA(t)
	cases := []struct {
		name  string
		host  string
		build func() *x509.Certificate
		code  string
	}{
		{"SAN 不匹配", "example.com", func() *x509.Certificate {
			c, _ := issueCert(t, ca, caKey, &x509.Certificate{DNSNames: []string{"other.example"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)})
			return c
		}, "TLS_HOSTNAME_MISMATCH"},
		{"IP SAN 错配", "192.0.2.10", func() *x509.Certificate {
			c, _ := issueCert(t, ca, caKey, &x509.Certificate{NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"example.com"}})
			return c
		}, "TLS_HOSTNAME_MISMATCH"},
		{"过期证书", "example.com", func() *x509.Certificate {
			c, _ := issueCert(t, ca, caKey, &x509.Certificate{DNSNames: []string{"example.com"}, NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: time.Now().Add(-time.Hour)})
			return c
		}, "TLS_CERT_EXPIRED"},
		{"未生效证书", "example.com", func() *x509.Certificate {
			c, _ := issueCert(t, ca, caKey, &x509.Certificate{DNSNames: []string{"example.com"}, NotBefore: time.Now().Add(time.Hour), NotAfter: time.Now().Add(48 * time.Hour)})
			return c
		}, "TLS_CERT_NOT_YET_VALID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			leaf := tc.build()
			err := verifyChain(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}, tc.host)
			if err == nil {
				t.Fatal("应拒绝")
			}
			got := classifyVerifyError(err)
			if got == nil || got.Code != tc.code {
				t.Fatalf("错误码=%v，期望 %s（err=%v）", got, tc.code, err)
			}
		})
	}
}

// 未知 CA：叶子由另一张未信任 CA 签发，verifyChain 必拒。
func TestG4TLSUnknownCA(t *testing.T) {
	otherCA, otherKey := testCA(t)
	leaf, _ := issueCert(t, otherCA, otherKey, &x509.Certificate{DNSNames: []string{"example.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)})
	err := verifyChain(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}, "example.com")
	if err == nil {
		t.Fatal("未知 CA 应拒绝")
	}
	if got := classifyVerifyError(err); got == nil || got.Code != "TLS_CERT_UNTRUSTED" {
		t.Fatalf("错误码=%v，期望 TLS_CERT_UNTRUSTED", got)
	}
}

// 缺失中间证书：中间未随链下发，verifyChain 必拒。
func TestG4TLSMissingIntermediate(t *testing.T) {
	ca, caKey := testCA(t)
	// 造一张中间 CA（由根签发）
	interKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	interTpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "inter"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	ider, _ := x509.CreateCertificate(rand.Reader, interTpl, ca, &interKey.PublicKey, caKey)
	inter, _ := x509.ParseCertificate(ider)
	leaf, _ := issueCert(t, inter, interKey, &x509.Certificate{DNSNames: []string{"example.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)})
	// 只下发 leaf，不附中间
	err := verifyChain(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}, "example.com")
	if err == nil {
		t.Fatal("缺中间证书应拒绝")
	}
	// 附上中间后：根 CA 未在系统信任库，仍应拒（验证中间会参与链构建）
	err2 := verifyChain(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf, inter}}, "example.com")
	if err2 == nil {
		t.Fatal("测试根 CA 不在系统信任库，附带中间也不应通过")
	}
	_ = net.IPv4len // 保留 net 导入以便扩展 IP SAN 场景
}
