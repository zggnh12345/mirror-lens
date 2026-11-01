package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// 老库（阶段 4 及以前）的流量表没有 kind 列，主键也不一样。
// 升级要保留数据、补空 kind，并把历史 proxy 改名为 bypass
// ——老代码只在「不落盘直接透传」的三条路径上记 proxy。
func TestMigrateTrafficKind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("建库: %v", err)
	}
	for _, q := range []string{
		`CREATE TABLE traffic_stats(day TEXT NOT NULL, traffic TEXT NOT NULL,
			n INTEGER NOT NULL DEFAULT 0, bytes INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY(day, traffic))`,
		`INSERT INTO traffic_stats(day,traffic,n,bytes) VALUES('2026-10-01','proxy',3,300)`,
		`INSERT INTO traffic_stats(day,traffic,n,bytes) VALUES('2026-10-01','cache_hit',5,500)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("造旧表: %v", err)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("打开老库: %v", err)
	}
	defer st.Close()

	rows, err := st.TrafficByDay(3650)
	if err != nil {
		t.Fatalf("读流量: %v", err)
	}
	got := map[string]int64{}
	for _, r := range rows {
		if r.Day != "2026-10-01" {
			continue
		}
		if r.Kind != "" {
			t.Fatalf("历史记录 kind 应为空，实际 %q", r.Kind)
		}
		got[r.Traffic] = r.Bytes
	}
	if got["bypass"] != 300 {
		t.Fatalf("proxy 应迁移成 bypass(300)，实际 %v", got)
	}
	if got["cache_hit"] != 500 {
		t.Fatalf("cache_hit 数据丢了: %v", got)
	}

	// 新记录按 kind 分开累加，同一天同一类型不同协议不能互相覆盖
	st.RecordTraffic(TrafficHit, KindRaw, 100)
	st.RecordTraffic(TrafficHit, KindDocker, 200)
	st.RecordTraffic(TrafficHit, KindRaw, 50)
	rows, _ = st.TrafficByDay(3650)
	var raw, docker int64
	for _, r := range rows {
		if r.Traffic != TrafficHit {
			continue
		}
		switch r.Kind {
		case KindRaw:
			raw += r.Bytes
		case KindDocker:
			docker += r.Bytes
		}
	}
	if raw != 150 || docker != 200 {
		t.Fatalf("按协议统计不对: raw=%d docker=%d", raw, docker)
	}
}
