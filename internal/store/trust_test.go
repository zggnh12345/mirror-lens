package store

import (
	"testing"
	"time"
)

func check(t *testing.T, st *Store, id int64, kind string, ok bool, digest, blob int, age time.Duration) {
	t.Helper()
	if err := st.AddCheck(Check{
		SourceID: id, Kind: kind, Tier: 3, OK: ok,
		LatencyMS: 100, DigestOK: digest, BlobOK: blob,
		TS: time.Now().Add(-age).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTrustLevels(t *testing.T) {
	st := openTest(t)
	id := addSrc(t, st)

	if tm, err := st.trustMap("docker", time.Now()); err != nil || tm[id].Level != TrustUnknown {
		t.Fatalf("没有任何检测时应为未验证，实际 %+v %v", tm[id], err)
	}

	// 能通，但从没校验过内容
	check(t, st, id, "docker", true, -1, -1, 0)
	if tm, _ := st.trustMap("docker", time.Now()); tm[id].Level != TrustReachable {
		t.Fatalf("只成功未校验应为仅可用，实际 %d", tm[id].Level)
	}

	// 校验通过一次
	check(t, st, id, "docker", true, 1, -1, 0)
	if tm, _ := st.trustMap("docker", time.Now()); tm[id].Level != TrustVerified {
		t.Fatalf("一次校验通过应为内容一致，实际 %d", tm[id].Level)
	}

	// 攒到 5 次校验通过
	for i := 0; i < 4; i++ {
		check(t, st, id, "docker", true, 1, 1, 0)
	}
	if tm, _ := st.trustMap("docker", time.Now()); tm[id].Level != TrustProven {
		t.Fatalf("5 次校验通过应为长期一致，实际 %d", tm[id].Level)
	}

	// 一次不符就把信任打到最低，哪怕之前攒了一百次
	check(t, st, id, "docker", true, 1, 0, 0)
	if tm, _ := st.trustMap("docker", time.Now()); tm[id].Level != TrustPoisoned {
		t.Fatalf("出现字节不符应为内容不符，实际 %d", tm[id].Level)
	}
}

// 窗口是 7 天：更早的记录不该继续给信任，也不该继续罚它。
func TestTrustWindow(t *testing.T) {
	st := openTest(t)
	id := addSrc(t, st)
	for i := 0; i < 6; i++ {
		check(t, st, id, "docker", true, 1, 1, 8*24*time.Hour)
	}
	if tm, _ := st.trustMap("docker", time.Now()); tm[id].Level != TrustUnknown {
		t.Fatalf("8 天前的记录不该算进可信度，实际 %d", tm[id].Level)
	}
}

func TestListSourcesCarriesTrust(t *testing.T) {
	st := openTest(t)
	id := addSrc(t, st)
	check(t, st, id, "docker", true, 1, 1, 0)

	list, err := st.ListSources()
	if err != nil {
		t.Fatal(err)
	}
	if got := list[0].Trust["docker"]; got.Level != TrustVerified || got.Digest7 != 1 {
		t.Fatalf("列表没带上可信度：%+v", got)
	}
	if got := list[0].Trust["github"]; got.Level != TrustUnknown {
		t.Fatalf("没有 github 检测时应为未验证，实际 %d", got.Level)
	}
}
