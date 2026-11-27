// Package probe checks whether a Docker registry mirror or a GitHub
// proxy actually serves real content, and measures the speed.
//
// 原则：状态码不算结论，取到的字节才算。
package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/endpoint"
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
	DigestOK  int    `json:"digest_ok"`          // manifest：-1 未知 / 0 与官方基准不同 / 1 一致
	Tampered  bool   `json:"tampered,omitempty"` // 内容与自身声明不符（真篡改），与「落后官方 tag」区分
	BlobOK    int    `json:"blob_ok"`            // layer 实际字节：-1 未校验 / 0 不符 / 1 一致
	TTFBMS    int64  `json:"ttfb_ms"`            // 从发请求到收到正文首字节；0 表示没测
	TLSOK     bool   `json:"tls_ok"`
	CertDays  int    `json:"cert_days"`
	// GhcrPrefix 表示这次 ghcr 探测是通过「带 ghcr.io 前缀」格式打通的。
	GhcrPrefix bool   `json:"ghcr_prefix,omitempty"`
	Error      string `json:"error"`
	Detail     string `json:"detail"`
}

const userAgent = "mirror-lens/1.0 (+https://github.com/zggnh12345/mirror-lens)"

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

// Configure 设置探测的生产拨号策略。allowPrivate 只放行 RFC1918 + IPv6 ULA，
// 与回环完全独立：本函数永不打开 AllowLoopback，生产路径不存在任何隐式回环权限。
// ipFamily 必须和回源一致：探测走 v4、下载走 v6 时，源会被判成「能连」但实际拉不动。
func Configure(allowPrivate bool, ipFamily ...string) {
	configure(allowPrivate, false, ipFamily...)
}

// ConfigureForTest 仅供测试：显式打开回环以连本地 listener 桩。
// 生产代码（main.go）不得调用本函数；它不新增任何用户可见配置字段。
func ConfigureForTest(allowPrivate, allowLoopback bool, ipFamily ...string) {
	configure(allowPrivate, allowLoopback, ipFamily...)
}

