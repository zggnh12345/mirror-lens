package store

// TrustedRelay 是勾了「信任」的源：带 PAT 的私有请求只会发给这些地址，
// 多个之间并发竞速。白名单只存标记，地址本身仍由「源管理」维护。
type TrustedRelay struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

// ListTrustedRelays 返回当前生效的白名单：勾了信任、并且源本身是启用的。
func (s *Store) ListTrustedRelays() ([]TrustedRelay, error) {
	rows, err := s.db.Query(`SELECT id, name, base_url FROM sources
		WHERE trusted = 1 AND enabled = 1 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TrustedRelay, 0, 4)
	for rows.Next() {
		var t TrustedRelay
		if err := rows.Scan(&t.ID, &t.Name, &t.URL); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetSourceTrusted 勾选或取消某个源的信任标记。
func (s *Store) SetSourceTrusted(id int64, on bool) (bool, error) {
	v := 0
	if on {
		v = 1
	}
	res, err := s.db.Exec(`UPDATE sources SET trusted = ? WHERE id = ?`, v, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
