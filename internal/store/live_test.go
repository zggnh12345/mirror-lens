package store

import "testing"

func TestLiveSpeedTableCreated(t *testing.T) {
	s, err := Open(t.TempDir() + "/x.db")
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	defer s.Close()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='live_speed'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("live_speed 表没建出来")
	}
}
