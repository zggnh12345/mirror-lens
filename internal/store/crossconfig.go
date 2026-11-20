package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// 3.5.18 阶段 18a：B 独立总开关 + 派发模式 + 排序模式的统一配置模型。
//
// 三字段彼此独立，全部存 settings 表（1.6 §2811：与 A 的 Host 开关同载体，
// 不新建表、不改 Schema）。本文件是三字段解析与迁移的**唯一入口**，所有
// 代码路径（API / 网关运行时 / Web 展示）必须调用它，不得各自决定优先级。
//
// 冲突优先级见 docs/3.5.18-config-migration.md §2（18a 设计冻结报告）：
//  1. 新字段 cross_registry_enabled 存在时以其为准；
//  2. cross_registry_mode 新值直接用；历史 off → enabled=false + sequential；
//  3. sort_mode 独立，缺省 fixed；
//  4. 新旧冲突以新字段为准并写审计；
//  5. 三键全缺省 → enabled=true + sequential + fixed；
//  6. deployMode（registry.replica_mode）只在三键全缺省时作基线。

const (
	bEnabledKey  = "cross_registry_enabled"
	bModeKey     = "cross_registry_mode" // 与 3.5.17 兼容：同一 settings 键
	bSortModeKey = "cross_registry_sort_mode"
)

// BConfig 是 B 三字段的解析结果。
type BConfig struct {
	Enabled  bool
	Mode     string // sequential / race
	SortMode string // fixed / dynamic
}

// BEnabledKey / BModeKey / BSortModeKey 暴露 settings 键名（API/测试断言用）。
func BEnabledKey() string  { return bEnabledKey }
func BModeKey() string     { return bModeKey }
func BSortModeKey() string { return bSortModeKey }

// HasBConfig 判定 settings 中是否出现过任一 B 配置键（含值为 off/空但键存在的情况
// 用非空值判定，与 hasSetting 一致）。供运行时决定是否使用 settings 覆盖部署基线。
func (s *Store) HasBConfig() bool {
	return s.hasSetting(bEnabledKey) || s.hasSetting(bModeKey) || s.hasSetting(bSortModeKey)
}

// 设置键是否存在的判定：GetSetting 用空串代表不存在，但显式空值也视为不存在。
func (s *Store) hasSetting(key string) bool {
	v := strings.TrimSpace(s.GetSetting(key, ""))
	return v != ""
}

// ResolveBConfig 是三字段解析的唯一入口，不写库、无副作用。
// deployMode 是部署期 registry.replica_mode 基线（off/sequential/race/空）。
func (s *Store) ResolveBConfig(deployMode string) BConfig {
	enabledRaw := strings.TrimSpace(s.GetSetting(bEnabledKey, ""))
	modeRaw := strings.ToLower(strings.TrimSpace(s.GetSetting(bModeKey, "")))
	sortRaw := strings.ToLower(strings.TrimSpace(s.GetSetting(bSortModeKey, "")))

	// 规则 5 + 6：三键全缺省时用部署期基线解析。
	if enabledRaw == "" && modeRaw == "" && sortRaw == "" {
		return bConfigFromDeploy(deployMode)
	}

	// 规则 1：enabled 由新字段决定；不存在时由 mode 的历史 off 推导。
	enabled := true
	if enabledRaw != "" {
		enabled = strings.EqualFold(enabledRaw, "true")
	} else if modeRaw == "off" {
		// 规则 2：历史 off 在缺少新 enabled 字段时映射为关闭。
		enabled = false
	}

	// 规则 2：mode 新值直接用；off/非法值归一为 sequential。
	mode := "sequential"
	switch modeRaw {
	case "sequential", "race":
		mode = modeRaw
	}

	// 规则 3：sort_mode 独立，缺省 fixed。
	sortMode := "fixed"
	if sortRaw == "dynamic" {
		sortMode = "dynamic"
	}

	return BConfig{Enabled: enabled, Mode: mode, SortMode: sortMode}
}

