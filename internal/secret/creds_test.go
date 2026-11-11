package secret

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

type credFixture struct {
	cs     *Store
	db     *store.Store
	path   string
	source map[string]int64
}

func newCredFixture(t *testing.T, hosts ...string) *credFixture {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "ml.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	key, err := LoadOrCreateKey(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "registry_creds.enc")
	cs, err := NewCredStore(path, key, db)
	if err != nil {
		t.Fatal(err)
	}
	f := &credFixture{cs: cs, db: db, path: path, source: map[string]int64{}}
	for _, h := range hosts {
		id, err := db.AddSource(store.Source{
			Name:     strings.ReplaceAll(h, ".", "-"),
			BaseURL:  "https://" + strings.ReplaceAll(h, ".", "-") + ".example",
			Status:   "usable",
			RepoHost: h,
		})
		if err != nil {
			t.Fatal(err)
		}
		f.source[h] = id
	}
	return f
}

func (f *credFixture) mustPut(t *testing.T, host, user, pass string) string {
	t.Helper()
	d, err := f.cs.Put(host, user, pass)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (f *credFixture) capOf(t *testing.T, host string) int {
	t.Helper()
	x, err := f.db.GetSource(f.source[host])
	if err != nil {
		t.Fatal(err)
	}
	return x.RegistryCap
}

func (f *credFixture) genOf(t *testing.T, host string) (int64, string) {
	t.Helper()
	g, d, err := f.db.HostGenState(host)
	if err != nil {
		t.Fatal(err)
	}
	return g, d
}

func TestCredsRoundTrip(t *testing.T) {
	f := newCredFixture(t, "quay.io")
	if _, _, err := f.cs.Get("quay.io"); !errors.Is(err, ErrNotReconciled) {
		t.Fatalf("对账前读凭据应被拒绝，得到 %v", err)
	}
	if n, err := f.cs.Reconcile(); err != nil || n != 0 {
		t.Fatalf("空文件对账应无变更: n=%d err=%v", n, err)
	}
	digest := f.mustPut(t, "quay.io", "robot", "p@ss-w0rd")

	c, ok, err := f.cs.Get("quay.io")
	if err != nil || !ok {
		t.Fatalf("读回失败: ok=%v err=%v", ok, err)
	}
	if c.Username != "robot" || c.Password != "p@ss-w0rd" {
		t.Fatalf("凭据内容不对: %+v", c)
	}
	raw, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "p@ss-w0rd") || strings.Contains(string(raw), "robot") {
		t.Fatal("凭据文件里出现了明文")
	}
	fi, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("凭据文件权限 %o，应为 600", fi.Mode().Perm())
	}
	gen, dbDigest := f.genOf(t, "quay.io")
	if gen != 1 || dbDigest != digest {
		t.Fatalf("版本未提交: gen=%d digest=%s want %s", gen, dbDigest, digest)
	}
	if digest == emptyDigest("quay.io") {
		t.Fatal("有凭据的摘要不应等于空凭据摘要")
	}
	// 已提交状态下再对账不应产生变更。
	if n, err := f.cs.Reconcile(); err != nil || n != 0 {
		t.Fatalf("重复对账不应有变更: n=%d err=%v", n, err)
	}
	if ok, user, _ := f.cs.Has("quay.io"); !ok || user != "robot" {
		t.Fatalf("Has 结果不对: %v %q", ok, user)
	}
}

// 逐 Host 摘要：改 B 不能影响 A 的 gen / 摘要 / 能力状态。
func TestCredsPerHostIsolation(t *testing.T) {
	f := newCredFixture(t, "quay.io", "lscr.io")
	if _, err := f.cs.Reconcile(); err != nil {
		t.Fatal(err)
	}
	f.mustPut(t, "quay.io", "u1", "p1")
	f.mustPut(t, "lscr.io", "u2", "p2")
	genQ, digQ := f.genOf(t, "quay.io")

	if err := f.db.ApplyCredChanges([]store.CredChange{{Host: "lscr.io", Digest: strPtr(f.mustPut(t, "lscr.io", "u2", "p2-new"))}}); err != nil {
		t.Fatal(err)
	}
	genQ2, digQ2 := f.genOf(t, "quay.io")
	if genQ != genQ2 || digQ != digQ2 {
		t.Fatalf("改 lscr 影响了 quay 的版本: (%d,%s) -> (%d,%s)", genQ, digQ, genQ2, digQ2)
	}
	// quay 的凭据仍然可读，说明其摘要未被打乱。
	if c, ok, err := f.cs.Get("quay.io"); err != nil || !ok || c.Password != "p1" {
		t.Fatalf("quay 凭据被影响: %+v ok=%v err=%v", c, ok, err)
	}
}

