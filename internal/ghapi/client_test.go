package ghapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func clientWith(t *testing.T, body string, code int, calls *int32) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			atomic.AddInt32(calls, 1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := New("tok")
	c.Base = srv.URL
	c.HTTP = &http.Client{Timeout: 500 * time.Millisecond}
	return c
}

func TestAssetDigestOK(t *testing.T) {
	body := `{"assets":[{"name":"a.tgz","digest":"sha256:ABCdef0123"},{"name":"b.tgz","digest":null}]}`
	c := clientWith(t, body, 200, nil)
	d := c.AssetDigest(context.Background(), "o", "r", "v1", "a.tgz")
	if d.Result != ResultOK || d.Hex != "abcdef0123" || d.Algorithm != "sha256" {
		t.Fatalf("结果异常: %+v", d)
	}
}

func TestAssetDigestNullIsNotError(t *testing.T) {
	body := `{"assets":[{"name":"b.tgz","digest":null}]}`
	c := clientWith(t, body, 200, nil)
	d := c.AssetDigest(context.Background(), "o", "r", "v1", "b.tgz")
	if d.Result != ResultNoDigest || d.Err != nil {
		t.Fatalf("null digest 应正常降级: %+v", d)
	}
}

// 字段缺失和 null 必须分开：前者说明 API 结构变了，后者是资产本身没有摘要。
func TestAssetDigestMissingFieldIsSchema(t *testing.T) {
	body := `{"assets":[{"name":"b.tgz"}]}`
	c := clientWith(t, body, 200, nil)
	d := c.AssetDigest(context.Background(), "o", "r", "v1", "b.tgz")
	if d.Result != ResultSchema {
		t.Fatalf("缺字段应记 api_schema_unexpected: %+v", d)
	}
	if d.Result == ResultNoDigest {
		t.Fatal("不能把缺字段混同 null")
	}
}

func TestAssetDigestUnsupportedAlgorithm(t *testing.T) {
	body := `{"assets":[{"name":"a.tgz","digest":"sha512:deadbeef"}]}`
	c := clientWith(t, body, 200, nil)
	d := c.AssetDigest(context.Background(), "o", "r", "v1", "a.tgz")
	if d.Result != ResultUnsupportedAlg || d.Hex != "" {
		t.Fatalf("非 sha256 必须降级: %+v", d)
	}
}

func TestAssetDigestErrorStatuses(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{404, ResultNotFound},
		{403, ResultRateLimited},
		{429, ResultRateLimited},
		{500, ResultUnavailable},
		{502, ResultUnavailable},
	}
	for _, tc := range cases {
		c := clientWith(t, `{}`, tc.code, nil)
		d := c.AssetDigest(context.Background(), "o", "r", "v1", "a.tgz")
		if d.Result != tc.want {
			t.Fatalf("%d 应归 %s，实际 %s", tc.code, tc.want, d.Result)
		}
	}
}

func TestAssetDigestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{"assets":[]}`))
	}))
	t.Cleanup(srv.Close)
	c := New("tok")
	c.Base = srv.URL
	c.HTTP = &http.Client{Timeout: 50 * time.Millisecond}
	d := c.AssetDigest(context.Background(), "o", "r", "v1", "a.tgz")
	if d.Result != ResultTimeout {
		t.Fatalf("超时应归 timeout: %+v", d)
	}
}

func TestNoTokenNeverCallsAPI(t *testing.T) {
	var calls int32
	c := clientWith(t, `{}`, 200, &calls)
	c.Token = ""
	d := c.AssetDigest(context.Background(), "o", "r", "v1", "a.tgz")
	if d.Result != ResultNoToken {
		t.Fatalf("未配置 Token 应直接降级: %+v", d)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatal("未配置 Token 时不应发起任何 API 请求")
	}
	if c.Enabled() {
		t.Fatal("无 Token 时 Enabled 应为 false")
	}
}

func TestResultCached(t *testing.T) {
	var calls int32
	c := clientWith(t, `{"assets":[{"name":"a.tgz","digest":"sha256:aa"}]}`, 200, &calls)
	for i := 0; i < 3; i++ {
		c.AssetDigest(context.Background(), "o", "r", "v1", "a.tgz")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("同一资产应只查一次，实际 %d 次", n)
	}
}

func TestLatestUsesLatestEndpoint(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{"assets":[{"name":"a.tgz","digest":"sha256:aa"}]}`))
	}))
	t.Cleanup(srv.Close)
	c := New("tok")
	c.Base = srv.URL
	c.AssetDigest(context.Background(), "o", "r", "", "a.tgz")
	if path != "/repos/o/r/releases/latest" {
		t.Fatalf("latest 应走 latest 端点，实际 %s", path)
	}
}

