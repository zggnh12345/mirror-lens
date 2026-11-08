package metrics

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// 3.0 的 Lease 观测。口径见 docs/concurrency.md：
//   - leases_held 计 Reserved + Active + Reclaiming（准入单位）
//   - a_bodies_active 只计 A 档尚未退出的上游 Body，含 Reclaiming
//   - 两者在交接重叠窗口内可以相差 1，不是恒等式

// LeaseGauge 刷新某源的状态分项快照。
func LeaseGauge(id int64, held, reserved, active, reclaiming, bodies int) {
	r := current.Load()
	if r == nil || id == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leaseHeld[id] = held
	r.leaseReserved[id] = reserved
	r.leaseActive[id] = active
	r.leaseReclaiming[id] = reclaiming
	r.leaseBodies[id] = bodies
}

// LeaseCapHit 记一次撞上限：handover=true 时撞的是 hard_max。
func LeaseCapHit(id int64, handover bool) {
	r := current.Load()
	if r == nil || id == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if handover {
		r.leaseCapHand[id]++
		return
	}
	r.leaseCapNormal[id]++
}

// LeaseTransfer 记一次同源接管。
func LeaseTransfer(id int64, ok bool) {
	r := current.Load()
	if r == nil || id == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if ok {
		r.leaseTransfer[id]++
	} else {
		r.leaseTransferF[id]++
	}
}

// LeaseReleased 记一次真正归还名额。
func LeaseReleased(id int64, handover bool) {
	r := current.Load()
	if r == nil || id == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leaseReclaimed[id]++
	if handover {
		r.leaseRelHand[id]++
	}
}

