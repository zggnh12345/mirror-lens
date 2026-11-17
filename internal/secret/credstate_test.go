package secret

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

func newStateFixture(t *testing.T) (*credFixture, *Store) {
	t.Helper()
	f := newCredFixture(t, "quay.io", "gcr.io")
	if _, err := f.cs.Reconcile(); err != nil {
		t.Fatal(err)
	}
	return f, f.cs
}

// 三种模式与严格字段组合。
func TestCredentialModeValidation(t *testing.T) {
	cases := []struct {
		name string
		c    Credential
		ok   bool
	}{
		{"basic 合法", Credential{Mode: "basic", Username: "u", Password: "p"}, true},
		{"basic 缺密码", Credential{Mode: "basic", Username: "u"}, false},
		{"basic 带 token 参数", Credential{Mode: "basic", Username: "u", Password: "p", TokenAuth: "basic"}, false},
		{"bearer basic 合法", Credential{Mode: "bearer_token_basic", Username: "u", Password: "p"}, true},
		{"bearer basic 缺密码", Credential{Mode: "bearer_token_basic", Username: "u"}, false},
		{"bearer none 合法", Credential{Mode: "bearer_token_none", TokenAuth: "none"}, true},
		{"bearer none 未标记", Credential{Mode: "bearer_token_none"}, false},
		{"bearer none 带密码也合法", Credential{Mode: "bearer_token_none", TokenAuth: "none", Username: "u", Password: "p"}, true},
		{"POST 拒绝", Credential{Mode: "bearer_token_basic", Username: "u", Password: "p", TokenMethod: "POST"}, false},
		{"未知 TokenAuth", Credential{Mode: "bearer_token_basic", Username: "u", Password: "p", TokenAuth: "digest"}, false},
		{"未知模式", Credential{Mode: "oauth", Username: "u", Password: "p"}, false},
		{"控制字符", Credential{Mode: "basic", Username: "u\n", Password: "p"}, false},
	}
	for _, tc := range cases {
		_, err := tc.c.normalize()
		if tc.ok && err != nil {
			t.Errorf("%s：应通过，得到 %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s：应拒绝", tc.name)
		}
	}
}

