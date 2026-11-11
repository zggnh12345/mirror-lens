package scheduler

import (
	"net/url"
	"testing"
)

// 改写路径后必须清空 RawPath：否则 url.String() 会在「旧 RawPath 恰好仍是
// 新 Path 的合法编码」时原样吐出旧转义，把请求发到错的路径上。
func TestRewriteClearsRawPathOnHostPrefix(t *testing.T) {
	var s Scheduler
	// 客户端发来带转义字符的路径，Go 解析后 Path 是解码值、RawPath 是原文。
	base := &url.URL{
		Scheme:   "https",
		Host:     "quay.io",
		Path:     "/v2/prometheus/node-exporter/manifests/v1.8.0+meta",
		RawPath:  "/v2/prometheus/node-exporter/manifests/v1.8.0+meta",
		RawQuery: "ns=docker",
	}

	got, ok := s.Rewrite("registry:quay.io", base,
		Candidate{BaseURL: "https://mirror-b.example", RegistryPath: "host_prefix"})
	if !ok {
		t.Fatal("host_prefix 应改写成功")
	}
	if got.RawPath != "" {
		t.Fatalf("改写后 RawPath 必须清空，实际 %q", got.RawPath)
	}
	if got.Path != "/v2/quay.io/prometheus/node-exporter/manifests/v1.8.0+meta" {
		t.Fatalf("路径改写不对：%q", got.Path)
	}
	if q := got.RawQuery; q != "ns=docker" {
		t.Fatalf("查询串应原样保留，实际 %q", q)
	}
	// 序列化结果必须自洽：再解析回来等于同一个路径，且没有多余转义。
	back, err := url.Parse(got.String())
	if err != nil {
		t.Fatalf("序列化后无法解析: %v", err)
	}
	if back.Path != got.Path || back.RawPath != "" {
		t.Fatalf("往返不一致：Path=%q RawPath=%q", back.Path, back.RawPath)
	}
}

// 带已编码斜杠（%2F）的路径改写后不能被旧 RawPath 带偏。
func TestRewriteClearsRawPathWithEscapedSlash(t *testing.T) {
	var s Scheduler
	base, err := url.Parse("https://quay.io/v2/lib%2Fsub/manifests/v1")
	if err != nil {
		t.Fatal(err)
	}
	if base.RawPath == "" {
		t.Fatal("构造失败：应带 RawPath")
	}

	got, ok := s.Rewrite("registry:quay.io", base,
		Candidate{BaseURL: "https://mirror-b.example", RegistryPath: "host_prefix"})
	if !ok {
		t.Fatal("host_prefix 应改写成功")
	}
	if got.RawPath != "" {
		t.Fatalf("改写后 RawPath 必须清空，实际 %q", got.RawPath)
	}
	want := "https://mirror-b.example/v2/quay.io/lib/sub/manifests/v1"
	if got.String() != want {
		t.Fatalf("序列化结果应为 %q，实际 %q", want, got.String())
	}
}
