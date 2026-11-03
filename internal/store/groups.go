package store

import "fmt"

// Group 是一组源的集合，带一个优先级：rank 越小，调度时越先试。
// 用户把实测快的源放进同一个组并置顶，候选源与分片就优先吃这组。
type Group struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Rank    int    `json:"rank"`
	Sources int    `json:"sources"` // 组内源数量
}

// ListGroups 按 rank 升序返回分组（同 rank 按 id，保证顺序稳定）。
func (s *Store) ListGroups() ([]Group, error) {
	rows, err := s.db.Query(`SELECT g.id, g.name, g.rank,
		(SELECT COUNT(*) FROM sources x WHERE x.group_id = g.id)
		FROM source_groups g ORDER BY g.rank, g.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Group{}
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Name, &g.Rank, &g.Sources); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// CreateGroup 新建分组，默认排到最后，用户可再上移。
func (s *Store) CreateGroup(name string) (int64, error) {
	if name == "" {
		return 0, fmt.Errorf("分组名不能为空")
	}
	var maxRank int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(rank), 0) FROM source_groups`).Scan(&maxRank); err != nil {
		return 0, err
	}
	res, err := s.db.Exec(`INSERT INTO source_groups(name, rank) VALUES(?, ?)`, name, maxRank+1)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateGroup 改名或调整优先级。
func (s *Store) UpdateGroup(id int64, name *string, rank *int) error {
	if name != nil {
		if *name == "" {
			return fmt.Errorf("分组名不能为空")
		}
		if _, err := s.db.Exec(`UPDATE source_groups SET name=? WHERE id=?`, *name, id); err != nil {
			return err
		}
	}
	if rank != nil {
		if _, err := s.db.Exec(`UPDATE source_groups SET rank=? WHERE id=?`, *rank, id); err != nil {
			return err
		}
	}
	return nil
}

// ReorderGroups 按给定 id 顺序重排优先级（rank = 下标+1）。
// 一次事务内改完：两个分组的 rank 互换时不会出现短暂的重复档位，
// 面板连点也不会因为读到中间态而乱序。
func (s *Store) ReorderGroups(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`UPDATE source_groups SET rank=? WHERE id=?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i, id := range ids {
		if _, err := stmt.Exec(i+1, id); err != nil {
			return err
		}
	}
	// 没在列表里的组统一压到最后（用 id 拉开，保持相对顺序），
	// 这样即便前端只提交了一部分 id 也不会出现并列档位。
	if _, err := tx.Exec(`UPDATE source_groups SET rank = 1000 + id WHERE id NOT IN (`+mark(len(ids))+`)`,
		intsToAny(ids)...); err != nil {
		return err
	}
	return tx.Commit()
}

func mark(n int) string {
	out := "?"
	for i := 1; i < n; i++ {
		out += ",?"
	}
	return out
}

func intsToAny(v []int64) []any {
	out := make([]any, len(v))
	for i := range v {
		out[i] = v[i]
	}
	return out
}

// DeleteGroup 删组，组内源退回未分组（不删源：用户删的是标签，不是站点）。
func (s *Store) DeleteGroup(id int64) error {
	if _, err := s.db.Exec(`UPDATE sources SET group_id=0 WHERE group_id=?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM source_groups WHERE id=?`, id)
	return err
}

// SetSourceGroup 把一台源放进分组，0 表示移出分组。
func (s *Store) SetSourceGroup(id, groupID int64) error {
	if groupID != 0 {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM source_groups WHERE id=?`, groupID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("分组不存在")
		}
	}
	_, err := s.db.Exec(`UPDATE sources SET group_id=? WHERE id=?`, groupID, id)
	return err
}

// attachGroups 给源列表补上组名和组优先级，一次查完再分桶，避免逐源查询。
func (s *Store) attachGroups(list []Source) error {
	groups, err := s.ListGroups()
	if err != nil {
		return err
	}
	if len(groups) == 0 {
		return nil
	}
	idx := make(map[int64]Group, len(groups))
	for _, g := range groups {
		idx[g.ID] = g
	}
	for i := range list {
		if g, ok := idx[list[i].GroupID]; ok {
			list[i].GroupName = g.Name
			list[i].GroupRank = g.Rank
		}
	}
	return nil
}