// 平台摘要为 null 时，退到同一 Release 里的发布者校验文件。
func TestChecksumFallback(t *testing.T) {
	const sum = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/releases/tags/v1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":7,"assets":[
			{"id":11,"name":"a.tgz","digest":null,"updated_at":"2026-01-01T00:00:00Z","browser_download_url":"http://` + r.Host + `/dl/a.tgz"},
			{"id":12,"name":"SHA256SUMS","digest":null,"size":80,"updated_at":"2026-01-02T00:00:00Z","browser_download_url":"http://` + r.Host + `/dl/SHA256SUMS"}
		]}`))
	})
	mux.HandleFunc("/dl/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sum + "  a.tgz\n"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := New("tok")
	c.Base = srv.URL

	d := c.AssetDigest(context.Background(), "o", "r", "v1", "a.tgz")
	if d.Result != ResultOK || d.Hex != sum || d.Source != SourceSUMS {
		t.Fatalf("校验文件回退失败: %+v", d)
	}
	if d.AssetID != 11 || d.ChecksumAssetID != 12 || d.ReleaseID != 7 {
		t.Fatalf("身份绑定字段缺失: %+v", d)
	}
	if d.AssetUpdatedAt == "" || d.ChecksumUpdated == "" {
		t.Fatalf("资产版本字段缺失: %+v", d)
	}
}

// 平台摘要优先于校验文件：两者都在时不能去下载校验文件。
func TestAPIDigestWinsOverChecksum(t *testing.T) {
	const sum = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	var checksumHits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/releases/tags/v1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":7,"assets":[
			{"id":11,"name":"a.tgz","digest":"sha256:` + sum + `"},
			{"id":12,"name":"SHA256SUMS","size":80,"browser_download_url":"http://` + r.Host + `/dl/SHA256SUMS"}
		]}`))
	})
	mux.HandleFunc("/dl/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&checksumHits, 1)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := New("tok")
	c.Base = srv.URL

	d := c.AssetDigest(context.Background(), "o", "r", "v1", "a.tgz")
	if d.Result != ResultOK || d.Source != SourceAPI {
		t.Fatalf("应优先用平台摘要: %+v", d)
	}
	if atomic.LoadInt32(&checksumHits) != 0 {
		t.Fatal("有平台摘要时不应下载校验文件")
	}
}

// 校验文件里没有目标文件 → 无摘要，不是错误，也不阻塞下载。
func TestChecksumMissingEntryIsNoDigest(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/releases/tags/v1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":7,"assets":[
			{"id":11,"name":"a.tgz","digest":null},
			{"id":12,"name":"SHA256SUMS","size":80,"browser_download_url":"http://` + r.Host + `/dl/SHA256SUMS"}
		]}`))
	})
	mux.HandleFunc("/dl/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef  other.tgz\n"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := New("tok")
	c.Base = srv.URL

	d := c.AssetDigest(context.Background(), "o", "r", "v1", "a.tgz")
	if d.Result != ResultNoDigest || d.Source != SourceNone {
		t.Fatalf("缺记录应降级为无摘要: %+v", d)
	}
}
