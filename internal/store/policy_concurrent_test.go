package store

import (
	"fmt"
	"sync"
	"testing"
)

// v1.6 §3.4：两个 goroutine 同时修改同一 (source_id, host)。
// 只允许串行成功提交；generation 必须单调、不能丢失后一次修改、
// 不能出现重复主键或两条主 Realm。
func TestSetRealmPolicyConcurrentGenerations(t *testing.T) {
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
	var baseEvents int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM source_events WHERE source_id=? AND kind='policy_change'`, id).Scan(&baseEvents); err != nil {
		t.Fatal(err)
	}

	const n = 2
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = st.SetRealmPolicy(id, "quay.io", fmt.Sprintf("https://realm%d.example", i), nil)
		}(i)
	}
	wg.Wait()

	ok := 0
	for _, e := range errs {
		if e == nil {
			ok++
		}
	}
	if ok == 0 {
		t.Fatalf("并发修改全部失败: %v", errs)
	}

	after, err := st.GetRealmPolicy(id, "quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if after.PolicyGen <= before.PolicyGen {
		t.Fatalf("policy_gen 未单调递增: before=%d after=%d", before.PolicyGen, after.PolicyGen)
	}
	if after.PolicyGen != before.PolicyGen+int64(ok) {
		t.Fatalf("generation 与成功提交次数不符（丢失修改）: before=%d after=%d ok=%d", before.PolicyGen, after.PolicyGen, ok)
	}

	var rows int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM source_realm_policy WHERE source_id=? AND host=?`, id, "quay.io").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("出现重复主键/两条主 Realm: rows=%d", rows)
	}
	if after.PrimaryRealm == "" || after.PrimaryRealm == before.PrimaryRealm {
		t.Fatalf("最终主 Realm 被并发覆盖丢失: %q", after.PrimaryRealm)
	}

	var events int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM source_events WHERE source_id=? AND kind='policy_change'`, id).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != baseEvents+ok {
		t.Fatalf("策略事件数应与成功提交数一致: base=%d events=%d ok=%d", baseEvents, events, ok)
	}
}
