package store

import (
	"database/sql"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"
)

// TrustedRelay 是勾了「信任」的源：带 PAT 的私有请求只会发给这些地址，
// 多个之间并发竞速。白名单只存标记，地址本身仍由「源管理」维护。
//
// Pin* 是拨号时唯一允许使用的地址集合。它们来自首次解析（TOFU）或
// 管理员手填，之后不再随 DNS 变化，防止同一域名被重新解析到内网。
type TrustedRelay struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	URL            string `json:"url"`
	PinHost        string `json:"pin_host"`
	PinIPv4        string `json:"pin_ipv4"`
	PinIPv6        string `json:"pin_ipv6"`
	PinState       string `json:"pin_state"`
	PinAt          int64  `json:"pin_at"`
	PinConfirmedAt int64  `json:"pin_confirmed_at"`
	PinGraceUntil  int64  `json:"pin_grace_until"`
}

// ListTrustedRelays 返回当前生效的白名单：勾了信任、并且源本身是启用的。
func (s *Store) ListTrustedRelays() ([]TrustedRelay, error) {
	rows, err := s.db.Query(`SELECT id, name, base_url,
		pin_host, pin_ipv4, pin_ipv6, pin_state, pin_at, pin_confirmed_at, pin_grace_until
		FROM sources WHERE trusted = 1 AND enabled = 1 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TrustedRelay, 0, 4)
	for rows.Next() {
		var t TrustedRelay
		if err := rows.Scan(&t.ID, &t.Name, &t.URL,
			&t.PinHost, &t.PinIPv4, &t.PinIPv6, &t.PinState,
			&t.PinAt, &t.PinConfirmedAt, &t.PinGraceUntil); err != nil {
			return nil, err
		}
		// 老库升级上来的行还没有 pin_host，按 base_url 补出来给上层用；
		// 落库仍等第一次勾选或解析时写。
		if t.PinHost == "" {
			t.PinHost = RelayHost(t.URL)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RelayHost 从源的 base_url 取规范化主机（含端口），作为钉住记录的键。
func RelayHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Host)
}

// UpdateRelayPin 由管理员显式写入钉住的 IP 集合，直接进入 confirmed。
// 传空串表示清掉该家族的钉住地址。
func (s *Store) UpdateRelayPin(id int64, ipv4, ipv6 string) (bool, error) {
	now := time.Now().Unix()
	res, err := s.db.Exec(`UPDATE sources SET
		pin_ipv4=?, pin_ipv6=?, pin_state='confirmed',
		pin_at=CASE WHEN pin_at=0 THEN ? ELSE pin_at END,
		pin_confirmed_at=?, pin_grace_until=0
		WHERE id=? AND trusted=1`, strings.TrimSpace(ipv4), strings.TrimSpace(ipv6), now, now, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetRelayPin 写入状态机结果。pending/confirmed/stale 才带 IP；
// unresolved 只带到期时间；expired 保留最后已知 IP 供排查。
func (s *Store) SetRelayPin(id int64, state, ipv4, ipv6 string, graceUntil int64) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`UPDATE sources SET
		pin_state=?, pin_ipv4=?, pin_ipv6=?,
		pin_at=CASE WHEN pin_at=0 AND ?<>'' THEN ? ELSE pin_at END,
		pin_grace_until=?
		WHERE id=?`, state, strings.TrimSpace(ipv4), strings.TrimSpace(ipv6),
		state, now, graceUntil, id)
	return err
}

// RelayPinIPs 把逗号分隔的钉住地址拆开，过滤掉非法值。
func RelayPinIPs(list string) []string {
	if strings.TrimSpace(list) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(list, ",") {
		p = strings.TrimSpace(p)
		if p == "" || net.ParseIP(p) == nil {
			continue
		}
		out = append(out, p)
	}
	return out
}

// SetSourceTrusted 勾选或取消某个源的信任标记。
// 勾选时顺手记下 base_url 的域名（pin_host），作为后续解析与钉 IP 的键。
func (s *Store) SetSourceTrusted(id int64, on bool) (bool, error) {
	var base string
	err := s.db.QueryRow(`SELECT base_url FROM sources WHERE id = ?`, id).Scan(&base)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if on {
		_, err = s.db.Exec(`UPDATE sources SET trusted = 1,
			pin_host = CASE WHEN pin_host = '' THEN ? ELSE pin_host END
			WHERE id = ?`, RelayHost(base), id)
		return err == nil, err
	}
	_, err = s.db.Exec(`UPDATE sources SET trusted = 0 WHERE id = ?`, id)
	return err == nil, err
}
