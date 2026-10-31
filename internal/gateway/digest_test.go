package gateway

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func TestParseDigestHeader(t *testing.T) {
	sum := sha256.Sum256([]byte("hello"))
	b64 := base64.StdEncoding.EncodeToString(sum[:])
	hexSum := hex.EncodeToString(sum[:])

	cases := map[string]string{
		"sha-256=:" + b64 + ":":                 hexSum, // RFC 9530
		"sha-256=" + b64:                        hexSum, // 不带冒号
		"sha-512=:AAAA:, sha-256=:" + b64 + ":": hexSum, // 多算法里挑 sha-256
		"md5=:AAAA:":                            "",     // 没有 sha-256
		"sha-256=:not-base64:":                  "",     // 解不开
	}
	for in, want := range cases {
		if got := parseDigestHeader(in); got != want {
			t.Errorf("parseDigestHeader(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// 上游自报的摘要对不上时必须拒收，不能把坏字节写进缓存。
func TestBogusUpstreamDigestNotCached(t *testing.T) {
	g, st, _, up, _ := newGW(t, true)
	tok := token(t, st)
	up.mu.Lock()
	up.digest = "sha-256=:" + base64.StdEncoding.EncodeToString(make([]byte, 32)) + ":"
	up.mu.Unlock()

	rec := do(g, tok, "http://gw/raw/o/r/main/a.txt", nil)
	if rec.Code < 500 {
		t.Fatalf("摘要不符应失败，得到 %d %q", rec.Code, rec.Body.String())
	}
	if n := len(mustList(t, g)); n != 0 {
		t.Fatalf("校验不过的内容不该入缓存，实际 %d 条", n)
	}
	if g.VerifyFailed() != 1 {
		t.Fatalf("verify_failed 应计 1 次，实际 %d", g.VerifyFailed())
	}
}

// 自报摘要对得上，也只算 unverified：它挡得住传坏，挡不住换内容。
func TestSelfDeclaredDigestStaysUnverified(t *testing.T) {
	g, st, _, up, _ := newGW(t, true)
	tok := token(t, st)
	sum := sha256.Sum256([]byte("hello"))
	up.mu.Lock()
	up.digest = "sha-256=:" + base64.StdEncoding.EncodeToString(sum[:]) + ":"
	up.mu.Unlock()

	if rec := do(g, tok, "http://gw/raw/o/r/main/a.txt", nil); rec.Code != 200 {
		t.Fatalf("摘要一致应成功，得到 %d", rec.Code)
	}
	list := mustList(t, g)
	if len(list) != 1 || list[0].Verify != "unverified" {
		t.Fatalf("自报摘要不该升级成 verified，实际 %+v", list)
	}
}

func mustList(t *testing.T, g *Gateway) []struct {
	Key    string
	Verify string
} {
	t.Helper()
	es, err := g.st.CacheList(10)
	if err != nil {
		t.Fatalf("列缓存: %v", err)
	}
	out := make([]struct {
		Key    string
		Verify string
	}, 0, len(es))
	for _, e := range es {
		out = append(out, struct {
			Key    string
			Verify string
		}{e.Key, e.Verify})
	}
	return out
}
