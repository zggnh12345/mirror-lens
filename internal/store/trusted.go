package store

import (
	"errors"
	"strings"
	"time"
)

// TrustedRelay 是用户显式加入白名单的自建加速站：私有请求的 PAT
// 只会发给这里列出的地址。
type TrustedRelay struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Enabled bool   `json:"enabled"`
}

// ListTrusted 按加入顺序返回全部白名单（含停用的）。
func (s *Store) ListTrusted() ([]TrustedRelay, error) {
	rows, err := s.db.Query(`SELECT id, name, url, enabled FROM trusted_relays ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TrustedRelay, 0, 4)
	for rows.Next() {
		var t TrustedRelay
		var on int
		if err := rows.Scan(&t.ID, &t.Name, &t.URL, &on); err != nil {
			return nil, err
		}
		t.Enabled = on != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// AddTrusted 加一条；同一个 URL 重复加会把旧的启用并改名，不报错。
func (s *Store) AddTrusted(name, url string) (TrustedRelay, error) {
	name = strings.TrimSpace(name)
	url = strings.TrimSpace(url)
	if url == "" {
		return TrustedRelay{}, errors.New("地址不能为空")
	}
	if name == "" {
		name = url
	}
	_, err := s.db.Exec(`INSERT INTO trusted_relays(name, url, enabled, created_at)
		VALUES(?, ?, 1, ?)
		ON CONFLICT(url) DO UPDATE SET name=excluded.name, enabled=1`,
		name, url, time.Now().Unix())
	if err != nil {
		return TrustedRelay{}, err
	}
	var t TrustedRelay
	var on int
	err = s.db.QueryRow(`SELECT id, name, url, enabled FROM trusted_relays WHERE url=?`, url).
		Scan(&t.ID, &t.Name, &t.URL, &on)
	t.Enabled = on != 0
	return t, err
}

// DeleteTrusted 删一条，返回是否真的删到了。
func (s *Store) DeleteTrusted(id int64) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM trusted_relays WHERE id=?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetTrustedEnabled 只切换启用状态，不改地址。
func (s *Store) SetTrustedEnabled(id int64, on bool) (bool, error) {
	v := 0
	if on {
		v = 1
	}
	res, err := s.db.Exec(`UPDATE trusted_relays SET enabled=? WHERE id=?`, v, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
