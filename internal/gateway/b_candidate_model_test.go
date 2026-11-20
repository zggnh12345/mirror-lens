package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 3.5.18 18b 反向测试：候选来源模型按派发模式分离。
// 冻结稿 §10 反向测试 1/2/3/4：sequential 候选来自可信映射、不受参选集合限制；
// race + 空参选退化为按可信映射顺序回退，但仍执行安全门禁。
// 这些测试在修正 replicaTargets 之前必须失败（RED）。

// setReplicaMode 通过 settings + 即时失效切换模式（与生产 API 同路径）。
func setReplicaMode(t *testing.T, f *replicaFixtureT, mode string) {
	t.Helper()
	if err := f.st.SetCrossRegistryMode(mode, "admin", ""); err != nil {
		t.Fatalf("设置副本模式: %v", err)
	}
	f.gw.InvalidateReplicaMode()
	if got := f.gw.replicaMode(); got != mode {
		t.Fatalf("模式应即时生效为 %s，实际 %s", mode, got)
	}
}

// 反向测试 1：B 顺序 + 参选集合为空 + 有可信映射 → 必须能请求下一 Registry。
func TestB18SequentialIgnoresEmptySelection(t *testing.T) {
	good := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	bad := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[{"digest":"sha256:evil"}]}`)
	// official 返回坏内容、replica 返回好内容：用 good 的 Digest 请求时，
	// sequential 必须先跳过 official 再请求副本，才能证明空参选不阻断。
	f := newReplicaFixture(t, bad, good)
	sum := sha256.Sum256(good)
	f.plan.Ref = "sha256:" + hex.EncodeToString(sum[:])
	f.plan.ExpectSHA = hex.EncodeToString(sum[:])
	f.plan.Immutable = true
	f.plan.Upstream.Path = "/v2/kube-apiserver/manifests/" + f.plan.Ref
	setReplicaMode(t, f, "sequential")
	if err := f.st.SetCrossSelectedSources(nil, "admin", ""); err != nil {
		t.Fatalf("清空参选: %v", err)
	}
	ts, _, err := f.gw.replicaTargets(context.Background(), f.plan, time.Now())
	if err != nil {
		t.Fatalf("候选解析: %v", err)
	}
	// 权威官方 + 至少 1 个来自可信映射的副本；空参选不得阻断顺序候选。
	if len(ts) < 2 {
		t.Fatalf("sequential + 空参选 + 有可信映射应产生副本候选，实际 %d 个", len(ts))
	}
	foundReplica := false
	for _, c := range ts {
		if c.sourceID == f.srcID {
			foundReplica = true
		}
	}
	if !foundReplica {
		t.Fatal("顺序候选必须来自可信映射（quay.io 副本源）")
	}
	// 端到端：确实请求了副本，不是退化为本地路径就结束。
	if _, handled, _ := f.gw.crossRegistryManifest(f.ctx(), f.plan, nil); !handled {
		t.Fatal("sequential + 有可信映射应由 B 处理，而非退回本地路径")
	}
	if f.replica.Load() == 0 {
		t.Fatal("sequential + 空参选必须实际请求下一 Registry")
	}
}

// 反向测试 2：B 竞速 + 参选集合为空 → 不启动跨站竞速，退化为按可信映射顺序回退。
func TestB18RaceEmptySelectionDegradesToSequential(t *testing.T) {
	good := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	bad := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[{"digest":"sha256:evil"}]}`)
	f := newReplicaFixture(t, bad, good)
	sum := sha256.Sum256(good)
	f.plan.Ref = "sha256:" + hex.EncodeToString(sum[:])
	f.plan.ExpectSHA = hex.EncodeToString(sum[:])
	f.plan.Immutable = true
	f.plan.Upstream.Path = "/v2/kube-apiserver/manifests/" + f.plan.Ref
	setReplicaMode(t, f, "race")
	if err := f.st.SetCrossSelectedSources(nil, "admin", ""); err != nil {
		t.Fatalf("清空参选: %v", err)
	}
	// 退化到 sequential 后仍能从可信映射发现副本并请求（不是绕过门禁的裸回退）。
	_, handled, err := f.gw.crossRegistryManifest(f.ctx(), f.plan, nil)
	if err != nil {
		t.Fatalf("跨站处理: %v", err)
	}
	if !handled || f.replica.Load() == 0 {
		t.Fatalf("race + 空参选应退化为顺序回退并请求副本，handled=%v replica=%d", handled, f.replica.Load())
	}
}

// 反向测试 4：race + 空参选退化到顺序回退时，仍执行 B.3 安全门禁
// （三方 Digest 校验），坏内容不得被当成有效胜者采纳——不是绕过门禁的裸回退。
func TestB18DegradedStillEnforcesDigestGate(t *testing.T) {
	good := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	badOfficial := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[{"digest":"sha256:official-evil"}]}`)
	badReplica := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[{"digest":"sha256:replica-evil"}]}`)
	f := newReplicaFixture(t, badOfficial, badReplica)
	sum := sha256.Sum256(good)
	f.plan.Ref = "sha256:" + hex.EncodeToString(sum[:])
	f.plan.ExpectSHA = hex.EncodeToString(sum[:])
	f.plan.Immutable = true
	f.plan.Upstream.Path = "/v2/kube-apiserver/manifests/" + f.plan.Ref
	setReplicaMode(t, f, "race")
	if err := f.st.SetCrossSelectedSources(nil, "admin", ""); err != nil {
		t.Fatalf("清空参选: %v", err)
	}
	e, handled, err := f.gw.crossRegistryManifest(f.ctx(), f.plan, nil)
	if err != nil {
		t.Fatalf("跨站处理: %v", err)
	}
	if handled && e != nil && (e.SHA256 == shaOf(badOfficial) || e.SHA256 == shaOf(badReplica)) {
		t.Fatal("race 空参选退化后仍不得采纳 Digest 不符的坏内容")
	}
	// 两个候选都坏：不能形成有效胜者（handled=false 或返回的不是坏内容）。
	if handled && e == nil {
		t.Fatal("退化路径不得在没有有效正文时声称已处理")
	}
}

// 反向测试 3：B 顺序 + 参选集合非空 → 参选内容变化不改变顺序候选集。
func TestB18SequentialSelectionChangesDoNotAlterCandidates(t *testing.T) {
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	f := newReplicaFixture(t, body, body)
	setReplicaMode(t, f, "sequential")
	// 参选集合里放一个与副本无关的源，且不含副本源。
	other, err := f.st.AddSource(store.Source{BaseURL: "https://example.invalid", Name: "unrelated", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatalf("加源: %v", err)
	}
	starred := true
	if err := f.st.UpdateSource(other, nil, nil, nil, &starred); err != nil {
		t.Fatalf("加星标: %v", err)
	}
	if err := f.st.SetCrossSelectedSources([]int64{other}, "admin", ""); err != nil {
		t.Fatalf("设置参选: %v", err)
	}
	ts, _, err := f.gw.replicaTargets(context.Background(), f.plan, time.Now())
	if err != nil {
		t.Fatalf("候选解析: %v", err)
	}
	foundReplica := false
	for _, c := range ts {
		if c.sourceID == f.srcID {
			foundReplica = true
		}
	}
	if !foundReplica {
		t.Fatal("sequential 候选不得依赖参选勾选：参选里没有副本源时仍应能从可信映射发现它")
	}
}