// bConfigFromDeploy 把部署期 replica_mode 解析为 B 配置（规则 6）。
func bConfigFromDeploy(deployMode string) BConfig {
	switch strings.ToLower(strings.TrimSpace(deployMode)) {
	case "off":
		return BConfig{Enabled: false, Mode: "sequential", SortMode: "fixed"}
	case "race":
		return BConfig{Enabled: true, Mode: "race", SortMode: "fixed"}
	case "sequential", "":
		return BConfig{Enabled: true, Mode: "sequential", SortMode: "fixed"}
	default:
		return BConfig{Enabled: true, Mode: "sequential", SortMode: "fixed"}
	}
}

// SetBEnabled 写 B 总开关（settings 键，同事务写审计）。
func (s *Store) SetBEnabled(enabled bool, actor, ip string) error {
	v := "false"
	if enabled {
		v = "true"
	}
	return s.Tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO settings(key,value) VALUES(?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, bEnabledKey, v); err != nil {
			return err
		}
		return auditTx(tx, actor, ip, "cross_registry_enabled", bEnabledKey, v)
	})
}

// SetBSortMode 写 B 排序模式，只允许 fixed/dynamic。
func (s *Store) SetBSortMode(mode, actor, ip string) error {
	v := strings.ToLower(strings.TrimSpace(mode))
	if v != "fixed" && v != "dynamic" {
		return fmt.Errorf("sort_mode 只能是 fixed / dynamic")
	}
	return s.Tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO settings(key,value) VALUES(?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, bSortModeKey, v); err != nil {
			return err
		}
		return auditTx(tx, actor, ip, "cross_registry_sort_mode", bSortModeKey, v)
	})
}

// SetBMode 写 B 派发模式（sequential/race），与旧 SetCrossRegistryMode 同键但不再接受 off。
func (s *Store) SetBMode(mode, actor, ip string) error {
	v := strings.ToLower(strings.TrimSpace(mode))
	if v != "sequential" && v != "race" {
		return fmt.Errorf("mode 只能是 sequential / race")
	}
	return s.Tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO settings(key,value) VALUES(?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, bModeKey, v); err != nil {
			return err
		}
		return auditTx(tx, actor, ip, "cross_registry_mode", bModeKey, v)
	})
}

// MigrateBConfig 把旧配置一次性迁移为新三字段，幂等。返回迁移后的配置。
// 只在检测到"新三字段尚不存在"时写入；已迁移则原样返回当前解析值。
// 新旧字段冲突（旧 off 与新 enabled=true 同时存在）时按新字段为准并写审计。
func (s *Store) MigrateBConfig(deployMode string, actor, ip string) (BConfig, error) {
	already := s.hasSetting(bEnabledKey) && s.hasSetting(bSortModeKey)
	conflict := false
	oldMode := strings.ToLower(strings.TrimSpace(s.GetSetting(bModeKey, "")))
	if already && oldMode == "off" && strings.EqualFold(strings.TrimSpace(s.GetSetting(bEnabledKey, "")), "true") {
		conflict = true
	}

	cfg := s.ResolveBConfig(deployMode)
	if !already {
		if err := s.SetSetting(bEnabledKey, fmt.Sprintf("%t", cfg.Enabled)); err != nil {
			return BConfig{}, err
		}
		if err := s.SetSetting(bModeKey, cfg.Mode); err != nil {
			return BConfig{}, err
		}
		if err := s.SetSetting(bSortModeKey, cfg.SortMode); err != nil {
			return BConfig{}, err
		}
	}
	if conflict {
		if err := s.Tx(func(tx *sql.Tx) error {
			return auditTx(tx, actor, ip, "config_migration", "cross_registry_conflict",
				fmt.Sprintf("old_mode=off enabled=true -> adopted=true,seq"))
		}); err != nil {
			return BConfig{}, err
		}
	}
	return cfg, nil
}
