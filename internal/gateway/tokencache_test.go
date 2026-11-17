package gateway

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/v2auth"
)

// 过期的 token 不能一直堆在 map 里（key 带 scope，每个仓库一条）。
func TestTokenCachePrunesExpired(t *testing.T) {
	c := newTokenCache()
	c.mu.Lock()
	for i := 0; i < c.max+40; i++ {
		c.m[tokenKey{Host: string(rune('a'+i%26)) + string(rune('a'+i/26))}] = tokenEntry{
			tok: "old",
			exp: time.Now().Add(-time.Minute),
		}
	}
	c.pruneLocked()
	c.mu.Unlock()

	if n := len(c.m); n != 0 {
		t.Fatalf("过期的 token 应被清空，还剩 %d 条", n)
	}
}

// 未过期的条目要保住。
func TestTokenCacheKeepsFresh(t *testing.T) {
	c := newTokenCache()
	c.mu.Lock()
	for i := 0; i < c.max; i++ {
		c.m[tokenKey{Host: string(rune('a'+i%26)) + string(rune('a'+i/26))}] = tokenEntry{
			tok: "fresh",
			exp: time.Now().Add(time.Minute),
		}
	}
	c.m[tokenKey{Host: "keep"}] = tokenEntry{tok: "keep", exp: time.Now().Add(time.Hour)}
	c.pruneLocked()
	c.mu.Unlock()

	if _, ok := c.m[tokenKey{Host: "keep"}]; !ok {
		t.Fatal("未过期的 token 不该被清掉")
	}
}

// 同一个 key 的并发刷新只能打一次 Token 端点。
func TestTokenCacheSingleFlight(t *testing.T) {
	c := newTokenCache()
	var hits atomic.Int64
	release := make(chan struct{})
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		hits.Add(1)
		<-release
		return authResponse(http.StatusOK, `{"token":"shared"}`, nil), nil
	})
	key := tokenKey{SourceID: 1, Host: "registry.example", Realm: "https://auth.example/token", Service: "registry", Scope: "repository:g/p:pull"}

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			tok, err := c.get(context.Background(), rt, key, v2auth.Challenge{Realm: key.Realm, Service: key.Service}, key.Scope, nil)
			if err == nil && tok != "shared" {
				err = fmt.Errorf("token=%q", tok)
			}
			errs <- err
		}()
	}
	close(start)
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("并发刷新应合并为 1 次，实际 %d 次", got)
	}
}

// leader 的请求被取消不能拖垮同 key 的等待者。
func TestTokenCacheLeaderCancelDoesNotFailWaiters(t *testing.T) {
	c := newTokenCache()
	var hits atomic.Int64
	release := make(chan struct{})
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		hits.Add(1)
		<-release
		return authResponse(http.StatusOK, `{"token":"shared"}`, nil), nil
	})
	key := tokenKey{SourceID: 1, Host: "registry.example", Realm: "https://auth.example/token", Service: "registry", Scope: "repository:g/p:pull"}
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	go func() {
		_, _ = c.get(leaderCtx, rt, key, v2auth.Challenge{Realm: key.Realm, Service: key.Service}, key.Scope, nil)
	}()
	deadline := time.Now().Add(time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if hits.Load() == 0 {
		t.Fatal("leader 没有发起 token 请求")
	}
	cancelLeader()

	type result struct {
		tok string
		err error
	}
	waiter := make(chan result, 1)
	go func() {
		tok, err := c.get(context.Background(), rt, key, v2auth.Challenge{Realm: key.Realm, Service: key.Service}, key.Scope, nil)
		waiter <- result{tok, err}
	}()
	time.Sleep(20 * time.Millisecond)
	close(release)
	select {
	case r := <-waiter:
		if r.err != nil {
			t.Fatalf("等待者被 leader 取消拖垮: %v", r.err)
		}
		if r.tok != "shared" {
			t.Fatalf("token=%q", r.tok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("等待者超时")
	}
}
