package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/auth"
	"github.com/zggnh12345/mirror-lens/internal/cache"
	"github.com/zggnh12345/mirror-lens/internal/gateway"
	"github.com/zggnh12345/mirror-lens/internal/runner"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

type readyFixture struct {
	st     *store.Store
	gw     *gateway.Gateway
	h      http.Handler
	cacheD string
}

func newReadyFixture(t *testing.T) *readyFixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := auth.EnsureAdmin(st); err != nil {
		t.Fatal(err)
	}
	cacheD := filepath.Join(dir, "cache")
	cs, err := cache.New(cacheD)
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(st, gateway.Options{Cache: cs, RawBase: "http://127.0.0.1:1"})
	return &readyFixture{st: st, gw: gw, h: New(st, runner.New(st), gw, nil, nil), cacheD: cacheD}
}

func (f *readyFixture) get(t *testing.T) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v\n%s", err, rec.Body.String())
	}
	return rec, body
}

func checkOf(t *testing.T, body map[string]any, name string) map[string]any {
	t.Helper()
	checks, _ := body["checks"].(map[string]any)
	c, ok := checks[name].(map[string]any)
	if !ok {
		t.Fatalf("缺少 %s 检查项: %v", name, body)
	}
	return c
}

func TestReadyzOK(t *testing.T) {
	f := newReadyFixture(t)
	rec, body := f.get(t)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if body["status"] != "ok" {
		t.Fatalf("status 应为 ok: %v", body)
	}
	if checkOf(t, body, "db")["ok"] != true || checkOf(t, body, "cache")["ok"] != true {
		t.Fatalf("两项都该通过: %v", body)
	}
}

// 公网源站不可用不该影响就绪：/readyz 根本不问源站。
func TestReadyzIgnoresUpstream(t *testing.T) {
	f := newReadyFixture(t)
	// 放一台连不上的源（127.0.0.1:1），确认它不参与就绪判断。
	if _, err := f.st.AddSource(store.Source{
		Name: "dead", BaseURL: "http://127.0.0.1:1", Origin: "manual", Enabled: true, Status: "usable",
	}); err != nil {
		t.Fatal(err)
	}
	rec, _ := f.get(t)
	if rec.Code != http.StatusOK {
		t.Fatalf("源站不可达时仍应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
}

func TestReadyzDBBroken(t *testing.T) {
	f := newReadyFixture(t)
	f.st.Close()
	rec, body := f.get(t)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("库坏了应 503，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if checkOf(t, body, "db")["ok"] != false {
		t.Fatalf("db 应标失败: %v", body)
	}
	if checkOf(t, body, "cache")["ok"] != true {
		t.Fatalf("缓存没问题，不该跟着报错: %v", body)
	}
	// 错误信息不能把内部路径或 SQL 原文漏出去。
	if msg, _ := checkOf(t, body, "db")["error"].(string); msg != "数据库不可写" {
		t.Fatalf("db 错误信息应固定为「数据库不可写」，得到 %q", msg)
	}
}

func TestReadyzCacheGone(t *testing.T) {
	f := newReadyFixture(t)
	if err := os.RemoveAll(f.cacheD); err != nil {
		t.Fatal(err)
	}
	rec, body := f.get(t)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("缓存目录没了应 503，得到 %d", rec.Code)
	}
	if checkOf(t, body, "cache")["ok"] != false {
		t.Fatalf("cache 应标失败: %v", body)
	}
}

// 探针不能每次写临时文件：K8s 一秒一次，目录会被小文件刷满。
func TestReadyzDoesNotWriteFiles(t *testing.T) {
	f := newReadyFixture(t)
	count := func() int {
		ents, err := os.ReadDir(f.cacheD)
		if err != nil {
			t.Fatal(err)
		}
		return len(ents)
	}
	before := count()
	for i := 0; i < 20; i++ {
		if rec, _ := f.get(t); rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次探针失败: %d", i, rec.Code)
		}
	}
	if after := count(); after != before {
		t.Fatalf("探针在缓存目录里留了文件：%d → %d", before, after)
	}
}
