package store

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openMappingStore(t *testing.T) (*Store, int64) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	id, err := st.AddSource(Source{BaseURL: "https://mirror.example.org", Name: "m", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatalf("加源: %v", err)
	}
	return st, id
}

// 创建/修改/删除都必须同事务递增 Generation 并写审计快照。
func TestTrustedMappingLifecycle(t *testing.T) {
	st, sid := openMappingStore(t)
	host := "quay.io"
	g0, err := st.MappingGeneration(host)
	if err != nil {
		t.Fatalf("初始代次: %v", err)
	}

	g1, err := st.PutTrustedMapping(TrustedMapping{RegistryHost: host, SourceID: sid, Priority: 10, Enabled: true}, "admin", true)
	if err != nil {
		t.Fatalf("新建: %v", err)
	}
	if g1 <= int64(g0) {
		t.Fatalf("新建必须递增代次：%d → %d", g0, g1)
	}
	if _, err := st.PutTrustedMapping(TrustedMapping{RegistryHost: host, SourceID: sid, Priority: 20, Enabled: true}, "admin", true); !errors.Is(err, ErrMappingExists) {
		t.Fatalf("POST 重复必须返回 ErrMappingExists，实得 %v", err)
	}
	g2, err := st.PutTrustedMapping(TrustedMapping{RegistryHost: host, SourceID: sid, Priority: 20, Enabled: false}, "admin", false)
	if err != nil {
		t.Fatalf("修改: %v", err)
	}
	if g2 != g1+1 {
		t.Fatalf("修改应只递增 1：%d → %d", g1, g2)
	}
	g3, err := st.DeleteTrustedMapping(host, sid, "admin")
	if err != nil {
		t.Fatalf("删除: %v", err)
	}
	if g3 != g2+1 {
		t.Fatalf("删除应只递增 1：%d → %d", g2, g3)
	}
	if _, err := st.DeleteTrustedMapping(host, sid, "admin"); !errors.Is(err, ErrMappingMissing) {
		t.Fatalf("重复删除应返回 ErrMappingMissing，实得 %v", err)
	}

	rows, err := st.ListTrustedMappingAudit(host, 50)
	if err != nil {
		t.Fatalf("审计: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("应有三条审计（create/update/delete），实得 %d", len(rows))
	}
	// 最新在前：delete 的 old_snapshot 非空、new_snapshot 为空。
	if rows[0].Operation != "delete" || rows[0].OldSnapshot == "" || rows[0].NewSnapshot != "" {
		t.Fatalf("删除审计快照不对: %+v", rows[0])
	}
	if rows[1].Operation != "update" || rows[1].OldSnapshot == "" || rows[1].NewSnapshot == "" {
		t.Fatalf("修改审计快照不对: %+v", rows[1])
	}
	if rows[2].Operation != "create" || rows[2].OldSnapshot != "" {
		t.Fatalf("新建审计不应有旧快照: %+v", rows[2])
	}
}

// 有效期过滤：立即生效、永久有效、区间内、已到期、未到生效时间。
func TestTrustedMappingValidityWindow(t *testing.T) {
	now := time.Now()
	base := TrustedMapping{Enabled: true}
	cases := []struct {
		name string
		m    TrustedMapping
		want bool
	}{
		{"永久有效", TrustedMapping{Enabled: true}, true},
		{"已到期", TrustedMapping{Enabled: true, ValidUntil: now.Add(-time.Hour).Unix()}, false},
		{"未来生效", TrustedMapping{Enabled: true, ValidFrom: now.Add(time.Hour).Unix()}, false},
		{"区间内", TrustedMapping{Enabled: true, ValidFrom: now.Add(-time.Hour).Unix(), ValidUntil: now.Add(time.Hour).Unix()}, true},
		{"已停用", TrustedMapping{Enabled: false}, false},
	}
	for _, c := range cases {
		base = c.m
		if got := base.ActiveAt(now); got != c.want {
			t.Errorf("%s: ActiveAt=%v want %v", c.name, got, c.want)
		}
	}
}

// Source 删除时映射级联删除，审计历史保留（T58 / AUD-04-2）。
func TestTrustedMappingCascadeKeepsAudit(t *testing.T) {
	st, sid := openMappingStore(t)
	host := "ghcr.io"
	if _, err := st.PutTrustedMapping(TrustedMapping{RegistryHost: host, SourceID: sid, Enabled: true}, "admin", true); err != nil {
		t.Fatalf("新建: %v", err)
	}
	if err := st.DeleteSource(sid); err != nil {
		t.Fatalf("删源: %v", err)
	}
	items, err := st.ListTrustedMappings(host)
	if err != nil {
		t.Fatalf("列表: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("映射应随 Source 级联删除，仍有 %d 行", len(items))
	}
	rows, err := st.ListTrustedMappingAudit(host, 10)
	if err != nil {
		t.Fatalf("审计: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("审计必须保留，实得 %d 行", len(rows))
	}
}

// T58：并发写同一 Host 不得出现 database is locked，且 Generation 与
// 成功写入次数严格一致（每次都同事务 +1）。
func TestTrustedMappingConcurrentWrites(t *testing.T) {
	st, sid := openMappingStore(t)
	host := "registry.k8s.io"
	gen0, err := st.MappingGeneration(host)
	if err != nil {
		t.Fatalf("初始代次: %v", err)
	}
	const workers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	errs := []error{}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := st.PutTrustedMapping(TrustedMapping{RegistryHost: host, SourceID: sid, Priority: i, Enabled: true}, "admin", false)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			ok++
		}(i)
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("并发写失败: %v", errs)
	}
	gen, err := st.MappingGeneration(host)
	if err != nil {
		t.Fatalf("代次: %v", err)
	}
	// 初始行由第一次写入建立（generation=1），本次写入再 +1；
	// 因此基线是 max(初始,1)，每个成功写入恰好 +1。
	baseline := int64(gen0)
	if baseline < 1 {
		baseline = 1
	}
	if int64(gen)-baseline != int64(ok) {
		t.Fatalf("Generation 应为基线+成功次数：%d → %d，成功 %d", baseline, gen, ok)
	}
	if n := len(mustAudit(t, st, host)); n < ok {
		t.Fatalf("每次成功写入都必须有审计：审计 %d < 写入 %d", n, ok)
	}
}

func mustAudit(t *testing.T, st *Store, host string) []TrustedMappingAudit {
	t.Helper()
	rows, err := st.ListTrustedMappingAudit(host, 100)
	if err != nil {
		t.Fatalf("审计: %v", err)
	}
	return rows
}

// 校验错误必须在写库前拒绝：空 Host、负 priority、非法有效期、不存在的 Source。
func TestTrustedMappingValidation(t *testing.T) {
	st, sid := openMappingStore(t)
	cases := []TrustedMapping{
		{RegistryHost: "", SourceID: sid, Enabled: true},
		{RegistryHost: "quay.io", SourceID: 0, Enabled: true},
		{RegistryHost: "quay.io", SourceID: 99999, Enabled: true},
		{RegistryHost: "quay.io", SourceID: sid, Priority: -1, Enabled: true},
		{RegistryHost: "quay.io", SourceID: sid, ValidFrom: 100, ValidUntil: 50, Enabled: true},
	}
	for i, c := range cases {
		if _, err := st.PutTrustedMapping(c, "admin", true); err == nil {
			t.Errorf("用例 %d 应被拒绝: %+v", i, c)
		}
	}
	if items, _ := st.ListTrustedMappings(""); len(items) != 0 {
		t.Fatalf("非法输入不应落库，实得 %d 行", len(items))
	}
}
