package probe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	// 测试里的假源都在 127.0.0.1 上。
	Configure(true)
	m.Run()
	Configure(false)
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// fakeRegistry 是一台只实现探测用到的那几个接口的假 registry。
type fakeRegistry struct {
	manifest    []byte
	manifestCT  string
	contentDgst string // 空则按实际字节算
	blob        []byte
	blobStatus  int
	v2Delay     time.Duration
	blobHits    int
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v2/":
		if f.v2Delay > 0 {
			time.Sleep(f.v2Delay)
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "{}")
	case strings.HasPrefix(r.URL.Path, "/v2/"+probeImage+"/manifests/"):
		ct := f.manifestCT
		if ct == "" {
			ct = "application/vnd.docker.distribution.manifest.v2+json"
		}
		w.Header().Set("Content-Type", ct)
		dg := f.contentDgst
		if dg == "" {
			dg = sha(f.manifest)
		}
		w.Header().Set("Docker-Content-Digest", dg)
		w.Write(f.manifest)
	case strings.HasPrefix(r.URL.Path, "/v2/"+probeImage+"/blobs/"):
		f.blobHits++
		if f.blobStatus != 0 {
			w.WriteHeader(f.blobStatus)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(f.blob)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func manifestFor(layer []byte, layerDigest string) []byte {
	if layerDigest == "" {
		layerDigest = sha(layer)
	}
	m := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.docker.distribution.manifest.v2+json",
		"config": map[string]any{
			"mediaType": "application/vnd.docker.container.image.v1+json",
			"size":      10,
			"digest":    "sha256:" + strings.Repeat("a", 64),
		},
		"layers": []any{map[string]any{
			"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip",
			"size":      len(layer),
			"digest":    layerDigest,
		}},
	}
	b, _ := json.Marshal(m)
	return b
}

func TestDockerManifestDigestMismatch(t *testing.T) {
	layer := []byte("hello layer")
	f := &fakeRegistry{manifest: manifestFor(layer, ""), blob: layer}
	// 头里写一个别的摘要：内容与声明对不上。
	f.contentDgst = "sha256:" + strings.Repeat("b", 64)
	srv := httptest.NewServer(f)
	defer srv.Close()

	res := Docker(context.Background(), srv.URL, 2, "")
	if res.DigestOK != 0 {
		t.Fatalf("头与字节不符时应判 0，得到 %d", res.DigestOK)
	}
	if res.Digest == f.contentDgst {
		t.Fatalf("摘要应取自实际字节，不能照抄响应头")
	}
	if res.Digest != sha(f.manifest) {
		t.Fatalf("摘要应为内容哈希，得到 %s", res.Digest)
	}
}

func TestDockerBlobVerified(t *testing.T) {
	layer := make([]byte, 2<<20)
	for i := range layer {
		layer[i] = byte(i * 7)
	}
	f := &fakeRegistry{manifest: manifestFor(layer, ""), blob: layer, v2Delay: 600 * time.Millisecond}
	srv := httptest.NewServer(f)
	defer srv.Close()

	res := Docker(context.Background(), srv.URL, 3, "")
	if !res.OK {
		t.Fatalf("应可用，得到 error=%q", res.Error)
	}
	if res.BlobOK != 1 {
		t.Fatalf("整层校验应通过，得到 %d", res.BlobOK)
	}
	// 测速只算 blob 那一段：/v2/ 故意慢了 600ms，算进去就过不了 6MB/s 这条线。
	if res.SpeedKBps < 6*1024 {
		t.Fatalf("测速把前置请求算进去了？得到 %d KB/s", res.SpeedKBps)
	}
}

func TestDockerBlobDigestMismatch(t *testing.T) {
	want := []byte("the real layer bytes")
	f := &fakeRegistry{manifest: manifestFor(want, ""), blob: []byte("the fake layer bytes")} // 同样长度，内容不同
	if len(f.blob) != len(want) {
		t.Fatalf("测试数据长度要一致：%d vs %d", len(f.blob), len(want))
	}
	srv := httptest.NewServer(f)
	defer srv.Close()

	res := Docker(context.Background(), srv.URL, 3, "")
	if res.BlobOK != 0 {
		t.Fatalf("内容被换掉时应判 0，得到 %d", res.BlobOK)
	}
	if res.OK {
		t.Fatalf("摘要不符的源不该算可用")
	}
}

func TestDockerBlobFailureMarksUnusable(t *testing.T) {
	layer := []byte("hello layer")
	f := &fakeRegistry{manifest: manifestFor(layer, ""), blobStatus: http.StatusInternalServerError}
	srv := httptest.NewServer(f)
	defer srv.Close()

	res := Docker(context.Background(), srv.URL, 3, "")
	if res.OK {
		t.Fatalf("L3 拿不到 blob 时不应报 OK")
	}
	if !strings.Contains(res.Error, "blob") {
		t.Fatalf("错误里应说明 blob 失败，得到 %q", res.Error)
	}
}

func TestDockerLargeLayerNotClaimedVerified(t *testing.T) {
	// 层比 cap 大：只测速，不能宣称摘要一致。
	big := make([]byte, blobCap+1024)
	f := &fakeRegistry{manifest: manifestFor(big, ""), blob: big}
	srv := httptest.NewServer(f)
	defer srv.Close()

	res := Docker(context.Background(), srv.URL, 3, "")
	if res.BlobOK != -1 {
		t.Fatalf("没整份下载时应为 -1，得到 %d", res.BlobOK)
	}
	if !res.OK || res.SpeedKBps <= 0 {
		t.Fatalf("仍应可用并测到速度：ok=%v speed=%d err=%q", res.OK, res.SpeedKBps, res.Error)
	}
}

func TestProbeBlocksPrivateByDefault(t *testing.T) {
	layer := []byte("hello layer")
	srv := httptest.NewServer(&fakeRegistry{manifest: manifestFor(layer, ""), blob: layer})
	defer srv.Close()

	Configure(false)
	defer Configure(true)

	res := Docker(context.Background(), srv.URL, 1, "")
	if res.OK {
		t.Fatalf("默认策略下不该能探测回环地址")
	}
	if res.Error == "" {
		t.Fatalf("应给出被拦截的原因")
	}
}
