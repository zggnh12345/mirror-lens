package store

import (
	"fmt"
	"sync"
	"testing"
)

// D-STORE：AddSourceHost 必须在事务内读取 base_url；与 PATCH endpoint 并发时
// 不得让旧 Endpoint 的 Realm policy 写入新状态，也不得出现事务竞态错误。
func TestAddSourceHostConcurrentWithSetBaseURL(t *testing.T) {
	st := openTest(t)
	id, err := st.AddSource(Source{BaseURL: "https://quay.example/", Name: "race", RepoHost: "quay.io", Status: "usable", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	const rounds = 20
	var wg sync.WaitGroup
	errCh := make(chan error, rounds*2)
	for i := 0; i < rounds; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			base := fmt.Sprintf("https://host%d.example/", i)
			if err := st.SetBaseURL(id, base); err != nil {
				errCh <- fmt.Errorf("SetBaseURL: %w", err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			primary := fmt.Sprintf("https://host%d.example", i)
			if err := st.AddSourceHost(id, "quay.io", primary, nil); err != nil {
				errCh <- fmt.Errorf("AddSourceHost: %w", err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	got, err := st.GetSource(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseURL == "" {
		t.Fatal("并发后 base_url 丢失")
	}
	hosts, err := st.GetSourceHosts(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) == 0 {
		t.Fatal("并发后 Host 关系丢失")
	}
}
