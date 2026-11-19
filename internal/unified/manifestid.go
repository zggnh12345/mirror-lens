package unified

import (
	"sort"
	"strconv"
	"strings"
)

// ManifestIdentity 是 Manifest 的引用 + 内容协商语义。
//
// 冻结（3.5.17 §2.2 / 17b 第三轮）：ClaimKey 必须包含规范化 Ref 与内容协商
// 语义（Accept 媒体类型、参数、优先级 q 值与顺序）。同一 Ref 在不同 Accept
// 下是不同身份，不能用「同 Host/Repo/Ref」判等价；认证上下文仍不参与身份。
//
// Accept 存放规范化后的有序串（逗号分隔），因此 Identity 保持可比较，
// 可直接作为 Claim 表的 map key。
type ManifestIdentity struct {
	Ref    string
	Accept string
}

// Empty 报告引用是否缺失。Accept 可以为空（表示客户端没有声明偏好）。
func (m ManifestIdentity) Empty() bool { return m.Ref == "" }

// Key 返回稳定的身份串，只用于日志/诊断。
func (m ManifestIdentity) Key() string {
	return m.Ref + "|" + m.Accept
}

// NormalizeManifestIdentity 规范化 Ref 与 Accept 头。
//
// Accept 语义保留三件事：媒体类型、参数（含 q）、顺序。规范化只做
// 大小写/空白/参数顺序这类无语义差异的合并，绝不删除 q 值或重排优先级。
func NormalizeManifestIdentity(ref string, accept []string) ManifestIdentity {
	return ManifestIdentity{Ref: strings.TrimSpace(ref), Accept: NormalizeAccept(accept)}
}

// NormalizeAccept 把 Accept 头值规范化为「按原顺序、去重」的稳定串。
//
// 输入可以是多行头值（HTTP 允许），顺序即优先级顺序。
func NormalizeAccept(headers []string) string {
	var out []string
	seen := map[string]bool{}
	for _, h := range headers {
		for _, part := range splitAccept(h) {
			c := canonicalMediaRange(part)
			if c == "" || seen[c] {
				continue
			}
			seen[c] = true
			out = append(out, c)
		}
	}
	return strings.Join(out, ",")
}

// splitAccept 按未加引号的逗号切分 Accept 值。
func splitAccept(s string) []string {
	var parts []string
	var b strings.Builder
	quoted := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch ch {
		case '"':
			quoted = !quoted
			b.WriteByte(ch)
		case ',':
			if quoted {
				b.WriteByte(ch)
				continue
			}
			parts = append(parts, b.String())
			b.Reset()
		default:
			b.WriteByte(ch)
		}
	}
	if b.Len() > 0 {
		parts = append(parts, b.String())
	}
	return parts
}

// canonicalMediaRange 规范化单个媒体范围：type/subtype 小写、去空白、
// 参数按名字排序、q 用最少小数位表示。无 type/subtype 的条目丢弃。
func canonicalMediaRange(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	segs := splitParams(raw)
	mt := strings.ToLower(strings.TrimSpace(segs[0]))
	if mt == "" {
		return ""
	}
	params := make([]string, 0, len(segs)-1)
	for _, p := range segs[1:] {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		if k == "" {
			continue
		}
		if k == "q" {
			// q 决定优先级，不能丢；只做数值规范化（0.9 与 .90 等价）。
			// q=1 是默认值，显式写出与省略语义等价，统一省略。
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				if f == 1 {
					continue
				}
				v = strconv.FormatFloat(f, 'f', -1, 64)
			}
		}
		params = append(params, k+"="+v)
	}
	sort.Strings(params)
	if len(params) == 0 {
		return mt
	}
	return mt + ";" + strings.Join(params, ";")
}

// splitParams 按未加引号的分号切分媒体范围。
func splitParams(s string) []string {
	var parts []string
	var b strings.Builder
	quoted := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch ch {
		case '"':
			quoted = !quoted
			b.WriteByte(ch)
		case ';':
			if quoted {
				b.WriteByte(ch)
				continue
			}
			parts = append(parts, b.String())
			b.Reset()
		default:
			b.WriteByte(ch)
		}
	}
	parts = append(parts, b.String())
	return parts
}
