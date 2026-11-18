package store

import (
	"path/filepath"
	"testing"
)

func openRaceStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "race.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// T07：Host 归一化后同一个键，缺省一律关闭。
func TestStarRaceKeyNormalize(t *testing.T) {
	cases := map[string]string{
		"docker.io":            "registry_star_race:docker.io",
		"registry-1.docker.io": "registry_star_race:docker.io",
		"index.docker.io":      "registry_star_race:docker.io",
		"QUAY.IO":              "registry_star_race:quay.io",
	}
	for in, want := range cases {
		got, err := StarRaceKey(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got != want {
			t.Fatalf("%s → %s，期望 %s", in, got, want)
		}
	}
	if _, err := StarRaceKey(""); err == nil {
		t.Fatal("空 Host 应报错")
	}
	if _, err := StarRaceKey("bad host/../x"); err == nil {
		t.Fatal("非法 Host 应报错")
	}
}

func TestStarRaceDefaultOff(t *testing.T) {
	st := openRaceStore(t)
	if st.StarRaceEnabled("quay.io") {
		t.Fatal("缺省必须关闭")
	}
	if len(st.StarRaceHosts()) != 0 {
		t.Fatal("缺省不应有开启的 Host")
	}
}

// 只认显式 "1"：任何其他值（"true"/"yes"/"0"）都不算开启。
func TestStarRaceOnlyExplicitOne(t *testing.T) {
	st := openRaceStore(t)
	key, _ := StarRaceKey("quay.io")
	for _, v := range []string{"true", "yes", "on", "0", ""} {
		if err := st.SetSetting(key, v); err != nil {
			t.Fatalf("SetSetting: %v", err)
		}
		if st.StarRaceEnabled("quay.io") {
			t.Fatalf("值 %q 不应算开启", v)
		}
	}
	if err := st.SetSetting(key, "1"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if !st.StarRaceEnabled("quay.io") {
		t.Fatal("显式 1 应开启")
	}
	if hosts := st.StarRaceHosts(); !hosts["quay.io"] || len(hosts) != 1 {
		t.Fatalf("Host 列表不对：%v", hosts)
	}
}

func TestSetStarRaceRoundTrip(t *testing.T) {
	st := openRaceStore(t)
	if err := st.SetStarRace("registry-1.docker.io", true); err != nil {
		t.Fatalf("SetStarRace: %v", err)
	}
	if !st.StarRaceEnabled("docker.io") {
		t.Fatal("别名应读到同一个开关")
	}
	if err := st.SetStarRace("docker.io", false); err != nil {
		t.Fatalf("SetStarRace: %v", err)
	}
	if st.StarRaceEnabled("registry-1.docker.io") {
		t.Fatal("关闭后别名也应为关")
	}
	if err := st.SetStarRace("bad host", true); err == nil {
		t.Fatal("非法 Host 应拒绝")
	}
}
