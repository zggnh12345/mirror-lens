// Package probe checks whether a Docker registry mirror or a GitHub
// proxy actually serves real content, and measures the speed.
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

// 能力判定：只有「对方明确回答不支持」才算结论。超时、DNS、连接被重置、
// 429、5xx 一律 VerdictUnknown——那说明不了它支持什么，只说明此刻不通。
const (
	VerdictUnknown = ""
	VerdictYes     = "yes"
	VerdictNo      = "no"
)

type Result struct {
	OK        bool   `json:"ok"`
	Verdict   string `json:"verdict,omitempty"`
	HTTPCode  int    `json:"http_code"`
	LatencyMS int64  `json:"latency_ms"`
	SpeedKBps int64  `json:"speed_kbps"`
	Digest    string `json:"digest"`
	DigestOK  int    `json:"digest_ok"` // manifest：-1 未知 / 0 不一致 / 1 一致
	BlobOK    int    `json:"blob_ok"`   // layer 实际字节：-1 未校验 / 0 不符 / 1 一致
	TTFBMS    int64  `json:"ttfb_ms"`   // 从发请求到收到正文首字节；0 表示没测
	TLSOK     bool   `json:"tls_ok"`
	CertDays  int    `json:"cert_days"`
	Error     string `json:"error"`
	Detail    string `json:"detail"`
}

const userAgent = "mirror-lens/1.0 (+https://github.com/)"

// ttfbReader 记下「第一次读到正文」用了多久。
// 用 ResponseHeader 的时间不算 TTFB——镜像站常在发完头之后卡住正文。
type ttfbReader struct {
	r     io.Reader
	start time.Time
	ms    int64
	hit   bool
}

func (t *ttfbReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 && !t.hit {
		t.hit = true
		t.ms = time.Since(t.start).Milliseconds()
	}
	return n, err
}

// 探测同样是在替别人发请求：重定向和 Docker 的 token realm 都由对方说了算，
// 所以跟网关走同一套安全拨号层，而不是各写一份。
var (
	guardMu sync.RWMutex
	guard   = safehttp.New(safehttp.Options{MaxRedirects: 8})
)

// Configure 设置探测的拨号策略。allowPrivate 打开后允许探测内网/回环地址，
// 只给「源在自己局域网里」的场景用；公网部署保持 false。
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

// CheckURL 用当前探测策略校验待添加的源地址：加源时就挡掉明显的内网地址，
// 比加进去再等探测失败清楚。
func CheckURL(u *url.URL) error {
	return currentGuard().Check(u)
}

func newClient(timeout time.Duration) *http.Client {
	c := currentGuard().Client()
	c.Timeout = timeout
	return c
}

// verifyChain 用系统根验证叶子证书，对端提供的其余证书只作为中间证书。
func verifyChain(cs tls.ConnectionState, host string) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("no certificate")
	}
	inter := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
		DNSName:       host,
		Intermediates: inter,
	})
	return err
}

// tlsInfo dials the host and reads its leaf certificate without
// aborting on verification errors: an expired or self-signed cert
// is reported as untrusted, not silently "unreachable".
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
	//
	// 关键：根必须是系统根，服务器发来的证书只能当中间证书。以前把对端
	// 证书一股脑塞进 Roots，等于让服务器自己给自己作证。
	verr := verifyChain(cs, host)
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
