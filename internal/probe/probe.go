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
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/safehttp"
)

type Result struct {
	OK        bool   `json:"ok"`
	HTTPCode  int    `json:"http_code"`
	LatencyMS int64  `json:"latency_ms"`
	SpeedKBps int64  `json:"speed_kbps"`
	Digest    string `json:"digest"`
	DigestOK  int    `json:"digest_ok"` // manifest：-1 未知 / 0 不一致 / 1 一致
	BlobOK    int    `json:"blob_ok"`   // layer 实际字节：-1 未校验 / 0 不符 / 1 一致
	TLSOK     bool   `json:"tls_ok"`
	CertDays  int    `json:"cert_days"`
	Error     string `json:"error"`
	Detail    string `json:"detail"`
}

const userAgent = "mirror-lens/1.0 (+https://github.com/)"

// 探测同样是在替别人发请求：源地址由用户填写，重定向和 Docker 的 token
// realm 都是对方说了算。所以这里跟网关走同一套安全拨号层，而不是各写一份。
var (
	guardMu sync.RWMutex
	guard   = safehttp.New(safehttp.Options{MaxRedirects: 8})
)

// Configure 设置探测的拨号策略。allowPrivate 打开后允许探测内网/回环地址，
// 只给「源就架在自己局域网里」的场景用；公网部署保持 false。
func Configure(allowPrivate bool) {
	guardMu.Lock()
	guard = safehttp.New(safehttp.Options{MaxRedirects: 8, AllowPrivate: allowPrivate})
	guardMu.Unlock()
}

func currentGuard() *safehttp.Guard {
	guardMu.RLock()
	defer guardMu.RUnlock()
	return guard
}

// CheckURL 用当前探测策略校验一个待添加的源地址。加源时就挡掉明显的
// 内网地址，比加进去再等探测失败清楚。
func CheckURL(u *url.URL) error {
	return currentGuard().Check(u)
}

func newClient(timeout time.Duration) *http.Client {
	c := currentGuard().Client()
	c.Timeout = timeout
	return c
}

// tlsInfo dials the host and reads its leaf certificate without aborting on
// verification errors, so an expired or self-signed cert is reported rather
// than silently treated as "unreachable".
func tlsInfo(ctx context.Context, host string) (ok bool, days int, err error) {
	// 走 Guard 的拨号器，并且吃调用方的 ctx：证书探测不能成为绕过检查、
	// 或者在取消后继续占着连接的口子。
	d := currentGuard().Dialer()
	d.Timeout = 8 * time.Second
	td := &tls.Dialer{NetDialer: d, Config: &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	}}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, derr := td.DialContext(cctx, "tcp", net.JoinHostPort(host, "443"))
	if derr != nil {
		return false, 0, derr
	}
	defer conn.Close()
	cs := conn.(*tls.Conn).ConnectionState()
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