func TestCredsCorruptRejected(t *testing.T) {
	f := newCredFixture(t, "quay.io")
	if _, err := f.cs.Reconcile(); err != nil {
		t.Fatal(err)
	}
	digest := f.mustPut(t, "quay.io", "u", "p")
	gen, _ := f.genOf(t, "quay.io")

	tamper(t, f.path, func(doc map[string]any) {
		hosts := doc["hosts"].(map[string]any)
		rec := hosts["quay.io"].(map[string]any)
		ct := rec["ct"].(string)
		// 改一个字符但保持合法 base64：AEAD 必须失败。
		b := []byte(ct)
		if b[0] == 'A' {
			b[0] = 'B'
		} else {
			b[0] = 'A'
		}
		rec["ct"] = string(b)
	})

	if _, err := f.cs.Reconcile(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("AEAD 损坏应返回 ErrCorrupt，得到 %v", err)
	}
	g2, d2 := f.genOf(t, "quay.io")
	if g2 != gen || d2 != digest {
		t.Fatalf("损坏文件不应更新 DB: (%d,%s) -> (%d,%s)", gen, digest, g2, d2)
	}
	if _, _, err := f.cs.Get("quay.io"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("损坏文件不应读出凭据，得到 %v", err)
	}
	// 写入也必须先校验现有文件，不能把损坏的部分覆盖掉当没事。
	if _, err := f.cs.Put("lscr.io", "u", "p"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("文件损坏时写入应失败，得到 %v", err)
	}
}

func TestCredsMissingRecordRejected(t *testing.T) {
	f := newCredFixture(t, "quay.io", "lscr.io")
	if _, err := f.cs.Reconcile(); err != nil {
		t.Fatal(err)
	}
	digest := f.mustPut(t, "quay.io", "u", "p")
	f.mustPut(t, "lscr.io", "u2", "p2")
	gen, _ := f.genOf(t, "quay.io")

	// 模拟文件被截断/从旧备份恢复：quay 的记录凭空消失。
	tamper(t, f.path, func(doc map[string]any) {
		delete(doc["hosts"].(map[string]any), "quay.io")
	})
	if _, err := f.cs.Reconcile(); !errors.Is(err, ErrRecordMissing) {
		t.Fatalf("记录丢失应返回 ErrRecordMissing，得到 %v", err)
	}
	g2, d2 := f.genOf(t, "quay.io")
	if g2 != gen || d2 != digest {
		t.Fatalf("记录丢失不应被当成删除: (%d,%s) -> (%d,%s)", gen, digest, g2, d2)
	}
	// 也不能用 Delete 把它「补删」掉。
	if err := f.cs.Delete("quay.io"); !errors.Is(err, ErrRecordMissing) {
		t.Fatalf("Delete 缺失记录应报错，得到 %v", err)
	}
}

