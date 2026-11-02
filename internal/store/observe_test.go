package store

import (
	"path/filepath"
	"testing"
)

// 观察池只看最近一次 L2 之后的 L1：L2 落库即清零，避免老记录一直凑数。
func TestObserveStreak(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	id, err := st.AddSource(Source{
		Name: "m", BaseURL: "https://m.example", Status: "pending", Enabled: true,
		DockerCap: CapUnknown, GithubCap: CapUnknown,
	})
	if err != nil {
		t.Fatal(err)
	}

	add := func(tier int, ok bool) {
		if err := st.AddCheck(Check{SourceID: id, Kind: "docker", Tier: tier, OK: ok}); err != nil {
			t.Fatal(err)
		}
	}

	add(1, true)
	add(1, true)
	if pass, fail := st.ObserveStreak(id, "docker"); pass != 2 || fail != 0 {
		t.Fatalf("两次 L1 通过应得 2/0，实际 %d/%d", pass, fail)
	}

	add(2, true)
	if pass, fail := st.ObserveStreak(id, "docker"); pass != 0 || fail != 0 {
		t.Fatalf("L2 落库后应清零，实际 %d/%d", pass, fail)
	}

	add(1, false)
	add(1, false)
	if pass, fail := st.ObserveStreak(id, "docker"); pass != 0 || fail != 2 {
		t.Fatalf("连续失败应得 0/2，实际 %d/%d", pass, fail)
	}

	add(1, true)
	if pass, fail := st.ObserveStreak(id, "docker"); pass != 1 || fail != 0 {
		t.Fatalf("失败后一次通过应从头计数，实际 %d/%d", pass, fail)
	}
}
