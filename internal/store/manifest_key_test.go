package store

import "testing"

// TestManifestCacheKeyEndpointIdentity 锁定 Manifest 键身份：
// Scheme + CanonicalHost + EffectivePort，不含调度期 SourceEndpoint。
func TestManifestCacheKeyEndpointIdentity(t *testing.T) {
	cases := []struct {
		name   string
		epoch  int64
		scheme string
		host   string
		port   int
		want   string
	}{
		{
			name:   "HTTPS 默认端口显式化",
			scheme: "https", host: "registry.example.com", port: 0,
			want: "docker:manifest:https://registry.example.com:443/library/alpine:latest|accept=x",
		},
		{
			name:   "HTTPS 443 与省略等价",
			scheme: "https", host: "registry.example.com", port: 443,
			want: "docker:manifest:https://registry.example.com:443/library/alpine:latest|accept=x",
		},
		{
			name:   "非默认端口保留",
			scheme: "https", host: "registry.example.com", port: 5443,
			want: "docker:manifest:https://registry.example.com:5443/library/alpine:latest|accept=x",
		},
		{
			name:   "HTTP 与 HTTPS 隔离",
			scheme: "http", host: "registry.example.com", port: 0,
			want: "docker:manifest:http://registry.example.com:80/library/alpine:latest|accept=x",
		},
		{
			name:   "IPv6 方括号",
			scheme: "https", host: "2001:db8::10", port: 5443,
			want: "docker:manifest:https://[2001:db8::10]:5443/library/alpine:latest|accept=x",
		},
		{
			name:  "Epoch 前缀",
			epoch: 7, scheme: "https", host: "registry.example.com", port: 0,
			want: "docker:m:7:https://registry.example.com:443/library/alpine:latest|accept=x",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ManifestCacheKey(tc.epoch, tc.scheme, tc.host, tc.port, "library/alpine", "latest", "x")
			if got != tc.want {
				t.Fatalf("got %q，期望 %q", got, tc.want)
			}
			if k := RegistryCacheKind(got); k != "manifest" {
				t.Fatalf("RegistryCacheKind=%q", k)
			}
		})
	}
}

func TestManifestCacheKeySchemeIsolation(t *testing.T) {
	https := ManifestCacheKey(0, "https", "h.example", 0, "a/b", "1", "x")
	http := ManifestCacheKey(0, "http", "h.example", 0, "a/b", "1", "x")
	if https == http {
		t.Fatal("http/https 必须产生不同键，否则跨协议串缓存")
	}
	p1 := ManifestCacheKey(0, "https", "h.example", 443, "a/b", "1", "x")
	p2 := ManifestCacheKey(0, "https", "h.example", 5443, "a/b", "1", "x")
	if p1 == p2 {
		t.Fatal("不同 EffectivePort 必须产生不同键")
	}
}