func TestCredsExplicitDelete(t *testing.T) {
	f := newCredFixture(t, "quay.io")
	if _, err := f.cs.Reconcile(); err != nil {
		t.Fatal(err)
	}
	f.mustPut(t, "quay.io", "u", "p")
	gen, _ := f.genOf(t, "quay.io")
	// 先放一个能力结论，删除后必须被清空。
	if ok, err := f.db.SetHostCaps(f.source["quay.io"], "quay.io", store.RegistryCapYes,
		store.PathNative, 100, "", gen, mustDigest(t, f, "quay.io")); err != nil || !ok {
		t.Fatalf("预置能力失败: ok=%v err=%v", ok, err)
	}
	if f.capOf(t, "quay.io") != store.RegistryCapYes {
		t.Fatal("预置能力没写进去")
	}

	if err := f.cs.Delete("quay.io"); err != nil {
		t.Fatal(err)
	}
	g2, d2 := f.genOf(t, "quay.io")
	if g2 != gen+1 || d2 != emptyDigest("quay.io") {
		t.Fatalf("删除后版本不对: gen=%d digest=%s", g2, d2)
	}
	if f.capOf(t, "quay.io") != store.RegistryCapUnknown {
		t.Fatal("删除凭据后能力状态应被清空")
	}
	if _, ok, err := f.cs.Get("quay.io"); err != nil || ok {
		t.Fatalf("删除后不应读出凭据: ok=%v err=%v", ok, err)
	}
	if n, err := f.cs.Reconcile(); err != nil || n != 0 {
		t.Fatalf("删除后对账应无变更: n=%d err=%v", n, err)
	}
	if err := f.cs.Delete("quay.io"); err != nil {
		t.Fatalf("重复删除应幂等，得到 %v", err)
	}
}

// 删除已写文件、DB 未提交：Probe 不能把它当成「本来就没有凭据」，
// 对账必须依据文件里的删除标记补提交，而不是报记录丢失。
func TestCredsDeleteCrashAdopted(t *testing.T) {
	f := newCredFixture(t, "quay.io")
	if _, err := f.cs.Reconcile(); err != nil {
		t.Fatal(err)
	}
	f.mustPut(t, "quay.io", "u", "p")
	gen, _ := f.genOf(t, "quay.io")

	realDB := f.db
	f.cs.db = &failingDB{DB: realDB}
	if err := f.cs.Delete("quay.io"); err == nil {
		t.Fatal("DB 失败时 Delete 应报错")
	}
	f.cs.db = realDB
	if _, _, err := f.cs.Get("quay.io"); !errors.Is(err, ErrCredentialPending) {
		t.Fatalf("未提交的删除不应表现为无凭据，得到 %v", err)
	}
	n, err := f.cs.Reconcile()
	if err != nil || n != 1 {
		t.Fatalf("对账应补提交删除: n=%d err=%v", n, err)
	}
	g2, d2 := f.genOf(t, "quay.io")
	if g2 != gen+1 || d2 != emptyDigest("quay.io") {
		t.Fatalf("删除提交结果不对: gen=%d digest=%s", g2, d2)
	}
	if _, ok, err := f.cs.Get("quay.io"); err != nil || ok {
		t.Fatalf("删除后应无凭据: ok=%v err=%v", ok, err)
	}
	// 删除标记留在文件里，重复对账仍然稳定。
	if n, err := f.cs.Reconcile(); err != nil || n != 0 {
		t.Fatalf("重复对账不应有变更: n=%d err=%v", n, err)
	}
	// 重新配置后撤销标记必须消失，否则下次对账会误删。
	if _, err := f.cs.Put("quay.io", "u", "p2"); err != nil {
		t.Fatal(err)
	}
	doc := readDoc(t, f.path)
	if doc.Revoked["quay.io"] {
		t.Fatal("重新配置后不应保留删除标记")
	}
	if n, err := f.cs.Reconcile(); err != nil || n != 0 {
		t.Fatalf("重配后对账不应有变更: n=%d err=%v", n, err)
	}
	if _, ok, err := f.cs.Get("quay.io"); err != nil || !ok {
		t.Fatalf("重配后应可读: ok=%v err=%v", ok, err)
	}
}

