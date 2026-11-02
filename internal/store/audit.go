package store

import (
	"log"
	"time"
)

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

const (
	auditRetention = 90 * 24 * time.Hour
	auditSweep     = 24 * time.Hour
)

// startAuditor 由 Open 显式调用。审计协程和数据库同生共死，
// 不依赖「第一条事件」来懒启动：没有事件时清理也要照跑。
func (s *Store) startAuditor() {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	if s.auditStarted {
		return
	}
	s.auditCh = make(chan AuditEvent, 512)
	s.auditStop = make(chan struct{})
	s.auditDone = make(chan struct{})
	s.auditStarted = true
	// 启动清理就地做完再交给协程：Open 返回时过期记录已经是清掉的状态，
	// 不用猜协程什么时候跑、也没有「重启后半天不清」的窗口。
	s.PurgeAudit(time.Now().Add(-auditRetention))
	go s.auditLoop()
}

// stopAuditor 关闭通道接收、等队列写完、等协程退出。
// 三步顺序不能换：先置 stopped 保证没有新事件进队，再唤醒协程排空，最后等它退出。
func (s *Store) stopAuditor() {
	s.auditMu.Lock()
	if !s.auditStarted || s.auditStopped {
		s.auditMu.Unlock()
		return
	}
	s.auditStopped = true
	close(s.auditStop)
	done := s.auditDone
	s.auditMu.Unlock()
	<-done
}

// Audit 入队即返回。队列满了就丢：审计是旁路，不能反压认证和下载。
// Store 已关闭后调用是空操作（不会 panic，也不会往关掉的库上写）。
func (s *Store) Audit(e AuditEvent) {
	if e.TS == 0 {
		e.TS = time.Now().Unix()
	}
	// 发送与关闭在同一把锁下互斥：stopped 一旦置位，就不会再有事件进队，
	// 排空阶段自然能把所有已入队的事件写完。
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	if !s.auditStarted || s.auditStopped {
		return
	}
	select {
	case s.auditCh <- e:
	default:
	}
}

func (s *Store) auditLoop() {
	defer close(s.auditDone)
	tick := time.NewTicker(auditSweep)
	defer tick.Stop()
	for {
		select {
		case <-s.auditStop:
			s.drainAudit()
			return
		case <-tick.C:
			s.PurgeAudit(time.Now().Add(-auditRetention))
		case e := <-s.auditCh:
			s.writeAudit(e)
		}
	}
}

// drainAudit 把已经入队的事件写完再退出，保证正常关闭时不丢记录。
func (s *Store) drainAudit() {
	for {
		select {
		case e := <-s.auditCh:
			s.writeAudit(e)
		default:
			return
		}
	}
}

func (s *Store) writeAudit(e AuditEvent) {
	ok := 1
	if !e.OK {
		ok = 0
	}
	_, _ = s.db.Exec(
		`INSERT INTO audit(ts,ip,user,action,target,detail,ok) VALUES(?,?,?,?,?,?,?)`,
		e.TS, e.IP, e.User, e.Action, e.Target, e.Detail, ok)
}

// PurgeAudit 删掉 before 之前的记录，返回删除条数。
// 保留期到了就真删，别只靠列表接口截断——审计表只增不删迟早把盘吃满。
func (s *Store) PurgeAudit(before time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM audit WHERE ts < ?`, before.Unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		log.Printf("审计清理: 删除 %d 条（早于 %s）", n, before.Format("2006-01-02"))
	}
	return n, nil
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
