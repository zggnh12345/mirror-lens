package unified

import (
	"errors"
	"testing"
)

// 内容协商必须进入 Claim 身份：同 Ref 不同 Accept 不能等价。

func TestNormalizeAcceptKeepsQAndOrder(t *testing.T) {
	in := []string{
		"Application/VND.OCI.image.index.v1+json; q=0.9 ;foo=Bar, " +
			"application/vnd.docker.distribution.manifest.v2+json;q=1.0",
	}
	got := NormalizeAccept(in)
	want := "application/vnd.oci.image.index.v1+json;foo=Bar;q=0.9," +
		"application/vnd.docker.distribution.manifest.v2+json"
	if got != want {
		t.Fatalf("规范化结果\n got=%q\nwant=%q", got, want)
	}
}

func TestNormalizeAcceptInsensitiveToWhitespaceCaseAndParamOrder(t *testing.T) {
	a := NormalizeAccept([]string{"application/json; q=0.8; profile=\"x\" , text/plain"})
	b := NormalizeAccept([]string{"  APPLICATION/JSON ;profile=\"x\";q=0.80,text/plain "})
	if a != b {
		t.Fatalf("无语义差异被当成不同身份：%q vs %q", a, b)
	}
}

func TestNormalizeAcceptDistinguishesQAndOrder(t *testing.T) {
	base := NormalizeAccept([]string{"application/json, text/plain;q=0.5"})
	if got := NormalizeAccept([]string{"application/json, text/plain;q=0.4"}); got == base {
		t.Fatalf("q 值不同却等价：%q", got)
	}
	// 顺序是优先级语义：交换两项不是同一内容协商。
	if got := NormalizeAccept([]string{"text/plain;q=0.5, application/json"}); got == base {
		t.Fatalf("顺序不同却等价：%q", got)
	}
}

func TestClaimIdentityIsolatesManifestAccept(t *testing.T) {
	s := newTestScope(t)
	defer s.Close()

	ref := "sha256:abc"
	idA := Identity{
		Registry:   "ghcr.io",
		Repository: "acme/app",
		Manifest:   NormalizeManifestIdentity(ref, []string{"application/vnd.oci.image.index.v1+json"}),
	}
	idB := Identity{
		Registry:   "ghcr.io",
		Repository: "acme/app",
		Manifest:   NormalizeManifestIdentity(ref, []string{"application/vnd.docker.distribution.manifest.v2+json"}),
	}
	if idA == idB {
		t.Fatal("同 Ref 不同 Accept 被当成同一身份")
	}

	a, _ := s.NewChain("A", ChainA)
	b, _ := s.NewChain("B", ChainB)
	h := mustClaim(t, s, a, idA)
	att := mustAttempt(t, a, h)
	if err := att.Dispatch(); err != nil {
		t.Fatal(err)
	}

	// 不同 Accept：另一条链必须能独立取得自己的 Claim。
	h2, err := s.Claim(b.ID(), idB)
	if err != nil {
		t.Fatalf("不同内容协商被错误抑制：%v", err)
	}
	if h2.Identity() != idB {
		t.Fatalf("取得的身份 = %v，期望 %v", h2.Identity(), idB)
	}

	// 同 Accept 的等价规范化串：仍然冲突（同一身份）。
	again := Identity{
		Registry:   "ghcr.io",
		Repository: "acme/app",
		Manifest:   NormalizeManifestIdentity(ref, []string{"APPLICATION/VND.OCI.IMAGE.INDEX.V1+JSON;q=1.0"}),
	}
	if _, err := s.Claim(b.ID(), again); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("等价规范化串应冲突，err=%v", err)
	}
}

func TestIdentityEmptyRequiresRef(t *testing.T) {
	if !(Identity{Registry: "ghcr.io", Repository: "a/b"}).Empty() {
		t.Fatal("缺 Ref 的身份应判为空")
	}
	if (Identity{Registry: "ghcr.io", Repository: "a/b", Manifest: ManifestIdentity{Ref: "latest"}}).Empty() {
		t.Fatal("完整身份被判为空")
	}
}
