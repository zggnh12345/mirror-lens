package gateway

import (
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// digestFromHeader 从上游响应头找 sha256（小写十六进制）。
// 上游自报摘要只证明「字节没传坏」，不是官方那份，只用来
// 拒绝不一致的字节、不升级 verified；可信摘要来自客户端 URL。
func digestFromHeader(h http.Header) string {
	for _, k := range []string{"Content-Digest", "Digest"} {
		if v := h.Get(k); v != "" {
			if d := parseDigestHeader(v); d != "" {
				return d
			}
		}
	}
	if d := normHex(h.Get("X-Checksum-Sha256")); d != "" {
		return d
	}
	if v := strings.TrimSpace(h.Get("x-amz-checksum-sha256")); v != "" {
		if b, err := base64.StdEncoding.DecodeString(v); err == nil && len(b) == 32 {
			return hex.EncodeToString(b)
		}
	}
	return ""
}

// parseDigestHeader 解析 RFC 9530 的 `sha-256=:BASE64:`，
// 兼容不带冒号的写法。
func parseDigestHeader(v string) string {
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		i := strings.Index(part, "=")
		if i < 0 {
			continue
		}
		algo := strings.ToLower(strings.TrimSpace(part[:i]))
		if algo != "sha-256" && algo != "sha256" {
			continue
		}
		val := strings.Trim(strings.TrimSpace(part[i+1:]), ":")
		if b, err := base64.StdEncoding.DecodeString(val); err == nil && len(b) == 32 {
			return hex.EncodeToString(b)
		}
		if d := normHex(val); d != "" {
			return d
		}
	}
	return ""
}

func normHex(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if len(v) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(v); err != nil {
		return ""
	}
	return v
}

// checkDigest 在响应头带摘要时比对；没有摘要返回 (false, true)，表示没得比。
func checkDigest(h http.Header, sum string) (checked, ok bool) {
	want := digestFromHeader(h)
	if want == "" {
		return false, true
	}
	return true, strings.EqualFold(want, sum)
}

// verifyState：只有客户端给的摘要（docker digest 拉取时在 URL 里）才算参照；
// 上游自报的摘要挡得住传坏，挡不住换内容，不算。
func verifyState(plan *adapter.Plan) string {
	if plan.ExpectSHA != "" {
		return store.VerifyVerified
	}
	return store.VerifyUnverified
}
