package api

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/config"
	"gopkg.in/yaml.v3"
)

func newCfgServer(t *testing.T, path string) *Server {
	t.Helper()
	s := &Server{cfgPath: path}
	s.cfg.Store(config.Default())
	return s
}

func writeInitialConfig(t *testing.T, path string) {
	t.Helper()
	out, err := yaml.Marshal(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

// 两个并发 PUT 改不同字段，文件和运行态都必须保留两个修改。
func TestSaveConfigConcurrentNoLostUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeInitialConfig(t, path)
	s := newCfgServer(t, path)

	const rounds = 10
	var wg sync.WaitGroup
	errs := make(chan error, rounds*2)
	for i := 0; i < rounds; i++ {
		wg.Add(2)
		go func(n int) {
			defer wg.Done()
			if _, _, err := s.saveConfig(map[string]string{"cache.max_gb": fmt.Sprint(200 + n)}); err != nil {
				errs <- err
			}
		}(i)
		go func(n int) {
			defer wg.Done()
			if _, _, err := s.saveConfig(map[string]string{"raw.ttl": fmt.Sprintf("%dm", 45+n)}); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发保存失败: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got config.Config
	if err := yaml.Unmarshal(b, &got); err != nil {
		t.Fatalf("落盘配置无法解析: %v", err)
	}
	def := config.Default()
	// 写入值必须全部避开默认值：n=0 那轮曾与默认值重合，
	// 最后落定若是它，会被误报成 lost update。
	if got.Cache.MaxGB == def.Cache.MaxGB || got.Cache.MaxGB < 200 || got.Cache.MaxGB > 209 {
		t.Fatalf("cache.max_gb 不是任何一次合法写入: %d", got.Cache.MaxGB)
	}
	if got.Raw.TTL.Duration == def.Raw.TTL.Duration || got.Raw.TTL.Duration < 45*60e9 || got.Raw.TTL.Duration > 54*60e9 {
		t.Fatalf("raw.ttl 不是任何一次合法写入: %v", got.Raw.TTL.Duration)
	}
	live := s.cfg.Load()
	if live.Cache.MaxGB != got.Cache.MaxGB || live.Raw.TTL.Duration != got.Raw.TTL.Duration {
		t.Fatalf("运行态与磁盘不一致: live=%d/%v disk=%d/%v",
			live.Cache.MaxGB, live.Raw.TTL.Duration, got.Cache.MaxGB, got.Raw.TTL.Duration)
	}
}

// 非空但解析失败的文件必须原样保留，且不发布新运行态。
func TestSaveConfigRejectsBrokenYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	bad := []byte("cache: [unclosed\n")
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	s := newCfgServer(t, path)
	before := s.cfg.Load().Cache.MaxGB

	_, _, err := s.saveConfig(map[string]string{"cache.max_gb": "100"})
	if err == nil {
		t.Fatal("损坏配置竟被写入覆盖")
	}
	if !strings.Contains(err.Error(), "拒绝写入") {
		t.Fatalf("错误信息应说明拒绝写入: %v", err)
	}
	after, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !bytes.Equal(after, bad) {
		t.Fatalf("原文件被改动:\n%s", after)
	}
	if s.cfg.Load().Cache.MaxGB != before {
		t.Fatal("运行态被错误发布")
	}
}

// 合法 YAML 但字段类型错误，同样拒绝。
func TestSaveConfigRejectsTypeError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	bad := []byte("cache:\n  max_gb: notanint\n")
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	s := newCfgServer(t, path)
	if _, _, err := s.saveConfig(map[string]string{"cache.max_gb": "100"}); err == nil {
		t.Fatal("类型错误配置竟被写入覆盖")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(after, bad) {
		t.Fatalf("原文件被改动:\n%s", after)
	}
}

// 空文件视为损坏：拒绝写入，且原文件保持 0 字节（不能被默认值覆盖）。
func TestSaveConfigRejectsEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := newCfgServer(t, path)

	_, _, err := s.saveConfig(map[string]string{"cache.max_gb": "100"})
	if err == nil {
		t.Fatal("空文件应被拒绝，而不是用默认值重建")
	}
	if !strings.Contains(err.Error(), "为空") {
		t.Fatalf("错误信息应说明文件为空: %v", err)
	}
	st, serr := os.Stat(path)
	if serr != nil {
		t.Fatal(serr)
	}
	if st.Size() != 0 {
		t.Fatalf("空文件被改写，大小 %d", st.Size())
	}
}

// 落盘文件权限必须是 0600。
func TestSaveConfigFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeInitialConfig(t, path)
	s := newCfgServer(t, path)
	if _, _, err := s.saveConfig(map[string]string{"cache.max_gb": "101"}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("权限应为 0600，实际 %o", perm)
	}
}

// rename 失败：原文件不变，运行态不发布。
func TestSaveConfigRenameFailureKeepsOldState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeInitialConfig(t, path)
	s := newCfgServer(t, path)

	before, _ := os.ReadFile(path)
	liveBefore := s.cfg.Load().Cache.MaxGB

	restore := config.SetRenameFunc(func(a, b string) error { return fmt.Errorf("injected rename failure") })
	defer restore()

	if _, _, err := s.saveConfig(map[string]string{"cache.max_gb": "102"}); err == nil {
		t.Fatal("rename 失败应返回错误")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(after, before) {
		t.Fatalf("rename 失败后原文件被改动:\n%s", after)
	}
	if s.cfg.Load().Cache.MaxGB != liveBefore {
		t.Fatal("rename 失败后运行态被发布")
	}
}

// Node 级写盘：注释和面板不认识的未来字段都要保留。
func TestSaveConfigKeepsCommentsAndUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	src := "# 手写注释\ncache:\n  max_gb: 50 # 行尾注释\nfuture_key: 42\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newCfgServer(t, path)
	if _, _, err := s.saveConfig(map[string]string{"cache.max_gb": "60"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	text := string(b)
	for _, want := range []string{"# 手写注释", "# 行尾注释", "future_key: 42", "max_gb: 60"} {
		if !strings.Contains(text, want) {
			t.Fatalf("落盘内容缺少 %q:\n%s", want, text)
		}
	}
}

// 超过 1 MiB 的请求体回 413，而不是混成 400。
func TestConfigPutBodyTooLarge(t *testing.T) {
	s := newCfgServer(t, filepath.Join(t.TempDir(), "config.yaml"))
	body := `{"values":{"cache.max_gb":"` + strings.Repeat("a", 2<<20) + `"}}`
	req := httptest.NewRequest(http.MethodPut, "/api/config", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.configHandler(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码应为 413，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// 面板保存后文件应包含全部生效参数，而不是只写改动的那一项。
func TestSaveConfigWritesFullConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("cache:\n  max_gb: 10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newCfgServer(t, path)

	if _, _, err := s.saveConfig(map[string]string{"cache.max_gb": "20"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"addr:", "hedge:", "parallel:", "limits:", "logging:"} {
		if !strings.Contains(string(b), key) {
			t.Fatalf("保存后缺少 %q：\n%s", key, b)
		}
	}
	var got config.Config
	if err := yaml.Unmarshal(b, &got); err != nil {
		t.Fatalf("保存后的文件无法解析: %v", err)
	}
	if got.Cache.MaxGB != 20 {
		t.Fatalf("max_gb=%d，期望 20", got.Cache.MaxGB)
	}
	if got.Addr != config.Default().Addr {
		t.Fatalf("addr=%q，期望默认值", got.Addr)
	}
}
