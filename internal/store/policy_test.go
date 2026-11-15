package store

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/v2auth"
)

func TestAddSourceSeedsPolicyAndEvent(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{
		BaseURL: "https://m.daocloud.io/", Name: "daocloud", RepoHost: "public.ecr.aws",
		DockerCap: 1, Status: "usable", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.GetRealmPolicy(id, "public.ecr.aws")
	if err != nil {
		t.Fatal(err)
	}
	if p.PrimaryRealm != "https://m.daocloud.io" || p.PolicyGen != 1 {
		t.Fatalf("第三方镜像策略应使用 BaseURL origin: %+v", p)
	}
	var n int
	if err := st.db.QueryRow(`SELECT count(*) FROM source_events WHERE source_id=? AND kind=?`, id, SourceEventPolicyChange).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("AddSource 应写一条策略事件，实际 %d", n)
	}
	// 策略事件不能进入健康三态统计。
	ok, neutral, fail, _, err := st.SourceEvents(id, 20)
	if err != nil {
		t.Fatal(err)
	}
	if ok != 0 || neutral != 0 || fail != 0 {
		t.Fatalf("策略事件污染健康统计: %d/%d/%d", ok, neutral, fail)
	}
}

func TestStorePolicyOfficialAndMissing(t *testing.T) {
	st := openTest(t)
	pol := NewStorePolicy(st)
	p, err := pol.Resolve(context.Background(), 0, "registry.example")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Official || !p.AllowSameHost || p.PolicyGen != 0 {
		t.Fatalf("官方策略不正确: %+v", p)
	}
	if err := p.Allows(mustURL(t, "https://registry.example/token")); err != nil {
		t.Fatalf("官方同 Host 应放行: %v", err)
	}
	if _, err := pol.Resolve(context.Background(), 7, "quay.io"); !errors.Is(err, v2auth.ErrPolicyMissing) {
		t.Fatalf("无策略 Source 必须 fail closed，实际 %v", err)
	}
}

func TestStorePolicyExtraLimitAndGeneration(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{
		BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io",
		DockerCap: 1, Status: "usable", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetRealmPolicy(id, "quay.io", "https://quay.io", []string{"https://auth.example"}); err != nil {
		t.Fatal(err)
	}
	p, err := st.GetRealmPolicy(id, "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if p.PolicyGen != 2 || len(p.ExtraRealms) != 1 {
		t.Fatalf("策略更新未递增 generation: %+v", p)
	}
	if err := st.SetRealmPolicy(id, "quay.io", "https://quay.io", []string{"https://a", "https://b", "https://c", "https://d"}); err == nil {
		t.Fatal("超过 3 个额外 Realm 必须拒绝")
	}
	if err := st.SetRealmPolicy(id, "quay.io", "https://quay.io", []string{"http://insecure.example"}); err == nil {
		t.Fatal("http Realm 必须拒绝")
	}
}

func TestSetRepoHostReplacesPolicyAtomically(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{
		BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io",
		DockerCap: 1, Status: "usable", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetRealmPolicy(id, "quay.io", "https://quay.io", []string{"https://old.example"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRepoHost(id, "gcr.io"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetRealmPolicy(id, "quay.io"); !errors.Is(err, v2auth.ErrPolicyMissing) {
		t.Fatalf("旧 Host 策略应删除，实际 %v", err)
	}
	p, err := st.GetRealmPolicy(id, "gcr.io")
	if err != nil {
		t.Fatal(err)
	}
	if p.PrimaryRealm == "" || p.PolicyGen <= 0 {
		t.Fatalf("新 Host 策略不完整: %+v", p)
	}
	var oldCount int
	if err := st.db.QueryRow(`SELECT count(*) FROM source_realm_policy WHERE source_id=? AND host='quay.io'`, id).Scan(&oldCount); err != nil {
		t.Fatal(err)
	}
	if oldCount != 0 {
		t.Fatal("SetRepoHost 后仍残留旧 Host 策略")
	}
}

func TestAddSourceRollsBackWhenPolicyInsertFails(t *testing.T) {
	st := openTest(t)
	if _, err := st.db.Exec(`CREATE TRIGGER fail_policy BEFORE INSERT ON source_realm_policy
		BEGIN SELECT RAISE(ABORT, 'inject policy insert'); END`); err != nil {
		t.Fatal(err)
	}
	_, err := st.AddSource(Source{
		BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io",
		DockerCap: 1, Status: "usable", Enabled: true,
	})
	if err == nil {
		t.Fatal("注入策略插入失败时 AddSource 应失败")
	}
	var n int
	if err := st.db.QueryRow(`SELECT count(*) FROM sources`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("AddSource 失败留下 %d 条源", n)
	}
}

func TestSetRealmPolicyRollsBackWhenEventFails(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{
		BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io",
		DockerCap: 1, Status: "usable", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := st.GetRealmPolicy(id, "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`CREATE TRIGGER fail_policy_event BEFORE INSERT ON source_events
		WHEN NEW.kind='policy_change'
		BEGIN SELECT RAISE(ABORT, 'inject policy event'); END`); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRealmPolicy(id, "quay.io", "https://new.example", nil); err == nil {
		t.Fatal("注入事件失败时 SetRealmPolicy 应失败")
	}
	after, err := st.GetRealmPolicy(id, "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if after.PrimaryRealm != before.PrimaryRealm || after.PolicyGen != before.PolicyGen {
		t.Fatalf("策略未被回滚: before=%+v after=%+v", before, after)
	}
}

func TestRealmPolicyRejectsUnsafeRealm(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{
		BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io",
		DockerCap: 1, Status: "usable", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	bad := []string{
		"https://*.example",
		"http://quay.io",
		"https://user:pass@quay.io",
		"https://quay.io/path",
		"https://127.0.0.1",
	}
	for _, realm := range bad {
		if err := st.SetRealmPolicy(id, "quay.io", realm, nil); err == nil {
			t.Fatalf("不安全 Realm 必须拒绝: %q", realm)
		}
	}
}

func TestStorePolicyUsesHostGeneration(t *testing.T) {
	st := openTest(t)
	if _, err := st.db.Exec(`INSERT INTO registry_host_gen(host,gen,cred_digest,updated_at)
		VALUES('quay.io',7,'digest',1700000000)`); err != nil {
		t.Fatal(err)
	}
	id, err := st.AddSource(Source{
		BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io",
		DockerCap: 1, Status: "usable", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewStorePolicy(st).Resolve(context.Background(), id, "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if p.CredGen != 7 || p.PolicyGen != 1 {
		t.Fatalf("StorePolicy 身份代次不正确: %+v", p)
	}
}

func TestPolicyEventIsExcludedFromHealth(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{
		BaseURL: "https://quay.example/", Name: "q", RepoHost: "quay.io",
		DockerCap: 1, Status: "usable", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 人为再加一条真实探测事件，确认统计只算它。
	if _, err := st.db.Exec(`INSERT INTO source_events(source_id,kind,ts,ok,neutral,ms,code)
		VALUES(?,?,?,?,?,?,?)`, id, "registry:quay.io", time.Now().Unix(), 1, 0, 10, 200); err != nil {
		t.Fatal(err)
	}
	ok, neutral, fail, _, err := st.SourceEvents(id, 20)
	if err != nil {
		t.Fatal(err)
	}
	if ok != 1 || neutral != 0 || fail != 0 {
		t.Fatalf("健康统计应只算真实探测事件: %d/%d/%d", ok, neutral, fail)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
