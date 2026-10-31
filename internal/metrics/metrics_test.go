package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func setup(t *testing.T) *Registry {
	t.Helper()
	r := Enable()
	t.Cleanup(Disable)
	return r
}

func TestMiddlewareCountsRequestAndBytes(t *testing.T) {
	setup(t)
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-ML-Cache", "HIT")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("0123456789"))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v2/library/ubuntu/blobs/sha256:abc", nil))

	out := string(Get().render())
	if !strings.Contains(out, `mirrorlens_http_requests_total{scene="docker",method="GET",code="200"} 1`) {
		t.Fatalf("请求数没记上:\n%s", out)
	}
	if !strings.Contains(out, `mirrorlens_http_response_bytes_total{scene="docker"} 10`) {
		t.Fatalf("字节数没记上:\n%s", out)
	}
	if !strings.Contains(out, `mirrorlens_cache_requests_total{scene="docker",result="hit"} 1`) {
		t.Fatalf("缓存结果没记上:\n%s", out)
	}
	// URL 本身绝不能出现在标签里
	if strings.Contains(out, "sha256") || strings.Contains(out, "ubuntu") {
		t.Fatalf("标签里混进了 URL 片段:\n%s", out)
	}
}

func TestMiddlewareRecords429FromLimiter(t *testing.T) {
	setup(t)
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/raw/a/b", nil))
	out := string(Get().render())
	if !strings.Contains(out, `code="429"`) {
		t.Fatalf("429 应被统计:\n%s", out)
	}
}

// /metrics 自己不进统计，否则每次抓取都在制造一条「请求」。
func TestMiddlewareSkipsOwnEndpoint(t *testing.T) {
	setup(t)
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/metrics", nil))
	if strings.Contains(string(Get().render()), `scene="metrics"`) {
		t.Fatal("/metrics 不该统计自己")
	}
}

func TestHistogramCumulative(t *testing.T) {
	setup(t)
	HTTP("docker", "GET", 200, 10*time.Millisecond, 0, "")
	HTTP("docker", "GET", 200, 2*time.Second, 0, "")
	out := string(Get().render())
	// 10ms 落在 le=0.01 里，2s 落在 le=2.5 之前；累计桶必须是递增的
	if !strings.Contains(out, `mirrorlens_http_request_duration_seconds_bucket{scene="docker",le="0.01"} 1`) {
		t.Fatalf("0.01 桶不对:\n%s", out)
	}
	if !strings.Contains(out, `mirrorlens_http_request_duration_seconds_bucket{scene="docker",le="0.005"} 0`) {
		t.Fatalf("0.005 桶应为 0:\n%s", out)
	}
	if !strings.Contains(out, `mirrorlens_http_request_duration_seconds_bucket{scene="docker",le="+Inf"} 2`) {
		t.Fatalf("+Inf 桶不对:\n%s", out)
	}
	if !strings.Contains(out, `mirrorlens_http_request_duration_seconds_count{scene="docker"} 2`) {
		t.Fatalf("count 不对:\n%s", out)
	}
}

