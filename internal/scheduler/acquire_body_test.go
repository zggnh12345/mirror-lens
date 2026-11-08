package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// observe 模式的语义是「保留旧 gate 准入 + 影子 Lease 观察」，
// AcquireBody 也必须走同一条路，不能因为要拿 Lease 就绕过闸门。
func TestAcquireBodyObserveKeepsGate(t *testing.T) {
	st := newStore(t)
	id, err := st.AddSource(store.Source{
		Name: "镜像A", BaseURL: "http://mirror", Status: "usable",
		Enabled: true, DockerCap: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := New(st, Config{Enabled: true, MaxPerSource: 4, Admission: "observe"})
	s.gateFor(id).setLimit(1)

	l1, _, done1, err := s.AcquireBody(context.Background(), id)
	if err != nil || l1 == nil {
		t.Fatalf("第一个请求应拿到名额: l=%v err=%v", l1, err)
	}

	second := make(chan struct{})
	go func() {
		l2, _, done2, err := s.AcquireBody(context.Background(), id)
		if err == nil {
			done2()
			_ = l2
		}
		close(second)
	}()

	select {
	case <-second:
		t.Fatal("observe 模式下第二个请求没经过旧 gate 限流")
	case <-time.After(200 * time.Millisecond):
	}
	done1()
	select {
	case <-second:
	case <-time.After(3 * time.Second):
		t.Fatal("释放闸门后第二个请求仍未放行")
	}
}
