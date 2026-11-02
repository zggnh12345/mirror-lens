package discover

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 发现清单来自第三方，内网/保留地址必须在入库前丢掉。
func TestPublicHost(t *testing.T) {
	bad := []string{
		"127.0.0.1", "127.0.0.1:8080", "10.1.1.5", "192.168.1.10:5000",
		"172.17.0.2:8080", "169.254.169.254", "[::1]:8080", "0.0.0.0:80",
		"localhost", "localhost:8080", "foo.local", "mirror.internal",
		"intranet", "user:pw@mirror.example.com",
	}
	for _, h := range bad {
		if publicHost(h) {
			t.Errorf("%q 应被拒", h)
		}
	}
	good := []string{"mirror.example.com", "ghcr.example.cn:443", "dockerproxy.net"}
	for _, h := range good {
		if !publicHost(h) {
			t.Errorf("%q 应通过", h)
		}
	}
}

// 抓清单也走同一层防护：回环地址在拨号层就被拦，一条候选都拿不到。
func TestFetchRefusesInternalFeed(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		_, _ = w.Write([]byte("https://mirror.example.com\n"))
	}))
	defer srv.Close()

	old := feeds
	feeds = []feed{{name: "test", url: srv.URL, format: fmtMarkdown, kind: KindDocker}}
	defer func() { feeds = old }()

	if got := fetchAll(context.Background()); len(got) != 0 {
		t.Fatalf("内网清单不该产出候选: %+v", got)
	}
	if hit {
		t.Fatal("请求不该真的打到回环地址：说明没走 Guard")
	}
}

// 重定向每一跳都重新校验：公网 → 元数据地址必须被拒。
func TestClientBlocksRedirectToPrivate(t *testing.T) {
	c := feedGuard.Client()
	if c.CheckRedirect == nil {
		t.Fatal("客户端没有重定向校验")
	}
	next := httptest.NewRequest(http.MethodGet, "http://169.254.169.254/latest/meta-data", nil)
	via := []*http.Request{httptest.NewRequest(http.MethodGet, "https://public.example.com/list", nil)}
	if err := c.CheckRedirect(next, via); err == nil {
		t.Fatal("重定向到元数据地址应被拒")
	}
}
