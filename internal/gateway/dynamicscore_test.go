package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 3.5.18 18c gateway 测试：动态排序应用与样本采集分类。
// 冻结稿 §3.3 / §18c：动态只重排 M 内副本顺序，不改成员/M/预算；
// 达门槛才启用，未达/关闭/损坏一律固定顺序。

// seedDynamic 给指定 (host,sid) 写入 n 个样本，ok 为成功数、ttfb 为延迟。
func seedDynamic(t *testing.T, f *replicaFixtureT, host string, sid int64, n, ok int, ttfb int64) {
	t.Helper()
	active := f.st.ActiveMappingIdentities(time.Now())
	for i := 0; i < n; i++ {
		_, err := f.st.ApplyDynamicSample(store.DynamicSampleInput{
			RegistryHost: host, SourceID: sid, OK: i < ok,
			TTFBMeasurable: true, TTFBMS: ttfb, TS: int64(i + 1),
		}, active)
		if err != nil {
			t.Fatalf("seed %s/%d: %v", host, sid, err)
		}
	}
}

func setSortMode(t *testing.T, f *replicaFixtureT, mode string) {
	t.Helper()
	if err := f.st.SetBSortMode(mode, "admin", ""); err != nil {
		t.Fatalf("set sort_mode: %v", err)
	}
}

// 未达 50 样本门槛：即使 sort_mode=dynamic 也按固定顺序（可信映射 priority）。
func TestB18DynamicBelowThresholdUsesFixed(t *testing.T) {
	good := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	f := newReplicaFixture(t, good, good)
	setSortMode(t, f, "dynamic")
	// 只给 10 个样本（未达 50）
	seedDynamic(t, f, "quay.io", f.srcID, 10, 10, 50)
	if f.gw.dynamicSortReady(time.Now()) {
		t.Fatal("样本不足 50 不应启用动态排序")
	}
}

// sort_mode=fixed：即使 50 样本达标也不启用动态排序。
func TestB18DynamicDisabledUsesFixed(t *testing.T) {
	good := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	f := newReplicaFixture(t, good, good)
	setSortMode(t, f, "fixed")
	seedDynamic(t, f, "quay.io", f.srcID, 50, 50, 50)
	if f.gw.dynamicSortReady(time.Now()) {
		t.Fatal("sort_mode=fixed 不应启用动态排序")
	}
}

// 达门槛：dynamicSortReady=true，且 applyDynamicOrder 按分数排序、不动官方站。
func TestB18DynamicReadyAndOrder(t *testing.T) {
	good := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	f := newReplicaFixture(t, good, good)
	setSortMode(t, f, "dynamic")
	// 第二个副本（用 foreign Host，避开 Hub/GHCR 的 CommitHostProbe 限制）
	id2 := f.addReplica(t, "registry.gitlab.com", "mirror/kube-apiserver", good)
	// 给两条映射各 50 样本：quay 低分（低成功率+高延迟）、gitlab 高分
	seedDynamic(t, f, "quay.io", f.srcID, 50, 10, 700)
	active := f.st.ActiveMappingIdentities(time.Now())
	for i := 0; i < 50; i++ {
		_, err := f.st.ApplyDynamicSample(store.DynamicSampleInput{
			RegistryHost: "registry.gitlab.com", SourceID: id2, OK: true,
			TTFBMeasurable: true, TTFBMS: 20, TS: int64(i + 1),
		}, active)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !f.gw.dynamicSortReady(time.Now()) {
		t.Fatal("两条映射都达 50 样本、sort_mode=dynamic 应启用动态排序")
	}
	// 构造 targets：官方在 0，后面两个副本按固定顺序 quay, ghcr
	f.plan.Ref = "latest"
	targets, _, err := f.gw.replicaTargets(context.Background(), f.plan, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) < 3 {
		t.Fatalf("应有官方+2副本，实际 %d", len(targets))
	}
	if targets[0].sourceID != 0 {
		t.Fatal("官方站必须固定在下标 0")
	}
	// 动态排序后：高分 gitlab 应排在低分 quay 之前
	ghcrBeforeQuay := false
	ghcrPos, quayPos := -1, -1
	for i, tg := range targets {
		if tg.sourceID == id2 && tg.registry == "registry.gitlab.com" {
			ghcrPos = i
		}
		if tg.sourceID == f.srcID && tg.registry == "quay.io" {
			quayPos = i
		}
	}
	ghcrBeforeQuay = ghcrPos >= 0 && quayPos >= 0 && ghcrPos < quayPos
	if !ghcrBeforeQuay {
		t.Fatalf("高分 gitlab 应排在低分 quay 之前: ghcr@%d quay@%d", ghcrPos, quayPos)
	}
}

// 样本采集：一次成功回源产生一条成功样本（OK=true，TTFB 可测）。
func TestB18SampleCollectedOnSuccess(t *testing.T) {
	good := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	f := newReplicaFixture(t, good, good)
	// 构造一个副本 target
	targets, _, err := f.gw.replicaTargets(context.Background(), f.plan, time.Now())
	if err != nil || len(targets) < 2 {
		t.Fatalf("候选解析: %v n=%d", err, len(targets))
	}
	var replica *replicaTarget
	for i := range targets {
		if targets[i].sourceID == f.srcID {
			replica = &targets[i]
			break
		}
	}
	if replica == nil {
		t.Fatal("未找到副本 target")
	}
	_, _, err = f.gw.fetchReplicaArtifact(context.Background(), f.plan, *replica, "registry.k8s.io")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	st, err := f.st.LoadDynamicState()
	if err != nil {
		t.Fatalf("状态: %v", err)
	}
	found := false
	for _, m := range st.Mappings {
		if m.RegistryHost == replica.registry && m.SourceID == replica.sourceID && len(m.Samples) == 1 {
			if m.Samples[0].OK && m.Samples[0].TTFBMeasurable {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("成功回源应产生一条成功可测样本: %+v", st.Mappings)
	}
}

// 样本采集：官方权威（sourceID=0）不产生样本，避免污染映射统计。
func TestB18OfficialNoSample(t *testing.T) {
	good := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	f := newReplicaFixture(t, good, good)
	targets, _, err := f.gw.replicaTargets(context.Background(), f.plan, time.Now())
	if err != nil || len(targets) < 1 {
		t.Fatalf("候选解析: %v", err)
	}
	official := targets[0]
	if official.sourceID != 0 {
		t.Fatal("targets[0] 应为官方")
	}
	f.gw.recordReplicaSample(store.DynamicSampleInput{
		RegistryHost: official.registry, SourceID: 0, OK: true, TTFBMeasurable: true, TTFBMS: 10,
	})
	st, err := f.st.LoadDynamicState()
	if err == nil && len(st.Mappings) != 0 {
		t.Fatalf("官方样本不应落库: %+v", st.Mappings)
	}
}
