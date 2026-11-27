package api

import (
	"bytes"
	"encoding/json"
)

// firstLegacyField 按给定顺序检查 JSON 顶层是否出现旧字段，返回第一个命中的字段名。
func firstLegacyField(raw []byte, fields ...string) string {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	for _, f := range fields {
		if _, ok := obj[f]; ok {
			return f
		}
	}
	return ""
}

// firstUnknownField 返回 JSON 对象中第一个不在允许集合中的字段名。
// path 为空表示顶层；path 非空时检查 obj[path] 这个嵌套对象。
func firstUnknownField(raw []byte, allowed []string, path []string) string {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	cur := obj
	for _, key := range path {
		nested, ok := cur[key]
		if !ok {
			return ""
		}
		cur = nil
		if json.Unmarshal(nested, &cur) != nil {
			return ""
		}
	}
	okset := map[string]bool{}
	for _, a := range allowed {
		okset[a] = true
	}
	for k := range cur {
		if !okset[k] {
			return k
		}
	}
	return ""
}

// rawEqualNull 判断原始 JSON 值是否为显式 null。
func rawEqualNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