// 文件已换新、DB 未提交（崩在两步之间）：对账补提交。
func TestCredsReconcileAdoptsFile(t *testing.T) {
	f := newCredFixture(t, "quay.io")
	if _, err := f.cs.Reconcile(); err != nil {
		t.Fatal(err)
	}
	digest := f.mustPut(t, "quay.io", "u", "p")
	gen, _ := f.genOf(t, "quay.io")
	// 把 DB 摘要改成一个「旧版本」，模拟提交失败后残留的状态。
	stale := "stale-digest"
	if err := f.db.ApplyCredChanges([]store.CredChange{{Host: "quay.io", Digest: &stale}}); err != nil {
		t.Fatal(err)
	}
	// 此时 Probe 读不到凭据：文件与数据库摘要不一致。
	if _, _, err := f.cs.Get("quay.io"); !errors.Is(err, ErrCredentialPending) {
		t.Fatalf("摘要不一致应拒绝读取，得到 %v", err)
	}
	n, err := f.cs.Reconcile()
	if err != nil || n != 1 {
		t.Fatalf("对账应补 1 条: n=%d err=%v", n, err)
	}
	g3, d3 := f.genOf(t, "quay.io")
	if g3 != gen+2 || d3 != digest {
		t.Fatalf("对账后版本不对: gen=%d digest=%s want gen=%d digest=%s", g3, d3, gen+2, digest)
	}
	if _, ok, err := f.cs.Get("quay.io"); err != nil || !ok {
		t.Fatalf("对账后应能读出凭据: ok=%v err=%v", ok, err)
	}
}

// DB 提交失败：文件新、DB 旧，Probe 必须拒绝；换回可用 DB 后对账自愈。
func TestCredsDBFailureBlocks(t *testing.T) {
	f := newCredFixture(t, "quay.io")
	if _, err := f.cs.Reconcile(); err != nil {
		t.Fatal(err)
	}
	realDB := f.db
	f.cs.db = &failingDB{DB: realDB}
	if _, err := f.cs.Put("quay.io", "u", "p"); err == nil {
		t.Fatal("DB 失败时 Put 应报错")
	}
	f.cs.db = realDB
	if _, _, err := f.cs.Get("quay.io"); !errors.Is(err, ErrCredentialPending) {
		t.Fatalf("未提交的凭据不应被 Probe 读到，得到 %v", err)
	}
	if n, err := f.cs.Reconcile(); err != nil || n != 1 {
		t.Fatalf("对账应补 1 条: n=%d err=%v", n, err)
	}
	if _, ok, err := f.cs.Get("quay.io"); err != nil || !ok {
		t.Fatalf("对账后应可读: ok=%v err=%v", ok, err)
	}
}

func TestCredsConcurrent(t *testing.T) {
	hosts := []string{"quay.io", "lscr.io", "gcr.io", "registry.k8s.io"}
	f := newCredFixture(t, hosts...)
	if _, err := f.cs.Reconcile(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, h := range hosts {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			if _, err := f.cs.Put(h, "u-"+h, "p-"+h); err != nil {
				t.Errorf("%s: %v", h, err)
			}
		}(h)
	}
	wg.Wait()
	for _, h := range hosts {
		if c, ok, err := f.cs.Get(h); err != nil || !ok || c.Username != "u-"+h {
			t.Fatalf("%s 丢失或被覆盖: %+v ok=%v err=%v", h, c, ok, err)
		}
	}

	// 同一个 Host 并发改：最终文件与 DB 必须一致。
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := f.cs.Put("quay.io", "robot", "p-"+string(rune('a'+i))); err != nil {
				t.Errorf("并发同 Host 写入: %v", err)
			}
		}(i)
	}
	wg.Wait()
	gen, dbDigest := f.genOf(t, "quay.io")
	if gen != 9 {
		t.Fatalf("同 Host 写 8 次后 gen 应为 9，实际 %d", gen)
	}
	doc := readDoc(t, f.path)
	if got := recordDigest("quay.io", doc.Hosts["quay.io"]); got != dbDigest {
		t.Fatalf("文件与 DB 摘要不一致: %s vs %s", got, dbDigest)
	}
	if _, ok, err := f.cs.Get("quay.io"); err != nil || !ok {
		t.Fatalf("并发后应可读: ok=%v err=%v", ok, err)
	}
}

