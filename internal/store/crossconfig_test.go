package store

import (
	"testing"
)

// 3.5.18 18a：B 三字段迁移矩阵（§2.3）与冲突优先级（§2.4）。
func TestResolveBConfigMatrix(t *testing.T) {
	cases := []struct {
		name     string
		enabled  string // settings 原值；"" = 不存在
		mode     string
		sort     string
		deploy   string // registry.replica_mode
		wantEn   bool
		wantMode string
		wantSort string
	}{
		{"三键全缺省/部署空", "", "", "", "", true, "sequential", "fixed"},
		{"三键全缺省/部署off", "", "", "", "off", false, "sequential", "fixed"},
		{"三键全缺省/部署race", "", "", "", "race", true, "race", "fixed"},
		{"旧显式off", "", "off", "", "", false, "sequential", "fixed"},
		{"旧显式sequential", "", "sequential", "", "", true, "sequential", "fixed"},
		{"旧显式race", "", "race", "", "", true, "race", "fixed"},
		{"新enabled显式关闭", "false", "race", "dynamic", "", false, "race", "dynamic"},
		{"新enabled显式开启", "true", "sequential", "fixed", "off", true, "sequential", "fixed"},
		{"冲突:旧off+新enabled=true", "true", "off", "", "", true, "sequential", "fixed"},
		{"非法mode归一sequential", "true", "bogus", "dynamic", "", true, "sequential", "dynamic"},
		{"非法sort归一fixed", "true", "race", "bogus", "", true, "race", "fixed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := openTestStore(t)
			if tc.enabled != "" {
				if err := s.SetSetting(bEnabledKey, tc.enabled); err != nil {
					t.Fatal(err)
				}
			}
			if tc.mode != "" {
				if err := s.SetSetting(bModeKey, tc.mode); err != nil {
					t.Fatal(err)
				}
			}
			if tc.sort != "" {
				if err := s.SetSetting(bSortModeKey, tc.sort); err != nil {
					t.Fatal(err)
				}
			}
			got := s.ResolveBConfig(tc.deploy)
			if got.Enabled != tc.wantEn || got.Mode != tc.wantMode || got.SortMode != tc.wantSort {
				t.Fatalf("got %+v, want enabled=%t mode=%s sort=%s", got, tc.wantEn, tc.wantMode, tc.wantSort)
			}
		})
	}
}

func TestMigrateBConfigIdempotent(t *testing.T) {
	s, _ := openTestStore(t)
	// 旧显式 race → 迁移为 enabled=true + race + fixed
	if err := s.SetSetting(bModeKey, "race"); err != nil {
		t.Fatal(err)
	}
	cfg1, err := s.MigrateBConfig("off", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg1.Enabled || cfg1.Mode != "race" || cfg1.SortMode != "fixed" {
		t.Fatalf("first migrate got %+v", cfg1)
	}
	if got := s.GetSetting(bEnabledKey, ""); got != "true" {
		t.Fatalf("enabled 应落库为 true, got %q", got)
	}
	// 用户随后把 enabled 改为 false；再迁移不得覆盖用户改动（幂等）。
	if err := s.SetSetting(bEnabledKey, "false"); err != nil {
		t.Fatal(err)
	}
	cfg2, err := s.MigrateBConfig("race", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Enabled {
		t.Fatalf("第二次迁移不得覆盖用户显式设置的 false, got %+v", cfg2)
	}
}

func TestMigrateBConfigConflictWritesAudit(t *testing.T) {
	s, _ := openTestStore(t)
	// 新旧字段同时存在且冲突：旧 off + 新 enabled=true
	if err := s.SetSetting(bModeKey, "off"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(bEnabledKey, "true"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(bSortModeKey, "fixed"); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.MigrateBConfig("", "admin", "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled || cfg.Mode != "sequential" {
		t.Fatalf("冲突应按新字段为准 enabled=true+sequential, got %+v", cfg)
	}
	// 审计应有一条 conflict 记录
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit WHERE action='config_migration' AND target='cross_registry_conflict'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应有 1 条冲突审计, got %d", n)
	}
}

// §10：全新安装从未设置 B 字段时，默认 ON（enabled=true+sequential+fixed），
// 且 hasBConfig=false 让运行时走部署基线分支。
func TestFreshInstallBDefaultsOn(t *testing.T) {
	s, _ := openTestStore(t)
	if s.HasBConfig() {
		t.Fatal("全新库不应有 B 配置键")
	}
	cfg := s.ResolveBConfig("")
	if !cfg.Enabled || cfg.Mode != "sequential" || cfg.SortMode != "fixed" {
		t.Fatalf("全新安装应默认 ON+sequential+fixed, got %+v", cfg)
	}
}

// §2.2：B OFF（enabled=false）不得删除可信映射与跨站参选配置。
func TestBOffKeepsMappingAndSelection(t *testing.T) {
	s, _ := openTestStore(t)
	// 先建两个加星的 Source，拿真实 ID。
	var ids []int64
	for _, u := range []string{"https://a.example", "https://b.example"} {
		res, err := s.db.Exec(`INSERT INTO sources(base_url,name,preferred,created_at) VALUES(?,?,1,strftime('%s','now'))`, u, u)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		ids = append(ids, id)
	}
	if err := s.SetCrossSelectedSources(ids, "admin", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBEnabled(false, "admin", ""); err != nil {
		t.Fatal(err)
	}
	if got := s.CrossSelectedSources(); len(got) != 2 {
		t.Fatalf("B OFF 后参选配置不得丢失, got %v", got)
	}
	cfg := s.ResolveBConfig("")
	if cfg.Enabled {
		t.Fatalf("显式 enabled=false 应关闭 B, got %+v", cfg)
	}
}
