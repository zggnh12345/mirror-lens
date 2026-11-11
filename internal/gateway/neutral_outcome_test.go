package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// 404/410 是中性结果：照旧换源到官方，但镜像不能因此进冷却，
// 也不能在事件里被记成故障。
func TestHeadMissLeavesSourceUncooled(t *testing.T) {
	f := newMOFixture(t)
	miss := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer miss.Close()
	regID := f.addRegistrySource(t, miss, "native")

	w := f.head(t, "/v2/quay.io/library/alpine/blobs/sha256:"+headHex)
	if w.Code != http.StatusOK {
		t.Fatalf("镜像 404 后应回退官方并拿到 200，实际 %d", w.Code)
	}
	if f.officialHits.Load() != 1 {
		t.Fatalf("官方应被访问 1 次，实际 %d", f.officialHits.Load())
	}

	ev := eventsOf(t, f.dbPath, regID)
	if len(ev) != 1 || ev[0].Code != 404 || ev[0].OK != 1 || ev[0].Kind != "registry:quay.io" {
		t.Fatalf("应留一条 code=404/ok=1/registry:quay.io 的事件，实际 %+v", ev)
	}

	if id := f.gw.sched.StickyFor("registry:quay.io"); id != 0 {
		t.Fatalf("404 不该建立粘性，实际粘到源 %d", id)
	}

	// 冷却中的源会被候选筛选排除——它还在候选里，说明 404 没被当成故障。
	var found bool
	for _, c := range f.gw.sched.Candidates("registry:quay.io") {
		if c.ID == regID {
			found = true
		}
	}
	if !found {
		t.Fatal("404 不应让镜像进冷却（候选里已找不到它）")
	}
}

// 401/403 同样是中性结果：透传挑战、不换源、不冷却、事件照留。
func TestHeadAuthLeavesSourceUncooled(t *testing.T) {
	f := newMOFixture(t)
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="https://quay.io/v2/auth",service="quay.io"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer deny.Close()
	regID := f.addRegistrySource(t, deny, "native")

	w := f.head(t, "/v2/quay.io/library/alpine/blobs/sha256:"+headHex)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("401 应原样透传，实际 %d", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); got == "" {
		t.Fatal("挑战头必须透传，否则客户端无法认证")
	}
	if f.officialHits.Load() != 0 {
		t.Fatalf("认证失败不应去官方重试，实际官方命中 %d 次", f.officialHits.Load())
	}

	ev := eventsOf(t, f.dbPath, regID)
	if len(ev) != 1 || ev[0].Code != 401 || ev[0].OK != 1 || ev[0].Kind != "registry:quay.io" {
		t.Fatalf("应留一条 code=401/ok=1/registry:quay.io 的事件，实际 %+v", ev)
	}

	if id := f.gw.sched.StickyFor("registry:quay.io"); id != 0 {
		t.Fatalf("401 不该建立粘性，实际粘到源 %d", id)
	}

	var found bool
	for _, c := range f.gw.sched.Candidates("registry:quay.io") {
		if c.ID == regID {
			found = true
		}
	}
	if !found {
		t.Fatal("401 不应让镜像进冷却（候选里已找不到它）")
	}
	// 顺带确认协议族隔离：docker 族没有因为这些 registry 结果冒出新候选。
	for _, c := range f.gw.sched.Candidates("docker") {
		if c.ID == regID {
			t.Fatal("registry 结果污染了 docker 族候选")
		}
	}
}
