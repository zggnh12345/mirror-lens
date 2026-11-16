package store

import (
	"errors"
	"testing"
)

func TestSourceHostCapsBackfillAndProjection(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{
		BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io",
		DockerCap: CapYes, GhcrCap: CapYes, Status: "usable", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	hosts, err := st.GetSourceHosts(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 3 {
		t.Fatalf("期望 docker/ghcr/quay 三行，实际 %+v", hosts)
	}
	var n int
	if err := st.db.QueryRow(`SELECT count(*) FROM source_realm_policy WHERE source_id=?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("期望三个 Host 都有策略，实际 %d", n)
	}

	// SetCapsLive 同时更新 caps 和旧投影；14b 仍需要旧投影用于回滚。
	ok, err := st.SetCapsLive(id, CapNo, CapYes, CapYes, true)
	if err != nil || !ok {
		t.Fatalf("SetCapsLive=%v,%v", ok, err)
	}
	var docker, ghcr string
	var prefix int
	if err := st.db.QueryRow(`SELECT
		(SELECT effective_status FROM source_host_caps WHERE source_id=? AND host='docker.io'),
		(SELECT effective_status FROM source_host_caps WHERE source_id=? AND host='ghcr.io'),
		(SELECT registry_prefix FROM source_host_caps WHERE source_id=? AND host='ghcr.io')`,
		id, id, id).Scan(&docker, &ghcr, &prefix); err != nil {
		t.Fatal(err)
	}
	if docker != HostStatusUnsupported || ghcr != HostStatusSupported || prefix != 1 {
		t.Fatalf("caps 投影不对: docker=%s ghcr=%s prefix=%d", docker, ghcr, prefix)
	}
	d, err := st.GetSourceHost(id, "docker.io")
	if err != nil {
		t.Fatal(err)
	}
	if d.EffectiveStatus != HostStatusUnsupported || d.LastVerdict != HostVerdictUnsupported {
		t.Fatalf("docker caps 不对: %+v", d)
	}
	g, _ := st.GetSourceHost(id, "ghcr.io")
	if g.EffectiveStatus != HostStatusSupported || !g.RegistryPrefix {
		t.Fatalf("ghcr caps 不对: %+v", g)
	}
}

func TestNewForeignHostProbeDoesNotChangeRepoHostProjection(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{
		BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io",
		RegistryCap: RegistryCapYes, RegistryPath: PathNative, Status: "usable", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddSourceHost(id, "registry.k8s.io", "https://registry.k8s.io", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CommitHostProbe(HostProbeCommit{
		SourceID: id, Host: "registry.k8s.io", Cap: RegistryCapYes, PathMode: PathNative,
		At: 100, Gen: 0,
	}); err != nil {
		t.Fatal(err)
	}
	q, err := st.GetSourceHost(id, "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if q.EffectiveStatus != HostStatusSupported || q.RegistryPathMode != PathNative {
		t.Fatalf("新 Host 探测改了原 quay caps: %+v", q)
	}
	k, err := st.GetSourceHost(id, "registry.k8s.io")
	if err != nil {
		t.Fatal(err)
	}
	if k.EffectiveStatus != HostStatusSupported || k.RegistryPathMode != PathNative {
		t.Fatalf("新 Host caps 不对: %+v", k)
	}
}

func TestTransportErrorKeepsEffectiveStatus(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{
		BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io",
		RegistryCap: RegistryCapYes, RegistryPath: PathNative, Status: "usable", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CommitHostProbe(HostProbeCommit{
		SourceID: id, Host: "quay.io", Cap: RegistryCapUnknown, PathMode: "",
		Reason: probeReasonTransportError, At: 200, ErrText: "timeout",
	}); err != nil {
		t.Fatal(err)
	}
	h, err := st.GetSourceHost(id, "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if h.LastVerdict != HostVerdictInconclusive || h.EffectiveStatus != HostStatusSupported {
		t.Fatalf("传输错误应保留 Supported: %+v", h)
	}
	var effective, path string
	if err := st.db.QueryRow(`SELECT effective_status,registry_path_mode FROM source_host_caps
		WHERE source_id=? AND host='quay.io'`, id).Scan(&effective, &path); err != nil {
		t.Fatal(err)
	}
	if effective != HostStatusSupported || path != PathNative {
		t.Fatalf("传输错误不应改 caps: effective=%s path=%q", effective, path)
	}
}

func TestHostEnableDisableIsPerHost(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{
		BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io",
		DockerCap: CapYes, GhcrCap: CapYes, Status: "usable", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSourceHostEnabled(id, "ghcr.io", false); err != nil {
		t.Fatal(err)
	}
	d, _ := st.GetSourceHost(id, "docker.io")
	g, _ := st.GetSourceHost(id, "ghcr.io")
	if d.EffectiveStatus != HostStatusSupported || g.Enabled {
		t.Fatalf("只应停 ghcr: docker=%+v ghcr=%+v", d, g)
	}
	if err := st.SetSourceHostEnabled(id, "ghcr.io", true); err != nil {
		t.Fatal(err)
	}
	d, _ = st.GetSourceHost(id, "docker.io")
	g, _ = st.GetSourceHost(id, "ghcr.io")
	if d.EffectiveStatus != HostStatusSupported || !g.Enabled {
		t.Fatalf("恢复 ghcr 不对: docker=%+v ghcr=%+v", d, g)
	}
}

func TestRemoveHostDeletesPolicyAndRecovery(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{
		BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io",
		RegistryCap: RegistryCapYes, RegistryPath: PathNative, Status: "usable", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddSourceHost(id, "gcr.io", "https://gcr.io", nil); err != nil {
		t.Fatal(err)
	}
	if err := st.RemoveSourceHost(id, "gcr.io"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetSourceHost(id, "gcr.io"); err == nil {
		t.Fatal("gcr.io caps 应已删除")
	}
	var n int
	if err := st.db.QueryRow(`SELECT count(*) FROM source_realm_policy WHERE source_id=? AND host='gcr.io'`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("gcr.io policy 应已删除，实际 %d", n)
	}
	if err := st.db.QueryRow(`SELECT count(*) FROM registry_host_recovery WHERE source_id=? AND scope='gcr.io'`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("gcr.io recovery 应已删除，实际 %d", n)
	}
}

func TestResetMismatchToUnknown(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{
		BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io",
		RegistryCap: RegistryCapMismatch, Status: "usable", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ResetSourceHostVerdict(id, "quay.io"); err != nil {
		t.Fatal(err)
	}
	h, err := st.GetSourceHost(id, "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if h.EffectiveStatus != HostStatusUnknown || h.LastVerdict != HostVerdictMismatch {
		t.Fatalf("reset 应保留 last_verdict: %+v", h)
	}
}

func TestHostAdminRejectsMissingAndArchived(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RemoveSourceHost(id, "gcr.io"); !errors.Is(err, ErrHostCapsNotFound) {
		t.Fatalf("删除不存在的 Host 应返回 ErrHostCapsNotFound，实际 %v", err)
	}
	if err := st.ResetSourceHostVerdict(id, "gcr.io"); !errors.Is(err, ErrHostCapsNotFound) {
		t.Fatalf("重置不存在的 Host 应返回 ErrHostCapsNotFound，实际 %v", err)
	}
	if _, err := st.db.Exec(`UPDATE sources SET status='archived' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if err := st.ResetSourceHostVerdict(id, "quay.io"); !errors.Is(err, ErrArchived) {
		t.Fatalf("归档源重置应返回 ErrArchived，实际 %v", err)
	}
	if err := st.SetRealmPolicy(id, "quay.io", "https://quay.io", nil); !errors.Is(err, ErrArchived) {
		t.Fatalf("归档源改策略应返回 ErrArchived，实际 %v", err)
	}
}

func TestResetVerdictClearsScopedFailures(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO source_health_scope(source_id,family,scope,gen,fails,last_at)
		VALUES(?, 'registry', 'quay.io', 0, 3, 1)`, id); err != nil {
		t.Fatal(err)
	}
	if err := st.ResetSourceHostVerdict(id, "quay.io"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM source_health_scope WHERE source_id=? AND family='registry' AND scope='quay.io'`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("重置后 scoped 失败应清空，实际 %d 行", n)
	}
	h, err := st.GetSourceHost(id, "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if h.EffectiveStatus != HostStatusUnknown {
		t.Fatalf("重置后 effective=%s", h.EffectiveStatus)
	}
}

func TestAddSourceHostReenablesDisabled(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddSourceHost(id, "gcr.io", "https://gcr.io", nil); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSourceHostEnabled(id, "gcr.io", false); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSourceHost(id, "gcr.io", "https://gcr.io", nil); err != nil {
		t.Fatal(err)
	}
	h, err := st.GetSourceHost(id, "gcr.io")
	if err != nil {
		t.Fatal(err)
	}
	if !h.Enabled {
		t.Fatal("重新 AddSourceHost 应启用该 Host")
	}
}

func TestCommitHostProbeRejectsDisabledHost(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSourceHostEnabled(id, "quay.io", false); err != nil {
		t.Fatal(err)
	}
	res, err := st.CommitHostProbe(HostProbeCommit{SourceID: id, Host: "quay.io", Cap: RegistryCapYes, PathMode: PathNative, At: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res != HostCapsSourceUnavailable {
		t.Fatalf("停用 Host 的迟到探测应拒绝，实际 %v", res)
	}
}

func TestSetRealmPolicyInvalidatesCapability(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE source_host_caps SET enabled=1, effective_status='Supported', registry_path_mode='native'
		WHERE source_id=? AND host='quay.io'`, id); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRealmPolicy(id, "quay.io", "https://new-realm.example", nil); err != nil {
		t.Fatal(err)
	}
	h, err := st.GetSourceHost(id, "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if h.EffectiveStatus != HostStatusUnknown || !h.Enabled {
		t.Fatalf("策略变更后应 enabled/Unknown，实际 enabled=%v status=%s", h.Enabled, h.EffectiveStatus)
	}
}

func TestCommitHostProbeRejectsStalePolicyGen(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	var pg int64
	if err := st.db.QueryRow(`SELECT policy_gen FROM source_realm_policy WHERE source_id=? AND host='quay.io'`, id).Scan(&pg); err != nil {
		t.Fatal(err)
	}
	res, err := st.CommitHostProbe(HostProbeCommit{
		SourceID: id, Host: "quay.io", Cap: RegistryCapYes, PathMode: PathNative,
		At: 1, PolicyGen: pg, HasPolicyGen: true,
	})
	if err != nil || res != HostCapsCommitted {
		t.Fatalf("首次提交应成功，res=%v err=%v", res, err)
	}
	if err := st.SetRealmPolicy(id, "quay.io", "https://new-realm.example", nil); err != nil {
		t.Fatal(err)
	}
	res, err = st.CommitHostProbe(HostProbeCommit{
		SourceID: id, Host: "quay.io", Cap: RegistryCapYes, PathMode: PathNative,
		At: 2, PolicyGen: pg, HasPolicyGen: true,
	})
	if err != nil || res != HostCapsStaleSnapshot {
		t.Fatalf("旧策略代次应拒绝，res=%v err=%v", res, err)
	}
}

func TestSetCapsLiveDoesNotResurrectRemovedHost(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RemoveSourceHost(id, "docker.io"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetCapsLive(id, CapYes, CapYes, CapNo, false); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM source_host_caps WHERE source_id=? AND host='docker.io'`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("移除后的 docker.io 被 SetCapsLive 复活，行数=%d", n)
	}
}

func TestResetHostCapsSchedulesImmediateReprobe(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE source_host_caps SET effective_status='Supported', registry_path_mode='native'
		WHERE source_id=? AND host='quay.io'`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE registry_host_recovery SET state='ok', next_probe_at=9999999999
		WHERE source_id=? AND family='registry' AND scope='quay.io'`, id); err != nil {
		t.Fatal(err)
	}
	ok, err := st.ResetHostCaps(id, "quay.io", RegistryCapYes)
	if err != nil || !ok {
		t.Fatalf("ResetHostCaps=%v,%v", ok, err)
	}
	var state string
	var next int64
	if err := st.db.QueryRow(`SELECT state,next_probe_at FROM registry_host_recovery
		WHERE source_id=? AND family='registry' AND scope='quay.io'`, id).Scan(&state, &next); err != nil {
		t.Fatal(err)
	}
	if state != "initial" || next > 1<<62 {
		t.Fatalf("重置后应 initial 且立即到期，实际 state=%s next=%d", state, next)
	}
}

func TestRestoreArchivedClearsForeignEvidence(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE source_host_caps SET effective_status='Supported', registry_path_mode='native'
		WHERE source_id=? AND host='quay.io'`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE registry_host_recovery SET state='ok', gen=0, last_success_at=1, next_probe_at=9999999999
		WHERE source_id=? AND family='registry' AND scope='quay.io'`, id); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStatusWhy(id, "archived", "test"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStatusWhy(id, "pending", "restore"); err != nil {
		t.Fatal(err)
	}
	h, err := st.GetSourceHost(id, "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if h.EffectiveStatus != HostStatusUnknown {
		t.Fatalf("恢复后 Foreign 结论应清零，实际 %s", h.EffectiveStatus)
	}
	var state string
	if err := st.db.QueryRow(`SELECT state FROM registry_host_recovery
		WHERE source_id=? AND family='registry' AND scope='quay.io'`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "initial" {
		t.Fatalf("恢复后 recovery 应 initial，实际 %s", state)
	}
}

func TestSetCapsLiveSkipsDisabledHost(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE source_host_caps SET effective_status='Supported', registry_path_mode='native'
		WHERE source_id=? AND host='docker.io'`, id); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSourceHostEnabled(id, "docker.io", false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetCapsLive(id, CapNo, CapYes, CapNo, false); err != nil {
		t.Fatal(err)
	}
	h, err := st.GetSourceHost(id, "docker.io")
	if err != nil {
		t.Fatal(err)
	}
	if h.Enabled || h.EffectiveStatus != HostStatusSupported {
		t.Fatalf("停用 Host 不应被旧写路径改写：enabled=%v status=%s", h.Enabled, h.EffectiveStatus)
	}
}

func TestApplyCredChangesRejectsHubHosts(t *testing.T) {
	st := openTest(t)
	for _, h := range []string{"docker.io", "ghcr.io"} {
		if err := st.ApplyCredChanges([]CredChange{{Host: h, Digest: nil}}); err == nil {
			t.Fatalf("%s 不应接受 Host 代次凭据", h)
		}
	}
}

func TestEnableHostResetsRecovery(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE registry_host_recovery SET state='ok', next_probe_at=9999999999
		WHERE source_id=? AND family='registry' AND scope='quay.io'`, id); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSourceHostEnabled(id, "quay.io", false); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSourceHostEnabled(id, "quay.io", true); err != nil {
		t.Fatal(err)
	}
	var state string
	var next int64
	if err := st.db.QueryRow(`SELECT state,next_probe_at FROM registry_host_recovery
		WHERE source_id=? AND family='registry' AND scope='quay.io'`, id).Scan(&state, &next); err != nil {
		t.Fatal(err)
	}
	if state != "initial" || next > 1<<62 {
		t.Fatalf("重新启用应重置 recovery，实际 state=%s next=%d", state, next)
	}
}

func TestResetInvalidatesInFlightProbe(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	var pg int64
	if err := st.db.QueryRow(`SELECT policy_gen FROM source_realm_policy WHERE source_id=? AND host='quay.io'`, id).Scan(&pg); err != nil {
		t.Fatal(err)
	}
	if err := st.ResetSourceHostVerdict(id, "quay.io"); err != nil {
		t.Fatal(err)
	}
	res, err := st.CommitHostProbe(HostProbeCommit{
		SourceID: id, Host: "quay.io", Cap: RegistryCapYes, PathMode: PathNative,
		At: 1, PolicyGen: pg, HasPolicyGen: true,
	})
	if err != nil || res != HostCapsStaleSnapshot {
		t.Fatalf("重置前启动的探测应 stale，res=%v err=%v", res, err)
	}
}

func TestSetCapsLiveRejectsStalePolicyGen(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{BaseURL: "https://hub.example/", Name: "hub", RepoHost: "docker.io", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	hosts, err := st.GetSourceHosts(id)
	if err != nil {
		t.Fatal(err)
	}
	gens := map[string]int64{}
	for _, h := range hosts {
		gens[h.Host] = h.PolicyGen
	}
	if err := st.SetRealmPolicy(id, "docker.io", "https://realm.example", nil); err != nil {
		t.Fatal(err)
	}
	ok, err := st.SetCapsLive(id, CapYes, CapYes, CapYes, false, gens)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	h, err := st.GetSourceHost(id, "docker.io")
	if err != nil {
		t.Fatal(err)
	}
	if h.EffectiveStatus != HostStatusUnknown {
		t.Fatalf("旧策略代次的探测不得改写 caps: %s", h.EffectiveStatus)
	}
}
