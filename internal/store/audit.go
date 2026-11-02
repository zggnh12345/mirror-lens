package store

import "time"

// AuditEvent 是一次管理动作的记录。detail 只放非敏感摘要，
// 密码、Token 明文、Cookie 一律不许写进来。
type AuditEvent struct {
	ID     int64  `json:"id"`
	TS     int64  `json:"ts"`
	IP     string `json:"ip"`
	User   string `json:"user"`
	Action string `json:"action"`
	Target string `json:"target"`
	Detail string `json:"detail"`
	OK     bool   `json:"ok"`
}

const auditRetention = 90 * 24 * time.Hour

// Audit 入队即返回。队列满了就丢：审计是旁路，不能反压认证和下载。
func (s *Store) Audit(e AuditEvent) {
	s.auditOnce.Do(func() {
		s.auditCh = make(chan AuditEvent, 512)
		s.auditStop = make(chan struct{})
		go s.auditLoop()
	})
	if e.TS == 0 {
		e.TS = time.Now().Unix()
	}
	select {
	case s.auditCh <- e:
	default:
	}
}

func (s *Store) auditLoop() {
	n := 0
	for {
		select {
		case <-s.auditStop:
			return
		case e := <-s.auditCh:
			ok := 1
			if !e.OK {
				ok = 0
			}
			if _, err := s.db.Exec(
				`INSERT INTO audit(ts,ip,user,action,target,detail,ok) VALUES(?,?,?,?,?,?,?)`,
				e.TS, e.IP, e.User, e.Action, e.Target, e.Detail, ok); err != nil {
				continue
			}
			// 每 500 条顺手清一次过期记录，省掉一个定时任务。
			if n++; n%500 == 0 {
				_, _ = s.db.Exec(`DELETE FROM audit WHERE ts < ?`, time.Now().Add(-auditRetention).Unix())
			}
		}
	}
}

// ListAudit 返回最近 limit 条，新的在前。
func (s *Store) ListAudit(limit int) ([]AuditEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(
		`SELECT id,ts,ip,user,action,target,detail,ok FROM audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEvent{}
	for rows.Next() {
		var e AuditEvent
		var ok int
		if err := rows.Scan(&e.ID, &e.TS, &e.IP, &e.User, &e.Action, &e.Target, &e.Detail, &ok); err != nil {
			return nil, err
		}
		e.OK = ok == 1
		out = append(out, e)
	}
	return out, rows.Err()
}
