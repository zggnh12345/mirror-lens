package gateway

import (
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// digestFromHeader 从上游响应头里找一个 sha256 摘要，返回小写十六进制。
//
// 这些头都是供货方自己声明的：命中它只能证明「字节没传坏」，不能证明
// 「这就是官方那份」——同一个坏镜像完全可以附上自己那份内容的正确摘要。
// 所以它不把内容升级成 verified，只用来拒绝明显不一致的字节。
//
// 真正的可信摘要是客户端给的：docker 按 digest 拉 blob 时，摘要写在 URL 里，
// 那份参照与供货方无关，才能算 verified。
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

// parseDigestHeader 解析 RFC 9530 的 `sha-256=:BASE64:`，兼容不带冒号的写法。
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

// verifyState 判定这份内容算什么级别。
//
// 只有「客户端给的摘要」才算参照（docker 按 digest 拉取时摘要写在 URL 里）；
// 上游自报的摘要不算，它只能挡住传坏的字节，挡不住换掉的内容。
func verifyState(plan *adapter.Plan) string {
	if plan.ExpectSHA != "" {
		return store.VerifyVerified
	}
	return store.VerifyUnverified
}
