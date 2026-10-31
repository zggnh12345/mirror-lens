package auth

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := EnsureAdmin(st); err != nil {
		t.Fatal(err)
	}
	return st
}

// 并发设置：只有一个能赢，其余拿 ErrSetupDone；最终账号必须是赢家那一组，
// 不能出现「用户名是 A、密码是 B」。
func TestSetupConcurrentOnlyOneWins(t *testing.T) {
	st := newStore(t)

	const n = 8
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user := "user" + string(rune('a'+i))
			results[i] = Setup(st, user, "pass-"+user)
		}(i)
	}
	wg.Wait()

	won := -1
	for i, err := range results {
		switch {
		case err == nil:
			if won >= 0 {
				t.Fatalf("两个调用都成功了：%d 和 %d", won, i)
			}
			won = i
		case errors.Is(err, ErrSetupDone):
		default:
			t.Fatalf("第 %d 个调用出现意外错误：%v", i, err)
		}
	}
	if won < 0 {
		t.Fatalf("没有任何一个调用成功")
	}

	user := "user" + string(rune('a'+won))
	if got := st.GetSetting(KeyUser, ""); got != user {
		t.Fatalf("用户名应是赢家的 %q，实际 %q", user, got)
	}
	if !Check(st, user, "pass-"+user) {
		t.Fatalf("赢家自己的密码应该能登录")
	}
	if st.GetSetting(KeyMustChg, "0") != "0" {
		t.Fatalf("must_change 应已清掉")
	}
	// 输家那一组不能登录。
	for i := 0; i < n; i++ {
		if i == won {
			continue
		}
		u := "user" + string(rune('a'+i))
		if Check(st, u, "pass-"+u) {
			t.Fatalf("输家 %q 也能登录，说明写串了", u)
		}
	}
}

// 已经设置过的实例不能再走这个口子。
func TestSetupIsOneShot(t *testing.T) {
	st := newStore(t)
	if err := Setup(st, "alex", "correct-horse"); err != nil {
		t.Fatal(err)
	}
	if err := Setup(st, "other", "another-pass"); !errors.Is(err, ErrSetupDone) {
		t.Fatalf("第二次应返回 ErrSetupDone，得到 %v", err)
	}
	if got := st.GetSetting(KeyUser, ""); got != "alex" {
		t.Fatalf("第二次调用不该改用户名，实际 %q", got)
	}
	if !Check(st, "alex", "correct-horse") {
		t.Fatalf("原密码不该被覆盖")
	}
}
