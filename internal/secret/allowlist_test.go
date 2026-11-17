package secret

import "testing"

func TestHostProfileFor(t *testing.T) {
	for _, h := range []string{"gcr.io", "quay.io", "QUAY.IO"} {
		p, ok := HostProfileFor(h)
		if !ok {
			t.Fatalf("%s 应有 HostProfile", h)
		}
		if len(p.Modes) == 0 {
			t.Fatalf("%s 的 Modes 不能为空", h)
		}
	}
	for _, h := range []string{"example.com", "registry.k8s.io", "asia-east1-docker.pkg.dev", "localhost"} {
		if _, ok := HostProfileFor(h); ok {
			t.Fatalf("%s 不应有凭据 HostProfile", h)
		}
	}
}

func TestHostProfilesDeepCopy(t *testing.T) {
	first := HostProfiles()
	if len(first) == 0 {
		t.Fatal("HostProfiles 不能为空")
	}
	first[0].Modes[0] = ModeBasic
	first[0].Host = "mutated.example"
	second := HostProfiles()
	if second[0].Host == "mutated.example" {
		t.Fatal("HostProfiles 返回了内部切片")
	}
}
