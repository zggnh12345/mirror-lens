// Package sums 解析发布者提供的 SHA256 校验文件。
//
// 只做「把文件里的声明读出来」，不证明发布者身份：
// 它能说明下载到的字节与校验文件里写的一致，不能说明校验文件本身可信。
package sums

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var (
	ErrEmpty     = errors.New("校验文件为空")
	ErrNoEntry   = errors.New("校验文件中没有目标文件")
	ErrAmbiguous = errors.New("校验文件中存在同名但摘要不同的记录")
	ErrInvalid   = errors.New("校验文件没有可解析的记录")
)

var bsdRe = regexp.MustCompile(`^SHA256\s*\((.+)\)\s*=\s*([0-9a-fA-F]{64})$`)

type entry struct {
	name   string
	digest string
}

// Parse 接受 GNU sha256sum 与 BSD shasum 两种格式，容忍 CRLF、空行、
// 以 # 开头的注释，以及 GNU 的二进制前缀（"<hex> *name"）。
func Parse(data []byte) ([]entry, error) {
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var out []entry
	sawContent := false
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		sawContent = true
		if m := bsdRe.FindStringSubmatch(trimmed); m != nil {
			out = append(out, entry{name: normalizeName(m[1]), digest: strings.ToLower(m[2])})
			continue
		}
		// GNU：64 位十六进制 + 空格 + 可选 '*' + 文件名。文件名允许含空格。
		if len(line) < 66 {
			continue
		}
		hex := strings.ToLower(strings.TrimSpace(line[:64]))
		if !isHex64(hex) {
			continue
		}
		rest := line[64:]
		if !strings.HasPrefix(rest, " ") && !strings.HasPrefix(rest, "\t") {
			continue
		}
		rest = strings.TrimLeft(rest, " \t")
		rest = strings.TrimPrefix(rest, "*")
		rest = strings.TrimSpace(rest)
		if rest == "" {
			continue
		}
		out = append(out, entry{name: normalizeName(rest), digest: hex})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !sawContent {
		return nil, ErrEmpty
	}
	if len(out) == 0 {
		return nil, ErrInvalid
	}
	return out, nil
}

// Lookup 精确匹配目标文件名（含路径时取最后一段）。
// 同名多摘要视为歧义，拒绝自动选择——猜错等于放行错误内容。
func Lookup(data []byte, target string) (string, error) {
	entries, err := Parse(data)
	if err != nil {
		return "", err
	}
	want := normalizeName(target)
	var found string
	for _, e := range entries {
		if e.name != want {
			continue
		}
		if found != "" && found != e.digest {
			return "", fmt.Errorf("%w: %s", ErrAmbiguous, want)
		}
		found = e.digest
	}
	if found == "" {
		return "", fmt.Errorf("%w: %s", ErrNoEntry, want)
	}
	return found, nil
}

func normalizeName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "./")
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	return s
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
