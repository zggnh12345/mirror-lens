// Package probe checks whether a Docker registry mirror or a GitHub proxy
// actually serves real content, and measures how fast it does so.
//
// 原则：状态码不算结论，取到的字节才算。
package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type Result struct {
	OK        bool   `json:"ok"`
	HTTPCode  int    `json:"http_code"`
	LatencyMS int64  `json:"latency_ms"`
	SpeedKBps int64  `json:"speed_kbps"`
	Digest    string `json:"digest"`
	DigestOK  int    `json:"digest_ok"` // -1 未知 / 0 不一致 / 1 一致
	TLSOK     bool   `json:"tls_ok"`
	CertDays  int    `json:"cert_days"`
	Error     string `json:"error"`
	Detail    string `json:"detail"`
}

const userAgent = "mirror-lens/1.0 (+https://github.com/)"

func newClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 8 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

// tlsInfo dials the host and reads its leaf certificate without aborting on
// verification errors, so an expired or self-signed cert is reported rather
// than silently treated as "unreachable".
func tlsInfo(ctx context.Context, host string) (ok bool, days int, err error) {
	d := &net.Dialer{Timeout: 8 * time.Second}
	conn, derr := tls.DialWithDialer(d, "tcp", net.JoinHostPort(host, "443"), &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	})
	if derr != nil {
		return false, 0, derr
	}
	defer conn.Close()
	cs := conn.ConnectionState()
	if len(cs.PeerCertificates) == 0 {
		return false, 0, errors.New("no certificate")
	}
	leaf := cs.PeerCertificates[0]
	days = int(time.Until(leaf.NotAfter).Hours() / 24)

	// 独立验证：域名 + 链。自签/过期/域名不匹配都算 TLS 不可信。
	pool := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		pool.AddCert(c)
	}
	_, verr := leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: pool, Intermediates: pool})
	if verr != nil && len(cs.PeerCertificates) > 1 {
		// 用系统根再试一次，覆盖正常签发链
		_, verr = leaf.Verify(x509.VerifyOptions{DNSName: host})
	}
	return verr == nil, days, nil
}

func trimBase(base string) string {
	base = strings.TrimSpace(base)
	base = strings.TrimSuffix(base, "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "https://" + base
	}
	return base
}

func hostOf(base string) string {
	b := trimBase(base)
	b = strings.TrimPrefix(b, "https://")
	b = strings.TrimPrefix(b, "http://")
	if i := strings.IndexAny(b, "/"); i >= 0 {
		b = b[:i]
	}
	return b
}

func readCapped(r io.Reader, max int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, max))
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

func statusOf(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

var _ = fmt.Sprintf