func TestSourceMetrics(t *testing.T) {
	setup(t)
	SourceResult("docker", true, 200, "single", 120*time.Millisecond)
	SourceResult("docker", false, 429, "single", 30*time.Millisecond)
	SourceResult("github", true, 206, "chunk", 50*time.Millisecond)
	SourceInflight(7, 1)
	SourceInflight(7, 1)
	SourceInflight(7, -1)
	out := string(Get().render())
	for _, want := range []string{
		`mirrorlens_source_requests_total{kind="docker",result="ok"} 1`,
		`mirrorlens_source_requests_total{kind="docker",result="fail"} 1`,
		`mirrorlens_source_429_total{kind="docker"} 1`,
		`mirrorlens_source_inflight{source_id="7"} 1`,
		`mirrorlens_source_fetch_duration_seconds_count{kind="github",mode="chunk"} 1`,
		`mirrorlens_source_fetch_duration_seconds_count{kind="docker",mode="single"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺 %s:\n%s", want, out)
		}
	}
	// 名额退回到 0 就该消失，不能留着一条 0 一直占标签
	SourceInflight(7, -1)
	if strings.Contains(string(Get().render()), `source_id="7"`) {
		t.Fatal("并发归零后不该还留着这条序列")
	}
}

// 429 率的分母只能是「真的收到响应」的次数：连不上/超时不该稀释它。
func Test429DenominatorCountsOnlyRealResponses(t *testing.T) {
	setup(t)
	SourceResult("docker", false, 429, "single", 0)
	SourceResult("docker", false, 0, "single", 0) // 连接失败，没有状态码
	SourceResult("docker", false, 0, "single", 0)
	out := string(Get().render())
	if !strings.Contains(out, `source_429_total{kind="docker"} 1`) {
		t.Fatalf("429 计数不对：\n%s", out)
	}
	if !strings.Contains(out, `source_http_responses_total{kind="docker"} 1`) {
		t.Fatalf("分母应只算真实响应（1），不是 3：\n%s", out)
	}
	if !strings.Contains(out, `source_requests_total{kind="docker",result="fail"} 3`) {
		t.Fatalf("总尝试次数仍应是 3：\n%s", out)
	}
}

// 吞吐只在未命中缓存、够大、够慢的响应上量：命中缓存时那是磁盘速度。
func TestThroughputOnlyOnRealFetches(t *testing.T) {
	setup(t)
	HTTP("docker", "GET", 200, 2*time.Second, 4<<20, "miss")
	HTTP("docker", "GET", 200, 2*time.Second, 4<<20, "hit")
	HTTP("docker", "GET", 200, 100*time.Millisecond, 4<<20, "miss")
	out := string(Get().render())
	if !strings.Contains(out, `mirrorlens_download_throughput_bytes_per_second_count{scene="docker"} 1`) {
		t.Fatalf("应只记一次吞吐样本:\n%s", out)
	}
	// 桶边界用定点写法，别出现 1.048576e+06
	if !strings.Contains(out, `le="1048576"`) || strings.Contains(out, "e+06") {
		t.Fatalf("le 格式不对:\n%s", out)
	}
}

func TestRateLimitedCounter(t *testing.T) {
	setup(t)
	RateLimited("ip")
	RateLimited("ip")
	RateLimited("token")
	out := string(Get().render())
	if !strings.Contains(out, `mirrorlens_ratelimit_rejected_total{layer="ip"} 2`) ||
		!strings.Contains(out, `mirrorlens_ratelimit_rejected_total{layer="token"} 1`) {
		t.Fatalf("限流计数不对:\n%s", out)
	}
}

// 没启用时所有记录函数都是空操作，中间件直接透传。
func TestDisabledIsNoop(t *testing.T) {
	Disable()
	HTTP("docker", "GET", 200, time.Second, 1, "hit")
	SourceResult("docker", true, 200, "single", time.Second)
	SourceInflight(1, 1)
	RateLimited("ip")
	if Get() != nil {
		t.Fatal("禁用时不该有注册表")
	}
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v2/", nil))
	if rec.Code != 204 {
		t.Fatalf("透传失败: %d", rec.Code)
	}
}

func TestSceneMapping(t *testing.T) {
	cases := map[string]string{
		"/v2/library/ubuntu/manifests/latest": "docker",
		"/gh/o/r/releases/download/v1/f.tgz":  "github-release",
		"/raw/o/r/main/f.txt":                 "github-raw",
		"/git/o/r.git/info/refs":              "git",
		"/http/example.com/a.zip":             "http",
		"/api/sources":                        "api",
		"/healthz":                            "healthz",
		"/":                                   "panel",
		"/index.html":                         "other",
	}
	for p, want := range cases {
		if got := Scene(p); got != want {
			t.Errorf("%s → %s，应为 %s", p, got, want)
		}
	}
}

func TestHandlerContentType(t *testing.T) {
	setup(t)
	RateLimited("ip") // 有一条样本，输出才不为空
	rec := httptest.NewRecorder()
	Get().Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Fatalf("Content-Type: %q", ct)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("空输出")
	}
}