// LeaseReclaim 记一次回收：reason 是封闭集合（stall/cancel/timeout）。
func LeaseReclaim(id int64, reason string, immediate bool) {
	r := current.Load()
	if r == nil || id == 0 {
		return
	}
	if reason == "" {
		reason = "other"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leaseReclaimReason[reason]++
	if !immediate {
		r.leaseStuck[id]++
	}
}

// LeaseStuckCleared 表示一个曾经滞留的回收终于完成。
func LeaseStuckCleared(id int64) {
	r := current.Load()
	if r == nil || id == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.leaseStuck[id] > 0 {
		r.leaseStuck[id]--
	}
}

// LeaseWait 记一次真实排队并最终拿到名额的时长。
func LeaseWait(id int64, d time.Duration) {
	r := current.Load()
	if r == nil || id == 0 || d <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.leaseWait[id]
	if h == nil {
		h = newHist(durBuckets)
		r.leaseWait[id] = h
	}
	h.observe(d.Seconds(), durBuckets)
}

// LeaseWaitTimeout 记一次有限等待超时。
func LeaseWaitTimeout(id int64) {
	r := current.Load()
	if r == nil || id == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leaseWaitTO[id]++
}

// writeLease 输出 3.0 的租约指标。
func (r *Registry) writeLease(b *strings.Builder) {
	if len(r.leaseHeld) == 0 {
		return
	}
	ids := make([]int64, 0, len(r.leaseHeld))
	for id := range r.leaseHeld {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	b.WriteString("# HELP " + prefix + "leases_held 源站当前持有的回源租约数（reserved+active+reclaiming）\n")
	b.WriteString("# TYPE " + prefix + "leases_held gauge\n")
	for _, id := range ids {
		fmt.Fprintf(b, "%sleases_held{source_id=%q} %d\n", prefix, strconv.FormatInt(id, 10), r.leaseHeld[id])
	}

	b.WriteString("# HELP " + prefix + "a_bodies_active A 档尚未退出的上游响应体数（含回收中）\n")
	b.WriteString("# TYPE " + prefix + "a_bodies_active gauge\n")
	for _, id := range ids {
		fmt.Fprintf(b, "%sa_bodies_active{source_id=%q} %d\n", prefix, strconv.FormatInt(id, 10), r.leaseBodies[id])
	}

	b.WriteString("# HELP " + prefix + "leases_state 按状态分解的租约数\n")
	b.WriteString("# TYPE " + prefix + "leases_state gauge\n")
	for _, id := range ids {
		sid := strconv.FormatInt(id, 10)
		fmt.Fprintf(b, "%sleases_state{source_id=%q,state=\"reserved\"} %d\n", prefix, sid, r.leaseReserved[id])
		fmt.Fprintf(b, "%sleases_state{source_id=%q,state=\"active\"} %d\n", prefix, sid, r.leaseActive[id])
		fmt.Fprintf(b, "%sleases_state{source_id=%q,state=\"reclaiming\"} %d\n", prefix, sid, r.leaseReclaiming[id])
	}

	type counter struct {
		name string
		help string
		m    map[int64]uint64
	}
	for _, c := range []counter{
		{"lease_cap_hit_total", "撞普通派发上限的次数", r.leaseCapNormal},
		{"lease_cap_hit_handover_total", "撞硬上限的交接次数", r.leaseCapHand},
		{"lease_transfer_total", "同源接管成功次数", r.leaseTransfer},
		{"lease_transfer_failed_total", "同源接管失败次数", r.leaseTransferF},
		{"lease_released_total", "真正归还名额的次数", r.leaseReclaimed},
		{"lease_wait_timeout_total", "有限等待超时次数", r.leaseWaitTO},
	} {
		if len(c.m) == 0 {
			continue
		}
		b.WriteString("# HELP " + prefix + c.name + " " + c.help + "\n")
		b.WriteString("# TYPE " + prefix + c.name + " counter\n")
		k := make([]int64, 0, len(c.m))
		for id := range c.m {
			k = append(k, id)
		}
		sort.Slice(k, func(i, j int) bool { return k[i] < k[j] })
		for _, id := range k {
			fmt.Fprintf(b, "%s%s{source_id=%q} %d\n", prefix, c.name, strconv.FormatInt(id, 10), c.m[id])
		}
	}

	if len(r.leaseWait) > 0 {
		wids := make([]int64, 0, len(r.leaseWait))
		for id := range r.leaseWait {
			wids = append(wids, id)
		}
		sort.Slice(wids, func(i, j int) bool { return wids[i] < wids[j] })
		b.WriteString("# HELP " + prefix + "lease_wait_seconds 普通请求等待名额的时长（秒）\n")
		b.WriteString("# TYPE " + prefix + "lease_wait_seconds histogram\n")
		for _, id := range wids {
			writeHistBody(b, prefix+"lease_wait_seconds", "source_id", strconv.FormatInt(id, 10), r.leaseWait[id], durBuckets)
		}
	}

	if len(r.leaseStuck) > 0 {
		sids := make([]int64, 0, len(r.leaseStuck))
		for id, n := range r.leaseStuck {
			if n > 0 {
				sids = append(sids, id)
			}
		}
		sort.Slice(sids, func(i, j int) bool { return sids[i] < sids[j] })
		if len(sids) > 0 {
			b.WriteString("# HELP " + prefix + "lease_reclaim_stuck 回收中但 Body 尚未退出的租约数\n")
			b.WriteString("# TYPE " + prefix + "lease_reclaim_stuck gauge\n")
			for _, id := range sids {
				fmt.Fprintf(b, "%slease_reclaim_stuck{source_id=%q} %d\n", prefix, strconv.FormatInt(id, 10), r.leaseStuck[id])
			}
		}
	}
}

// B 档 / 预检的进程级指标：没有 source_id 维度，量小直接原子计数。
var (
	preflightInFlight atomic.Int64
	preflightSkipped  atomic.Uint64
	gitInFlight       atomic.Int64
	relayInFlight     atomic.Int64
)

func PreflightInflight(d int64) {
	for {
		cur := preflightInFlight.Load()
		next := cur + d
		if next < 0 {
			next = 0
		}
		if preflightInFlight.CompareAndSwap(cur, next) {
			return
		}
	}
}

func PreflightCapped() { preflightSkipped.Add(1) }

func GitInflight(d int64) {
	for {
		cur := gitInFlight.Load()
		next := cur + d
		if next < 0 {
			next = 0
		}
		if gitInFlight.CompareAndSwap(cur, next) {
			return
		}
	}
}

func RelayInflight(d int64) {
	for {
		cur := relayInFlight.Load()
		next := cur + d
		if next < 0 {
			next = 0
		}
		if relayInFlight.CompareAndSwap(cur, next) {
			return
		}
	}
}

func (r *Registry) writeQuota(b *strings.Builder) {
	b.WriteString("# HELP " + prefix + "preflight_inflight 预检在途请求数\n")
	b.WriteString("# TYPE " + prefix + "preflight_inflight gauge\n")
	fmt.Fprintf(b, "%spreflight_inflight %d\n", prefix, preflightInFlight.Load())
	b.WriteString("# HELP " + prefix + "preflight_capped_total 因预检名额不足而未预检的源数\n")
	b.WriteString("# TYPE " + prefix + "preflight_capped_total counter\n")
	fmt.Fprintf(b, "%spreflight_capped_total %d\n", prefix, preflightSkipped.Load())
	b.WriteString("# HELP " + prefix + "git_inflight Git upload-pack 在途数\n")
	b.WriteString("# TYPE " + prefix + "git_inflight gauge\n")
	fmt.Fprintf(b, "%sgit_inflight %d\n", prefix, gitInFlight.Load())
	b.WriteString("# HELP " + prefix + "relay_inflight 信任中转在途数\n")
	b.WriteString("# TYPE " + prefix + "relay_inflight gauge\n")
	fmt.Fprintf(b, "%srelay_inflight %d\n", prefix, relayInFlight.Load())
}