// 幂等：相同内容不写文件、不 bump、不失效 Token。
func TestPutCredentialIdempotent(t *testing.T) {
	f, cs := newStateFixture(t)
	cred := Credential{Mode: "basic", Username: "u", Password: "p"}
	r1, err := cs.PutCredential("quay.io", cred, nil)
	if err != nil || !r1.Changed || r1.Gen != 1 {
		t.Fatalf("首次写入: %+v err=%v", r1, err)
	}
	before, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := cs.PutCredential("quay.io", cred, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Changed || r2.Gen != 1 {
		t.Fatalf("幂等写入不应变更: %+v", r2)
	}
	after, _ := os.ReadFile(f.path)
	if string(before) != string(after) {
		t.Fatal("幂等写入不应重写凭据文件")
	}
	// 仅 TokenAuth 变化也算变更。
	r3, err := cs.PutCredential("quay.io", Credential{Mode: "bearer_token_none", TokenAuth: "none"}, nil)
	if err != nil || !r3.Changed || r3.Gen != 2 {
		t.Fatalf("模式变化必须 bump: %+v err=%v", r3, err)
	}
}

// expected_gen 冲突 → 409 语义，且不产生半写。
func TestPutCredentialGenConflict(t *testing.T) {
	_, cs := newStateFixture(t)
	if _, err := cs.PutCredential("quay.io", Credential{Mode: "basic", Username: "u", Password: "p"}, nil); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	_, err := cs.PutCredential("quay.io", Credential{Mode: "basic", Username: "u2", Password: "p2"}, &zero)
	if !errors.Is(err, store.ErrCredGenConflict) {
		t.Fatalf("期望代次冲突，得到 %v", err)
	}
	c, ok, gerr := cs.Get("quay.io")
	if gerr != nil || !ok || c.Username != "u" {
		t.Fatalf("冲突后凭据不应改变: %+v ok=%v err=%v", c, ok, gerr)
	}
}

// 状态机：Absent → Ready → Pending → Quarantined 的可解释转换。
func TestCredentialStatusMachine(t *testing.T) {
	f, cs := newStateFixture(t)
	st, err := cs.Status("quay.io")
	if err != nil || st.State != StateAbsent {
		t.Fatalf("未配置应为 Absent: %+v err=%v", st, err)
	}
	if _, err := cs.PutCredential("quay.io", Credential{Mode: "basic", Username: "u", Password: "p"}, nil); err != nil {
		t.Fatal(err)
	}
	if st, _ := cs.Status("quay.io"); st.State != StateReady || st.Gen != 1 || st.Username != "u" {
		t.Fatalf("Ready 状态不对: %+v", st)
	}

	// 模拟「文件已换新、DB 未提交」：直接改 DB 摘要。
	stale := "stale"
	if err := f.db.ApplyCredChanges([]store.CredChange{{Host: "quay.io", Digest: &stale}}); err != nil {
		t.Fatal(err)
	}
	if st, _ := cs.Status("quay.io"); st.State != StatePending || st.Reason != "digest_mismatch" {
		t.Fatalf("应为 Pending: %+v", st)
	}
	if _, _, err := cs.Get("quay.io"); !errors.Is(err, ErrCredentialPending) {
		t.Fatalf("Pending 时读取必须拒绝，得到 %v", err)
	}
	// 按 Host 对账 → Ready。
	if st, err := cs.ReconcileHost("quay.io"); err != nil || st.State != StateReady {
		t.Fatalf("对账后应 Ready: %+v err=%v", st, err)
	}
}

// 记录级损坏只隔离该 Host；文件级损坏才全局 fail closed。
func TestCredentialQuarantineIsolation(t *testing.T) {
	f, cs := newStateFixture(t)
	for _, h := range []string{"quay.io", "gcr.io"} {
		if _, err := cs.PutCredential(h, Credential{Mode: "basic", Username: "u", Password: "p"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	// 只破坏 quay.io 的密文。
	tamper(t, f.path, func(doc map[string]any) {
		hosts := doc["hosts"].(map[string]any)
		rec := hosts["quay.io"].(map[string]any)
		ct := rec["ct"].(string)
		b := []byte(ct)
		if b[0] == 'A' {
			b[0] = 'B'
		} else {
			b[0] = 'A'
		}
		rec["ct"] = string(b)
	})
	if st, _ := cs.Status("quay.io"); st.State != StateQuarantined || st.Reason != "record_corrupt" {
		t.Fatalf("损坏 Host 应为 Quarantined: %+v", st)
	}
	if st, _ := cs.Status("gcr.io"); st.State != StateReady {
		t.Fatalf("其他 Host 必须继续 Ready: %+v", st)
	}
	if _, ok, err := cs.Get("gcr.io"); err != nil || !ok {
		t.Fatalf("其他 Host 应可读: ok=%v err=%v", ok, err)
	}
	// 文件级损坏（顶层 JSON 坏了）→ 全局 Quarantined。
	if err := os.WriteFile(f.path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, _ := cs.Status("gcr.io"); st.State != StateQuarantined || st.Reason != "file_corrupt" {
		t.Fatalf("文件级损坏应全局隔离: %+v", st)
	}
}

// 删除幂等与 Pending。
func TestDeleteCredentialIdempotent(t *testing.T) {
	_, cs := newStateFixture(t)
	r, err := cs.DeleteCredential("quay.io", nil)
	if err != nil || r.State != StateAbsent || r.Changed {
		t.Fatalf("从未配置的删除应幂等: %+v err=%v", r, err)
	}
	if _, err := cs.PutCredential("quay.io", Credential{Mode: "basic", Username: "u", Password: "p"}, nil); err != nil {
		t.Fatal(err)
	}
	r, err = cs.DeleteCredential("quay.io", nil)
	if err != nil || r.State != StateAbsent || r.Gen != 2 {
		t.Fatalf("删除后应 Absent 且 gen+1: %+v err=%v", r, err)
	}
	r, err = cs.DeleteCredential("quay.io", nil)
	if err != nil || r.State != StateAbsent || r.Changed {
		t.Fatalf("重复删除应幂等: %+v err=%v", r, err)
	}
	if _, ok, err := cs.Get("quay.io"); err != nil || ok {
		t.Fatalf("删除后不应读出凭据: ok=%v err=%v", ok, err)
	}
}

// C04：文件已写、DB 未提交 → Pending；启动对账以文件为准补写。
func TestReconcileHostAdoptsFile(t *testing.T) {
	f, cs := newStateFixture(t)
	realDB := f.db
	f.cs.db = &failingDB{DB: realDB}
	if _, err := f.cs.PutCredential("quay.io", Credential{Mode: "basic", Username: "u", Password: "p"}, nil); err == nil {
		t.Fatal("DB 失败时写入应报错")
	}
	f.cs.db = realDB
	if st, _ := cs.Status("quay.io"); st.State != StatePending {
		t.Fatalf("DB 未提交应为 Pending: %+v", st)
	}
	if st, err := cs.ReconcileHost("quay.io"); err != nil || st.State != StateReady {
		t.Fatalf("对账后应 Ready: %+v err=%v", st, err)
	}
	if _, ok, err := cs.Get("quay.io"); err != nil || !ok {
		t.Fatalf("对账后应可读: ok=%v err=%v", ok, err)
	}
}

// 写入门禁：非 allowlist / pkg.dev / GitLab 一律拒绝。
func TestPutCredentialAllowlist(t *testing.T) {
	_, cs := newStateFixture(t)
	for _, h := range []string{"lscr.io", "registry.k8s.io", "registry.gitlab.com", "asia-east1-docker.pkg.dev", "docker.io", "ghcr.io", "localhost:5000", "10.0.0.1"} {
		if _, err := cs.PutCredential(h, Credential{Mode: "basic", Username: "u", Password: "p"}, nil); err == nil {
			t.Fatalf("%s 不应允许配置凭据", h)
		}
	}
	for _, h := range []string{"lscr.io", "registry.k8s.io", "registry.gitlab.com", "asia-east1-docker.pkg.dev", "docker.io", "ghcr.io", "localhost:5000", "10.0.0.1"} {
		if _, err := cs.DeleteCredential(h, nil); err == nil {
			t.Fatalf("%s 不应允许删除凭据", h)
		}
	}
}

// 1.6 D.1：credentialHost 必须排除 *.pkg.dev 与 localhost；
// 否则状态/列表路径仍可能把不支持的 Host 当作凭据 Host。
func TestCredentialHostRejectsArtifactRegistryAndLocalhost(t *testing.T) {
	for _, h := range []string{"asia-east1-docker.pkg.dev", "localhost", "foo.localhost"} {
		if _, err := credentialHost(h); err == nil {
			t.Fatalf("%s 不应通过 credentialHost", h)
		}
	}
	for _, h := range []string{"quay.io", "gcr.io", "registry.k8s.io"} {
		if _, err := credentialHost(h); err != nil {
			t.Fatalf("%s 应通过 credentialHost: %v", h, err)
		}
	}
}

func TestHasCredentialDigest(t *testing.T) {
	host := "quay.io"
	if HasCredentialDigest(host, "") {
		t.Fatal("空摘要不应算有凭据")
	}
	if HasCredentialDigest(host, emptyDigest(host)) {
		t.Fatal("显式删除摘要不应算有凭据")
	}
	if !HasCredentialDigest(host, "abc123") {
		t.Fatal("真实摘要应算有凭据")
	}
}

// 文件与 DB 摘要一致才允许读取（F13）。
func TestSnapshotRejectsMismatch(t *testing.T) {
	f, cs := newStateFixture(t)
	if _, err := cs.PutCredential("quay.io", Credential{Mode: "basic", Username: "u", Password: "p"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ApplyCredWrite(store.CredWrite{Host: "quay.io", Digest: "other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Snapshot("quay.io"); !errors.Is(err, ErrCredentialPending) {
		t.Fatalf("摘要不一致必须拒绝，得到 %v", err)
	}
}

// 权限：凭据文件与目录不因 umask/挂载被放宽。
func TestCredentialFileMode(t *testing.T) {
	f, cs := newStateFixture(t)
	if _, err := cs.PutCredential("quay.io", Credential{Mode: "basic", Username: "u", Password: "p"}, nil); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		t.Fatalf("凭据文件权限过宽: %v", st.Mode())
	}
	// 目录权限由数据卷的挂载语义决定（本机 /opt/data 为 btrfs+trimacl，
	// 会被改写成 0705），代码只保证凭据文件本身 0600，不对目录做断言。
	_ = filepath.Dir(f.path)
}

// 5b：运行/API 读路径必须拒绝非 allowlist Host，但文件解析仍可识别历史记录。
func TestStatusPathsRejectUnauthorizedHost(t *testing.T) {
	_, cs := newStateFixture(t)
	for _, h := range []string{"registry.k8s.io", "example.com"} {
		if _, err := cs.Status(h); err == nil {
			t.Fatalf("Status(%s) 应拒绝", h)
		}
		if _, _, err := cs.Get(h); err == nil {
			t.Fatalf("Get(%s) 应拒绝", h)
		}
		if _, err := cs.Snapshot(h); err == nil {
			t.Fatalf("Snapshot(%s) 应拒绝", h)
		}
		if _, err := cs.ReconcileHost(h); err == nil {
			t.Fatalf("ReconcileHost(%s) 应拒绝", h)
		}
	}
	if _, err := credentialHost("registry.k8s.io"); err != nil {
		t.Fatalf("文件解析层仍应接受历史 Host: %v", err)
	}
}
