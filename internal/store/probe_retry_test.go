package store

import (
	"path/filepath"
	"testing"
	"time"
)

func retryTestStore(t *testing.T) (*Store, int64, int64) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	id, err := st.AddSource(Source{Name: "mirror", BaseURL: "https://mirror.example", Status: "usable", Enabled: true, RepoHost: "quay.io"})
	if err != nil {
		t.Fatal(err)
	}
	// 建出 registry_host_gen 行：缺行在 CAS 里按「不匹配」处理。
	gen, err := st.BumpHostGen("quay.io")
	if err != nil {
		t.Fatal(err)
	}
	return st, id, gen
}

func TestHostProbeRetryLadderCapsAtSeven(t *testing.T) {
	st, id, gen := retryTestStore(t)
	at := time.Now().Unix()
	wantDelays := []int64{15, 30, 60, 120, 240, 480, 600, 600}

	for i, want := range wantDelays {
		ok, err := st.ScheduleHostProbeRetry(HostProbeRetry{SourceID: id, Host: "quay.io", Gen: gen, At: at})
		if err != nil {
			t.Fatalf("第 %d 次调度: %v", i+1, err)
		}
		if !ok {
			t.Fatalf("第 %d 次调度被拒", i+1)
		}
		failures, next, found, err := st.HostProbeRetryState(id, "quay.io")
		if err != nil || !found {
			t.Fatalf("读状态: found=%v err=%v", found, err)
		}
		if failures != i+1 && !(i+1 > 7 && failures == 7) {
			t.Fatalf("第 %d 次后 failures=%d", i+1, failures)
		}
		if got := next - at; got != want {
			t.Fatalf("第 %d 次退避=%ds，期望 %ds", i+1, got, want)
		}
		at = next
	}

	failures, _, _, _ := st.HostProbeRetryState(id, "quay.io")
	if failures != 7 {
		t.Fatalf("退避次数必须封顶在 7，得到 %d", failures)
	}
	if ok, err := st.ScheduleHostProbeRetry(HostProbeRetry{SourceID: id, Host: "quay.io", Gen: gen, At: at}); err != nil || !ok {
		t.Fatalf("封顶后再次调度: ok=%v err=%v", ok, err)
	}
	_, cappedNext, _, _ := st.HostProbeRetryState(id, "quay.io")
	if got := cappedNext - at; got != 600 {
		t.Fatalf("封顶后延迟仍应是 600s，得到 %ds", got)
	}
}

func TestHostProbeRetryCASRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, st *Store, id, gen int64)
		gen    func(gen int64) int64
	}{
		{name: "代次不符", gen: func(g int64) int64 { return g - 1 }},
		{name: "未到期", mutate: func(t *testing.T, st *Store, id, gen int64) {
			if _, err := st.db.Exec(`UPDATE registry_host_recovery SET next_probe_at=?
				WHERE source_id=? AND family='registry'`, time.Now().Unix()+3600, id); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "源被禁用", mutate: func(t *testing.T, st *Store, id, gen int64) {
			off := false
			if err := st.UpdateSource(id, &off, nil, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "源已归档", mutate: func(t *testing.T, st *Store, id, gen int64) {
			if _, err := st.db.Exec(`UPDATE sources SET status='archived' WHERE id=?`, id); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "hard-excluded", mutate: func(t *testing.T, st *Store, id, gen int64) {
			if _, err := st.db.Exec(`UPDATE registry_host_recovery SET state='hard-excluded', next_probe_at=0
				WHERE source_id=? AND family='registry'`, id); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "Host 已换归属", mutate: func(t *testing.T, st *Store, id, gen int64) {
			if _, err := st.db.Exec(`UPDATE sources SET repo_host='ghcr.io' WHERE id=?`, id); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "gen 行缺失", mutate: func(t *testing.T, st *Store, id, gen int64) {
			if _, err := st.db.Exec(`DELETE FROM registry_host_gen WHERE host='quay.io'`); err != nil {
				t.Fatal(err)
			}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, id, gen := retryTestStore(t)
			// 先累计一次，确保拒绝时不改动任何计数。
			at := time.Now().Unix()
			if ok, err := st.ScheduleHostProbeRetry(HostProbeRetry{SourceID: id, Host: "quay.io", Gen: gen, At: at}); err != nil || !ok {
				t.Fatalf("预置调度失败: ok=%v err=%v", ok, err)
			}
			before, beforeNext, _, _ := st.HostProbeRetryState(id, "quay.io")
			if tc.mutate != nil {
				tc.mutate(t, st, id, gen)
			}
			useGen := gen
			if tc.gen != nil {
				useGen = tc.gen(gen)
			}
			// hard-excluded 的 next 被重置为 0，用当下作为 At 才能验证 state 条件。
			ok, err := st.ScheduleHostProbeRetry(HostProbeRetry{SourceID: id, Host: "quay.io", Gen: useGen, At: beforeNext})
			if err != nil {
				t.Fatalf("拒绝路径不应报错: %v", err)
			}
			if ok {
				t.Fatal("CAS 应当拒绝")
			}
			after, _, _, _ := st.HostProbeRetryState(id, "quay.io")
			if after != before {
				t.Fatalf("拒绝时不得改动退避计数: %d -> %d", before, after)
			}
		})
	}
}

func TestHostProbeRetryTouchesOnlySchedulingFields(t *testing.T) {
	st, id, gen := retryTestStore(t)
	if _, err := st.db.Exec(`UPDATE registry_host_recovery
		SET state='ok', rec_failures=3, last_probe_at=100, last_success_at=90, last_error='boom'
		WHERE source_id=? AND family='registry'`, id); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Unix()
	if ok, err := st.ScheduleHostProbeRetry(HostProbeRetry{SourceID: id, Host: "quay.io", Gen: gen, At: at}); err != nil || !ok {
		t.Fatalf("调度失败: ok=%v err=%v", ok, err)
	}
	var state, lastErr string
	var rec, lastProbe, lastOK int
	if err := st.db.QueryRow(`SELECT state, rec_failures, last_probe_at, last_success_at, last_error
		FROM registry_host_recovery WHERE source_id=? AND family='registry'`, id).
		Scan(&state, &rec, &lastProbe, &lastOK, &lastErr); err != nil {
		t.Fatal(err)
	}
	if state != "ok" || rec != 3 || lastProbe != 100 || lastOK != 90 || lastErr != "boom" {
		t.Fatalf("调度只应写 next_probe_at/retry_failures: state=%s rec=%d probe=%d ok=%d err=%q",
			state, rec, lastProbe, lastOK, lastErr)
	}
}

func TestRetryFailuresClearedOnlyByFreshDefinition(t *testing.T) {
	t.Run("成功提交", func(t *testing.T) {
		st, id, gen := retryTestStore(t)
		at := time.Now().Unix()
		if ok, _ := st.ScheduleHostProbeRetry(HostProbeRetry{SourceID: id, Host: "quay.io", Gen: gen, At: at}); !ok {
			t.Fatal("预置失败")
		}
		res, err := st.CommitHostProbe(HostProbeCommit{
			SourceID: id, Host: "quay.io", Cap: RegistryCapYes, PathMode: PathNative,
			At: at, Gen: gen, TTL: 24 * time.Hour,
		})
		if err != nil {
			t.Fatal(err)
		}
		if res != HostCapsCommitted {
			t.Fatalf("提交结果=%v，期望 Committed", res)
		}
		if failures, _, _, _ := st.HostProbeRetryState(id, "quay.io"); failures != 0 {
			t.Fatalf("成功提交必须清零，得到 %d", failures)
		}
	})

	t.Run("代次变化", func(t *testing.T) {
		st, id, gen := retryTestStore(t)
		at := time.Now().Unix()
		if ok, _ := st.ScheduleHostProbeRetry(HostProbeRetry{SourceID: id, Host: "quay.io", Gen: gen, At: at}); !ok {
			t.Fatal("预置失败")
		}
		if _, err := st.BumpHostGen("quay.io"); err != nil {
			t.Fatal(err)
		}
		if failures, _, _, _ := st.HostProbeRetryState(id, "quay.io"); failures != 0 {
			t.Fatalf("代次变化必须清零，得到 %d", failures)
		}
	})

	t.Run("Host 变更", func(t *testing.T) {
		st, id, gen := retryTestStore(t)
		at := time.Now().Unix()
		if ok, _ := st.ScheduleHostProbeRetry(HostProbeRetry{SourceID: id, Host: "quay.io", Gen: gen, At: at}); !ok {
			t.Fatal("预置失败")
		}
		if err := st.SetRepoHost(id, "docker.io"); err != nil {
			t.Fatal(err)
		}
		if failures, _, _, _ := st.HostProbeRetryState(id, "docker.io"); failures != 0 {
			t.Fatalf("换 Host 后新行必须从 0 起算，得到 %d", failures)
		}
	})
}

// 重新启用是「Generation/启用状态重置」的一部分：真实的 0→1 转换
// 无条件清零退避次数；重复启用不算转换。
func TestReenableClearsRetryFailures(t *testing.T) {
	schedule := func(t *testing.T, st *Store, id, gen int64) int64 {
		t.Helper()
		at := time.Now().Unix()
		if ok, err := st.ScheduleHostProbeRetry(HostProbeRetry{SourceID: id, Host: "quay.io", Gen: gen, At: at}); err != nil || !ok {
			t.Fatalf("预置退避: ok=%v err=%v", ok, err)
		}
		_, next, found, err := st.HostProbeRetryState(id, "quay.io")
		if err != nil || !found {
			t.Fatalf("读状态: found=%v err=%v", found, err)
		}
		return next
	}
	toggle := func(t *testing.T, st *Store, id int64, on bool) error {
		t.Helper()
		return st.UpdateSource(id, &on, nil, nil, nil, nil)
	}
	retry := func(t *testing.T, st *Store, id int64) (int, int64) {
		t.Helper()
		f, n, found, err := st.HostProbeRetryState(id, "quay.io")
		if err != nil || !found {
			t.Fatalf("读状态: found=%v err=%v", found, err)
		}
		return f, n
	}

	t.Run("next 在未来则清零并拉近", func(t *testing.T) {
		st, id, gen := retryTestStore(t)
		schedule(t, st, id, gen)
		if err := toggle(t, st, id, false); err != nil {
			t.Fatal(err)
		}
		if err := toggle(t, st, id, true); err != nil {
			t.Fatal(err)
		}
		f, next := retry(t, st, id)
		if f != 0 {
			t.Fatalf("必须清零，得到 %d", f)
		}
		if next > time.Now().Unix() {
			t.Fatalf("原排期在未来时必须拉近，得到 %d", next)
		}
	})

	t.Run("next 已到期则清零且保持原值", func(t *testing.T) {
		st, id, gen := retryTestStore(t)
		schedule(t, st, id, gen)
		past := time.Now().Unix() - 60
		if _, err := st.db.Exec(`UPDATE registry_host_recovery SET next_probe_at=? WHERE source_id=? AND family='registry'`, past, id); err != nil {
			t.Fatal(err)
		}
		if err := toggle(t, st, id, false); err != nil {
			t.Fatal(err)
		}
		if err := toggle(t, st, id, true); err != nil {
			t.Fatal(err)
		}
		f, next := retry(t, st, id)
		if f != 0 {
			t.Fatalf("到期状态也必须清零，得到 %d", f)
		}
		if next != past {
			t.Fatalf("已到期不得改写 next: got=%d want=%d", next, past)
		}
	})

	t.Run("已启用重复启用不重置", func(t *testing.T) {
		st, id, gen := retryTestStore(t)
		schedule(t, st, id, gen)
		if err := toggle(t, st, id, true); err != nil {
			t.Fatal(err)
		}
		if f, _ := retry(t, st, id); f != 1 {
			t.Fatalf("没有 0→1 转换就不该重置，得到 %d", f)
		}
	})

	t.Run("第二步失败则整笔回滚", func(t *testing.T) {
		st, id, gen := retryTestStore(t)
		next := schedule(t, st, id, gen)
		if err := toggle(t, st, id, false); err != nil {
			t.Fatal(err)
		}
		// 只让「拉近 next_probe_at」这一步失败，验证清零被一起回滚。
		if _, err := st.db.Exec(`CREATE TRIGGER fail_pull BEFORE UPDATE OF next_probe_at
ON registry_host_recovery BEGIN SELECT RAISE(ABORT, 'boom'); END;`); err != nil {
			t.Fatal(err)
		}
		if err := toggle(t, st, id, true); err == nil {
			t.Fatal("预期第二步失败")
		}
		if f, n := retry(t, st, id); f != 1 || n != next {
			t.Fatalf("必须整笔回滚: failures=%d next=%d want 1/%d", f, n, next)
		}
		x, err := st.GetSource(id)
		if err != nil {
			t.Fatal(err)
		}
		if x.Enabled {
			t.Fatal("sources.enabled 也必须回滚")
		}
	})

	t.Run("多 Source 只重置发生转换的", func(t *testing.T) {
		st, id1, _ := retryTestStore(t)
		id2, err := st.AddSource(Source{Name: "second", BaseURL: "https://two.example", Status: "usable", Enabled: true, RepoHost: "quay.io"})
		if err != nil {
			t.Fatal(err)
		}
		gen, err := st.BumpHostGen("quay.io")
		if err != nil {
			t.Fatal(err)
		}
		schedule(t, st, id1, gen)
		schedule(t, st, id2, gen)

		if err := toggle(t, st, id1, false); err != nil {
			t.Fatal(err)
		}
		if err := toggle(t, st, id1, true); err != nil {
			t.Fatal(err)
		}
		if f, _ := retry(t, st, id1); f != 0 {
			t.Fatalf("转换的 Source 必须清零，得到 %d", f)
		}
		if f, _ := retry(t, st, id2); f != 1 {
			t.Fatalf("未转换的 Source 不得重置，得到 %d", f)
		}
	})
}

func TestMigrationAddsRetryFailuresColumn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "migrate.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.AddSource(Source{Name: "m", BaseURL: "https://m.example", Status: "usable", Enabled: true, RepoHost: "quay.io"})
	if err != nil {
		t.Fatal(err)
	}
	// 模拟 3.5.1 的库：没有 retry_failures 列。
	if _, err := st.db.Exec(`ALTER TABLE registry_host_recovery DROP COLUMN retry_failures`); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("重开老库: %v", err)
	}
	defer st2.Close()
	if failures, _, found, err := st2.HostProbeRetryState(id, "quay.io"); err != nil || !found || failures != 0 {
		t.Fatalf("补列后应为 0: failures=%d found=%v err=%v", failures, found, err)
	}
}