func configure(allowPrivate, allowLoopback bool, ipFamily ...string) {
	fam := "auto"
	if len(ipFamily) > 0 && ipFamily[0] != "" {
		fam = ipFamily[0]
	}
	guardMu.Lock()
	guard = safehttp.New(safehttp.Options{
		MaxRedirects: 8, AllowPrivate: allowPrivate,
		AllowLoopback: allowLoopback, IPFamily: fam,
	})
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

// TLSError 是 TLS 诊断的结构化错误。Error() 以稳定 code 开头，
// 使 store/API 无需改 Schema 也能把错误码透传到 UI。
type TLSError struct {
	Code      string
	Stage     string
	Message   string
	Retryable bool
	TLSApply  bool
	Cause     error
}

func (e *TLSError) Error() string {
	// 结构化错误对象以 JSON 透传到 store/API/UI，不依赖错误文本正则分类。
	// cause 只在 Unwrap 中保留给内部日志，对外只给类型名，避免细节外泄。
	causeType := ""
	if e.Cause != nil {
		causeType = fmt.Sprintf("%T", e.Cause)
	}
	b, _ := json.Marshal(map[string]any{
		"code": e.Code, "stage": e.Stage, "message": e.Message,
		"detail": "", "retryable": e.Retryable, "tls_applicable": e.TLSApply,
		"cause_type": causeType,
	})
	return string(b)
}

func (e *TLSError) Unwrap() error { return e.Cause }

func nowBefore(t time.Time) bool { return time.Now().Before(t) }

// classifyVerifyError 按 Go x509 原始错误类型分类，不用错误字符串正则。
func classifyVerifyError(err error) *TLSError {
	base := TLSError{Stage: "tls_verify", TLSApply: true, Retryable: false, Cause: err}
	var u x509.UnknownAuthorityError
	if errors.As(err, &u) {
		base.Code, base.Message = "TLS_CERT_UNTRUSTED", "证书不在系统信任链中"
		return &base
	}
	var h x509.HostnameError
	if errors.As(err, &h) {
		base.Code, base.Message = "TLS_HOSTNAME_MISMATCH", "证书与访问域名不匹配"
		return &base
	}
	var c x509.CertificateInvalidError
	if errors.As(err, &c) {
		if c.Cert != nil && nowBefore(c.Cert.NotBefore) {
			base.Code, base.Message = "TLS_CERT_NOT_YET_VALID", "证书尚未生效"
			return &base
		}
		switch c.Reason {
		case x509.Expired:
			base.Code, base.Message = "TLS_CERT_EXPIRED", "证书已过期"
		case x509.NotAuthorizedToSign, x509.CANotAuthorizedForThisName, x509.IncompatibleUsage:
			base.Code, base.Message = "TLS_CERT_UNTRUSTED", "证书验证失败"
		default:
			base.Code, base.Message = "TLS_CERT_UNTRUSTED", "证书验证失败"
		}
		return &base
	}
	base.Code, base.Message = "TLS_CERT_UNTRUSTED", "证书验证失败"
	return &base
}

// classifyDialError 区分 DNS/TCP/TLS 握手/超时，避免一律归为证书问题。
func classifyDialError(err error, stage string) *TLSError {
	base := TLSError{Stage: stage, TLSApply: true, Retryable: true, Cause: err}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		base.Code, base.Message = "TIMEOUT", "连接或握手超时"
		return &base
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Op == "dial" {
			base.Code, base.Message = "CONNECT_FAILED", "TCP 连接失败"
			return &base
		}
	}
	base.Code, base.Message = "TLS_HANDSHAKE_FAILED", "TLS 握手失败"
	return &base
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
//
// 3.5.24：地址必须来自结构化 SourceEndpoint，拨号端口用 EffectivePort，
// ServerName/verifyChain 只用裸 Host（不带端口/方括号）。
func tlsInfo(ctx context.Context, base string) (ok bool, days int, err error) {
	ep, err := tlsEndpoint(base)
	if err != nil {
		return false, 0, err
	}
	if ep.Scheme() != endpoint.HTTPS {
		return false, 0, errors.New("HTTP SourceEndpoint 不做 TLS 探测")
	}
	host, port := ep.Host(), ep.Port()
	// 走 Guard 的拨号器，并且吃调用方的 ctx：证书探测不能成为绕过检查、
	// 或者在取消后继续占着连接的口子。
	g := currentGuard()
	d := g.Dialer()
	d.Timeout = 8 * time.Second
	// 和回源用同一套 IP 族偏好：只按域名交给系统解析，v6 半通的源会被测成
	// 「连不上」，白白冤枉一个源。
	target := net.JoinHostPort(host, strconv.Itoa(port))
	if _, err := netip.ParseAddr(host); err != nil {
		if ips, lerr := net.DefaultResolver.LookupNetIP(ctx, "ip", host); lerr == nil {
			if ordered := g.OrderIPs(ips); len(ordered) > 0 {
				target = net.JoinHostPort(ordered[0].String(), strconv.Itoa(port))
			}
		}
	}
	td := &tls.Dialer{NetDialer: d, Config: &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	}}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, derr := td.DialContext(cctx, "tcp", target)
	if derr != nil {
		return false, 0, classifyDialError(derr, "tls_dial")
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
	if verr != nil {
		return false, days, classifyVerifyError(verr)
	}
	return true, days, nil
}

func trimBase(base string) string {
	base = strings.TrimSpace(base)
	base = strings.TrimSuffix(base, "/")
	// 3.5.24：Scheme 大小写不敏感。不得用小写前缀判断后误补 https://，
	// 否则 HTTP:// 大写形式的明文源会被错当 HTTPS 并执行 TLS 探测（§2.6）。
	if _, err := parseScheme(base); err == nil {
		return base
	}
	return "https://" + base
}

// parseScheme 用统一地址模型识别 Scheme，大小写不敏感。
// 仅用于判断"是否显式带 http/https scheme"，不解析 Host；
// 无 scheme 的裸域名返回错误，由 trimBase 补默认值。
func parseScheme(base string) (endpoint.Endpoint, error) {
	return endpoint.Parse(base)
}

// tlsEndpoint 把 sources.base_url 解析成 SourceEndpoint：
// Scheme + CanonicalHost + EffectivePort（https 默认 443，http 默认 80）。
func tlsEndpoint(base string) (endpoint.Endpoint, error) {
	return endpoint.Parse(trimBase(base))
}

// isPlainHTTP 判断 SourceEndpoint 是否显式 HTTP；HTTP 源不做 TLS 探测。
// 3.5.24：改用统一地址模型判定 Scheme，消除 HTTP:// 大写形式绕过。
func isPlainHTTP(base string) bool {
	ep, err := parseScheme(trimBase(base))
	return err == nil && ep.Scheme() == endpoint.HTTP
}

func readCapped(r io.Reader, max int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, max))
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	var te *TLSError
	if errors.As(err, &te) {
		return te.Error()
	}
	s := err.Error()
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

var _ = fmt.Sprintf