// SetHostCaps 必须同时校验 gen 与 cred_digest。
func TestSetHostCapsNeedsCurrentDigest(t *testing.T) {
	f := newCredFixture(t, "quay.io")
	if _, err := f.cs.Reconcile(); err != nil {
		t.Fatal(err)
	}
	digest := f.mustPut(t, "quay.io", "u", "p")
	gen, _ := f.genOf(t, "quay.io")
	id := f.source["quay.io"]

	if ok, err := f.db.SetHostCaps(id, "quay.io", store.RegistryCapYes, store.PathNative, 1, "", gen, "old-digest"); err != nil || ok {
		t.Fatalf("旧摘要不应写入: ok=%v err=%v", ok, err)
	}
	if ok, err := f.db.SetHostCaps(id, "quay.io", store.RegistryCapYes, store.PathNative, 1, "", gen+1, digest); err != nil || ok {
		t.Fatalf("旧代次不应写入: ok=%v err=%v", ok, err)
	}
	if ok, err := f.db.SetHostCaps(id, "quay.io", store.RegistryCapYes, store.PathNative, 1, "", gen, digest); err != nil || !ok {
		t.Fatalf("当前版本应写入: ok=%v err=%v", ok, err)
	}
	// 换凭据后旧摘要立即失效。
	newDigest := f.mustPut(t, "quay.io", "u", "p2")
	if ok, err := f.db.SetHostCaps(id, "quay.io", store.RegistryCapYes, store.PathNative, 2, "", gen+1, digest); err != nil || ok {
		t.Fatalf("换凭据后旧摘要不应写入: ok=%v err=%v", ok, err)
	}
	if ok, err := f.db.SetHostCaps(id, "quay.io", store.RegistryCapYes, store.PathNative, 2, "", gen+1, newDigest); err != nil || !ok {
		t.Fatalf("新摘要应写入: ok=%v err=%v", ok, err)
	}
}

// 摘要编码是契约的一部分：长度前缀、字段顺序、标签都不能悄悄变，
// 否则已提交的 digest 会全部错位，触发一次无意义的 gen 递增。
func TestDigestEncodingIsLocked(t *testing.T) {
	got := recordDigest("quay.io", cipherRecord{Nonce: "AAAA", CT: "BBBB"})
	want := "1989397636c2a5d15956cd839db8ea3b5eee11363b6e2b99b3729b10369b6e31"
	if got != want {
		t.Fatalf("摘要编码变了: %s != %s", got, want)
	}
	if e := emptyDigest("quay.io"); e != "58e395ca06ae9d5522c703183e552e1afba9a626a4bb15c9a6b700cc23691adf" {
		t.Fatalf("空摘要编码变了: %s", e)
	}
	// 长度前缀必须挡住「字段边界挪一格」的碰撞：
	// 朴素拼接 host|a|b|c 与 host|a|b|c 无法区分下面两条记录。
	a := recordDigest("quay.io", cipherRecord{Nonce: "a|b", CT: "c"})
	b := recordDigest("quay.io", cipherRecord{Nonce: "a", CT: "b|c"})
	if a == b {
		t.Fatal("不同字段切分拼出了同一段摘要输入")
	}
}

func TestCredsRecordAndRevokedConflict(t *testing.T) {
	f := newCredFixture(t, "quay.io")
	if _, err := f.cs.Reconcile(); err != nil {
		t.Fatal(err)
	}
	f.mustPut(t, "quay.io", "u", "p")
	tamper(t, f.path, func(doc map[string]any) {
		doc["revoked"] = map[string]any{"quay.io": true}
	})
	if _, err := f.cs.Reconcile(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("同时存在密文与删除标记应判损坏，得到 %v", err)
	}
}

type failingDB struct{ DB }

func (f *failingDB) ApplyCredChanges([]store.CredChange) error {
	return errors.New("数据库不可用")
}

func mustDigest(t *testing.T, f *credFixture, host string) string {
	t.Helper()
	_, d := f.genOf(t, host)
	if d == "" {
		t.Fatalf("%s 没有摘要", host)
	}
	return d
}

func strPtr(s string) *string { return &s }

func readDoc(t *testing.T, path string) *credsDoc {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc credsDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return &doc
}

func tamper(t *testing.T, path string, fn func(map[string]any)) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	fn(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}
