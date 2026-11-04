package registryadapter

import "testing"

// docker 拉 attestation manifest 时会把 digest 写成 sha256-<hex> 的 tag 形式，
// 直接透传上游按 tag 查会 404。
func TestRouteNormalizesDashDigest(t *testing.T) {
	a := &Adapter{}
	hex := "bf8dcc6243cf381edb29ca14d230306dd8f6da4cf9c008968ec068995c50dc7a"

	kind, repo, ref, err := a.Route("/v2/adguard/adguardhome/manifests/sha256-" + hex)
	if err != nil {
		t.Fatalf("Route 报错: %v", err)
	}
	if kind != KindManifest || repo != "adguard/adguardhome" || ref != "sha256:"+hex {
		t.Fatalf("got kind=%v repo=%q ref=%q", kind, repo, ref)
	}

	// 正常 tag 不受影响
	if _, _, ref, err := a.Route("/v2/library/alpine/manifests/latest"); err != nil || ref != "latest" {
		t.Fatalf("tag 被改写: ref=%q err=%v", ref, err)
	}
	// 长度不对的连字符串不能当 digest
	if _, _, ref, _ := a.Route("/v2/library/alpine/manifests/sha256-abc"); ref != "sha256-abc" {
		t.Fatalf("短串不应改写: %q", ref)
	}
}
