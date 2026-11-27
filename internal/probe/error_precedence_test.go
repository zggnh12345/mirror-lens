package probe

import (
	"os"
	"strings"
	"testing"
)

// F-REV-03 回归：TLS 探测错误写入后，后续 HTTP/token 错误不得覆盖。
// 静态守卫所有 res.Error 赋值：允许初始 TLS 赋值；其余必须在
// `if res.Error == ""` 块内。
func TestProbeErrorNeverOverwritesTLSError(t *testing.T) {
	for _, name := range []string{"docker.go", "github.go"} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(b), "\n")
		for i, line := range lines {
			s := strings.TrimSpace(line)
			if !strings.HasPrefix(s, "res.Error =") {
				continue
			}
			// 初始 TLS 错误赋值（tlsInfo 结果），允许无条件写入。
			if s == "res.Error = errStr(terr)" {
				continue
			}
			prev := ""
			if i > 0 {
				prev = strings.TrimSpace(lines[i-1])
			}
			if prev != `if res.Error == "" {` {
				t.Errorf("%s:%d 未保护的赋值: %s", name, i+1, s)
			}
		}
	}
}
