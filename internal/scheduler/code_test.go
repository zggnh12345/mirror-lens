package scheduler

import (
	"testing"
	"time"
)

// Hint.Code 要跟着事件落库：429 率靠它算，丢了这个字段就只能看到「失败」，
// 分不清是「被限流」还是「坏了」。
func TestReportKeepsStatusCode(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://m.example", true, 100, 900)
	s := New(st, Config{Enabled: true, MaxCandidates: 3})

	s.Report("docker", Candidate{ID: id, Name: "m"}, false, 120*time.Millisecond, Hint{Code: 429})

	codes, err := st.SourceEventCodes(id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != 1 || codes[0] != 429 {
		t.Fatalf("事件里应记下 429，实际 %v", codes)
	}
}

// 没有状态码时记 0，不能把连接失败当成某个 HTTP 码。
func TestReportWithoutCodeStoresZero(t *testing.T) {
	st := newStore(t)
	id := addSource(t, st, "https://m.example", true, 100, 900)
	s := New(st, Config{Enabled: true, MaxCandidates: 3})

	s.Report("docker", Candidate{ID: id, Name: "m"}, false, time.Second)

	codes, err := st.SourceEventCodes(id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != 1 || codes[0] != 0 {
		t.Fatalf("无状态码应记 0，实际 %v", codes)
	}
}
