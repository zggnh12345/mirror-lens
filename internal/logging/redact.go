package logging

import (
	"net/url"
	"regexp"
	"strings"
)

// urlLike 匹配错误文本里的 URL：http(s)://... 以及缓存键那种 http:host/path。
// 只用于输出前清洗，不参与任何匹配或存储。
var urlLike = regexp.MustCompile(`https?://[^\s"']+|http:[^\s"']+`)

// trimTrail 把紧跟在 URL 后面、属于标点的字符摘出来，避免把 "):" 当成 URL 一部分。
const trimTrail = `.,;:)]}>'"`

// ScrubURLs 去掉文本中每个 URL 的 userinfo、查询串和 fragment。
// 查询串整段丢弃而不是逐个打码：里面往往就是 token / 签名。
func ScrubURLs(s string) string {
	if s == "" {
		return ""
	}
	return urlLike.ReplaceAllStringFunc(s, func(m string) string {
		tail := ""
		for len(m) > 0 && strings.IndexByte(trimTrail, m[len(m)-1]) >= 0 {
			tail = m[len(m)-1:] + tail
			m = m[:len(m)-1]
		}
		u, err := url.Parse(m)
		if err != nil {
			return m + tail
		}
		u.User = nil
		u.RawQuery = ""
		u.Fragment = ""
		return u.String() + tail
	})
}

// SafeErr 返回只用于日志/告警输出的错误包装：
// Error() 里的 URL 已被清洗，Unwrap 保留原始错误，errors.Is/As 仍然可用。
//
// 它只允许用在输出边界。入库的 Upstream、缓存键、源匹配键必须保持原样，
// 否则缓存命中和源调度会被破坏。
func SafeErr(err error) error {
	if err == nil {
		return nil
	}
	return safeErr{err: err}
}

type safeErr struct{ err error }

func (e safeErr) Error() string { return ScrubURLs(e.err.Error()) }
func (e safeErr) Unwrap() error { return e.err }
