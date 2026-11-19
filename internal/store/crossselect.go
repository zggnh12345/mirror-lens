package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// 3.5.17 B20：跨站参选勾选（`cross_registry_selected_sources`）。
//
// 语义（1.6 §B.4 / AUD-04-1，冻结）：
//   - 它是 Source 星标这一人工授权在「跨站用途」上的子集过滤器，不是第二套资格；
//   - 默认空列表：空即无跨站候选、退化顺序回退；
//   - 恒为星标集合子集：读时过滤陈旧的已撤星条目（星标撤销后条目失效，不留幽灵资格）；
//   - 存于 settings 表（与 A 的 Host 开关同一载体），不新建表、不改 Schema。
const crossSelectedKey = "cross_registry_selected_sources"

// CrossSelectedKey 暴露设置键名（API/测试断言用）。
func CrossSelectedKey() string { return crossSelectedKey }

// CrossSelectedSources 返回勾选的 Source ID（按 ID 升序）。空列表表示没有
// 任何跨站候选——不是「全部参选」。损坏的 JSON 视为空列表。
func (s *Store) CrossSelectedSources() []int64 {
	raw := strings.TrimSpace(s.GetSetting(crossSelectedKey, ""))
	if raw == "" {
		return nil
	}
	var ids []int64
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil
	}
	out := make([]int64, 0, len(ids))
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// StarredSourceIDs 返回当前星标（preferred=1）的 Source ID 集合。
func (s *Store) StarredSourceIDs() (map[int64]bool, error) {
	rows, err := s.db.Query(`SELECT id FROM sources WHERE preferred=1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// EffectiveCrossSelected 返回「勾选 ∩ 星标」的集合：读时过滤已撤星的陈旧条目。
// 这是候选发现唯一该用的入口；只读 CrossSelectedSources 会把陈旧条目当成资格。
func (s *Store) EffectiveCrossSelected() map[int64]bool {
	ids := s.CrossSelectedSources()
	out := make(map[int64]bool, len(ids))
	if len(ids) == 0 {
		return out
	}
	starred, err := s.StarredSourceIDs()
	if err != nil {
		return out
	}
	for _, id := range ids {
		if starred[id] {
			out[id] = true
		}
	}
	return out
}

// SetCrossSelectedSources 全量写入勾选集合（空切片 = 清空）。
//
// 只接受存在且星标的 Source：勾选不能给非星标节点授权，写入时直接拒绝，
// 避免设置表里留下永远不生效的条目。设置与审计同事务落库，崩溃时同进同退。
func (s *Store) SetCrossSelectedSources(ids []int64, actor, ip string) error {
	want := make([]int64, 0, len(ids))
	seen := map[int64]bool{}
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		want = append(want, id)
	}
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	starred, err := s.StarredSourceIDs()
	if err != nil {
		return err
	}
	for _, id := range want {
		if !starred[id] {
			return fmt.Errorf("source %d 未加星标，不能进入跨站参选集合", id)
		}
	}
	raw := "[]"
	if len(want) > 0 {
		b, err := json.Marshal(want)
		if err != nil {
			return err
		}
		raw = string(b)
	}
	return s.Tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO settings(key,value) VALUES(?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, crossSelectedKey, raw); err != nil {
			return err
		}
		return auditTx(tx, actor, ip, "cross_registry_selected", crossSelectedKey, raw)
	})
}

// SetCrossSourceSelected 勾选/取消单个 Source，保持集合其余成员不变。
func (s *Store) SetCrossSourceSelected(id int64, on bool, actor, ip string) error {
	cur := s.CrossSelectedSources()
	out := make([]int64, 0, len(cur)+1)
	found := false
	for _, v := range cur {
		if v == id {
			found = true
			if !on {
				continue
			}
		}
		out = append(out, v)
	}
	if on && !found {
		out = append(out, id)
	}
	return s.SetCrossSelectedSources(out, actor, ip)
}

// auditTx 在同事务里写一条管理审计。凭据/映射/开关类改动必须耐久，
// 不能只依赖异步 Store.Audit（队列满即丢）。
func auditTx(tx *sql.Tx, actor, ip, action, target, detail string) error {
	if actor == "" {
		actor = "admin"
	}
	_, err := tx.Exec(`INSERT INTO audit(ts,ip,user,action,target,detail,ok) VALUES(?,?,?,?,?,?,1)`,
		time.Now().Unix(), ip, actor, action, target, detail)
	return err
}

// crossModeKey 是跨站模式的 UI 设置键。部署期配置 `registry.replica_mode`
// 仍是基线，UI 设置优先；两者都缺省时按 B17 的「默认顺序回退」处理。
const crossModeKey = "cross_registry_mode"

// CrossRegistryMode 返回 UI 设置的跨站模式：""（未设置）/sequential/race/off。
func (s *Store) CrossRegistryMode() string {
	v := strings.ToLower(strings.TrimSpace(s.GetSetting(crossModeKey, "")))
	switch v {
	case "sequential", "race", "off":
		return v
	}
	return ""
}

// SetCrossRegistryMode 写入跨站模式（顺序回退 / 跨站竞速 / off）。
// 与设置同事务写管理审计：模式切换是人工决策，必须可追溯。
func (s *Store) SetCrossRegistryMode(mode, actor, ip string) error {
	v := strings.ToLower(strings.TrimSpace(mode))
	switch v {
	case "sequential", "race", "off":
	default:
		return fmt.Errorf("mode 只能是 sequential / race / off")
	}
	return s.Tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO settings(key,value) VALUES(?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, crossModeKey, v); err != nil {
			return err
		}
		return auditTx(tx, actor, ip, "cross_registry_mode", crossModeKey, v)
	})
}
