package api

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/auth"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/gateway"
	"github.com/zggnh12345/mirror-lens/internal/runner"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// 12-F2：PATCH 源返回后，下一次调度必须看到新状态，不能等 ListTTL。
func TestSourcePatchInvalidatesSchedulerCache(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := auth.EnsureAdmin(st); err != nil {
		t.Fatal(err)
	}

	sch := scheduler.New(st, scheduler.Config{Enabled: true, ListTTL: 60 * time.Second, MaxCandidates: 3})
	eng := runner.New(st)
	eng.OnChange(sch.Invalidate)
	cs, err := cache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(st, gateway.Options{Cache: cs})
	h := New(st, eng, gw, nil, sch)
	ck := setupPanel(t, h)

	id, err := st.AddSource(store.Source{
		Name: "a", BaseURL: "http://127.0.0.1:1", Status: "usable",
		Enabled: true, DockerCap: store.CapYes,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 先取一次，把候选列表放进缓存。
	first := sch.Candidates("docker")
	if len(first) != 1 || first[0].ID != id {
		t.Fatalf("初始候选不对: %+v", first)
	}

	rec := patchJSON(h, "/api/sources/"+itoa(id), `{"enabled":false}`, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("停用应 200，得到 %d %s", rec.Code, rec.Body.String())
	}
	if got := sch.Candidates("docker"); len(got) != 0 {
		t.Fatalf("停用后下一次调度应立即排除该源，得到 %+v", got)
	}

	rec = patchJSON(h, "/api/sources/"+itoa(id), `{"enabled":true}`, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("启用应 200，得到 %d %s", rec.Code, rec.Body.String())
	}
	if got := sch.Candidates("docker"); len(got) != 1 || got[0].ID != id {
		t.Fatalf("启用后下一次调度应立即看到该源，得到 %+v", got)
	}
}
