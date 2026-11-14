package gateway

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/store"
)

// HEAD 与 GET 共用 targets()/Rewrite：镜像可用、官方不可达时探测请求也要成功。
// 这里同时盯住记账口径（registry:<host>）和 Body 语义（HEAD 永不写 Body）。

const headHex = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func (f *moFixture) head(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodHead, "http://gw"+path, nil)
	r.SetBasicAuth("x", f.token)
	w := httptest.NewRecorder()
	f.gw.ServeRegistry(w, r)
	return w
}

type seRow struct {
	Kind    string
	Code    int
	OK      int
	Neutral int
}

// eventsOf 只读打开测试库，直接看 source_events 的 kind/code，避免用间接断言。
func eventsOf(t *testing.T, dbPath string, id int64) []seRow {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("打开只读库: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT kind,code,ok,neutral FROM source_events WHERE source_id=? ORDER BY id`, id)
	if err != nil {
		t.Fatalf("查事件: %v", err)
	}
	defer rows.Close()
	var out []seRow
	for rows.Next() {
		var r seRow
		if err := rows.Scan(&r.Kind, &r.Code, &r.OK, &r.Neutral); err != nil {
			t.Fatalf("读事件: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// 镜像 HEAD 200：官方零回源、只发 HEAD、不带 Body、头透传。
func TestBlobHeadUsesCandidates(t *testing.T) {
	f := newMOFixture(t)
	var nonHead atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			nonHead.Add(1)
		}
		if r.URL.Path != "/v2/library/alpine/blobs/sha256:"+headHex {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", "1234")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Docker-Content-Digest", "sha256:"+headHex)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	regID := f.addRegistrySource(t, srv, "native")

	w := f.head(t, "/v2/quay.io/library/alpine/blobs/sha256:"+headHex)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if nonHead.Load() != 0 {
		t.Fatalf("HEAD 被改成了别的请求方法（%d 次），必须原生 HEAD", nonHead.Load())
	}
	if f.officialHits.Load() != 0 {
		t.Fatalf("镜像命中时官方回源 %d 次，应为 0", f.officialHits.Load())
	}
	if w.Body.Len() != 0 {
		t.Fatalf("HEAD 不得返回 Body，实际 %d 字节", w.Body.Len())
	}
	if got := w.Header().Get("Content-Length"); got != "1234" {
		t.Errorf("Content-Length=%q，应透传上游 1234", got)
	}
	if got := w.Header().Get("Docker-Content-Digest"); got != "sha256:"+headHex {
		t.Errorf("digest=%q", got)
	}
	if got := w.Header().Get("X-ML-Source"); got != srv.URL {
		t.Errorf("X-ML-Source=%q，应为镜像 %q", got, srv.URL)
	}
	if got := w.Header().Get("X-ML-Cache"); got != "MISS" {
		t.Errorf("X-ML-Cache=%q", got)
	}

	ev := eventsOf(t, f.dbPath, regID)
	if len(ev) != 1 || ev[0].Kind != "registry:quay.io" || ev[0].Code != 200 || ev[0].OK != 1 {
		t.Fatalf("事件应只有一条 registry:quay.io/200/ok，实际 %+v", ev)
	}
	if fams := mustHealth(t, f.st, regID); fams["docker"] != 0 || fams["ghcr"] != 0 {
		t.Errorf("HEAD 成功写进了别的健康族: %v", fams)
	}
}

// 候选 403 的 GET 也必须回官方，且不能透传候选挑战。
func TestBlobGetMirror403FallsBackOfficial(t *testing.T) {
	f := newMOFixture(t)
	body := []byte("official-blob-after-mirror-403")
	sum := sha256.Sum256(body)
	f.officialBody = body
	digest := "sha256:" + hex.EncodeToString(sum[:])

	hdr := http.Header{}
	hdr.Set("WWW-Authenticate", `Bearer realm="https://auth.example/token",service="quay.io"`)
	srv := httptest.NewServer(&moMirror{
		path: "/v2/library/alpine/blobs/" + digest,
		hits: &f.nativeHits, status: 403, hdr: hdr,
	})
	defer srv.Close()
	regID := f.addRegistrySource(t, srv, "native")

	w := f.get(t, "/v2/quay.io/library/alpine/blobs/"+digest)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d，镜像 403 后应回官方并成功", w.Code)
	}
	if got := w.Body.String(); got != string(body) {
		t.Fatalf("body=%q，应拿到官方完整内容", got)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != "" {
		t.Fatalf("候选源挑战不应透传，得到 %q", got)
	}
	if f.officialHits.Load() != 1 {
		t.Fatalf("official=%d，应回官方一次", f.officialHits.Load())
	}
	ev := eventsOf(t, f.dbPath, regID)
	if len(ev) != 1 || ev[0].Code != 403 || ev[0].OK != 1 || ev[0].Neutral != 1 {
		t.Fatalf("候选 403 应记中性事件，实际 %+v", ev)
	}
}

// GET 的 429/5xx 是源自身失败：回官方，但候选必须记失败并冷却。
func TestBlobGetMirrorRetryableFailureCoolsSource(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newMOFixture(t)
			body := []byte("official-blob-after-mirror-retryable")
			sum := sha256.Sum256(body)
			f.officialBody = body
			digest := "sha256:" + hex.EncodeToString(sum[:])

			srv := httptest.NewServer(&moMirror{
				path: "/v2/library/alpine/blobs/" + digest,
				hits: &f.nativeHits, status: status,
			})
			defer srv.Close()
			regID := f.addRegistrySource(t, srv, "native")

			w := f.get(t, "/v2/quay.io/library/alpine/blobs/"+digest)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d，候选 %d 后应回官方成功", w.Code, status)
			}
			if got := w.Body.String(); got != string(body) {
				t.Fatalf("body=%q，应拿到官方完整内容", got)
			}
			if got := w.Header().Get("X-ML-Source"); got != "" {
				t.Fatalf("回官方时 X-ML-Source 应为空，得到 %q", got)
			}
			if f.nativeHits.Load() != 1 || f.officialHits.Load() != 1 {
				t.Fatalf("native=%d official=%d，期望各一次", f.nativeHits.Load(), f.officialHits.Load())
			}
			ev := eventsOf(t, f.dbPath, regID)
			if len(ev) != 1 || ev[0].Code != status || ev[0].OK != 0 || ev[0].Neutral != 0 {
				t.Fatalf("候选 %d 应记失败事件，实际 %+v", status, ev)
			}
			if n := len(f.gw.sched.Candidates("registry:quay.io")); n != 0 {
				t.Fatalf("候选 %d 应进入冷却，候选池=%d", status, n)
			}
		})
	}
}

// 候选 401 后官方 5xx：最终只能返回官方错误，不能泄漏候选挑战。
func TestBlobGetMirror401Official5xxDoesNotLeakChallenge(t *testing.T) {
	f := newMOFixture(t)
	f.officialStatus = http.StatusServiceUnavailable
	digest := "sha256:" + headHex
	mirrorHdr := http.Header{}
	mirrorHdr.Set("WWW-Authenticate", `Bearer realm="https://mirror.example/token",service="quay.io"`)
	srv := httptest.NewServer(&moMirror{
		path: "/v2/library/alpine/blobs/" + digest,
		hits: &f.nativeHits, status: 401, hdr: mirrorHdr,
	})
	defer srv.Close()
	f.addRegistrySource(t, srv, "native")

	w := f.get(t, "/v2/quay.io/library/alpine/blobs/"+digest)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d，应返回官方 503", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != "" {
		t.Fatalf("官方失败时不得回传候选挑战，得到 %q", got)
	}
	if got := w.Header().Get("X-ML-Source"); got != "" {
		t.Fatalf("回官方时 X-ML-Source 应为空，得到 %q", got)
	}
	if f.nativeHits.Load() != 1 || f.officialHits.Load() != 1 {
		t.Fatalf("native=%d official=%d，期望各一次", f.nativeHits.Load(), f.officialHits.Load())
	}
}

// Manifest GET 走 fetchOnce 的路径，官方 401 的挑战也不能丢。
func TestManifestGetOfficial401PassesChallenge(t *testing.T) {
	f := newMOFixture(t)
	challenge := `Bearer realm="https://official.example/token",service="quay.io"`
	f.officialStatus = http.StatusUnauthorized
	f.officialHdr = http.Header{"Www-Authenticate": {challenge}}
	mirrorHdr := http.Header{}
	mirrorHdr.Set("WWW-Authenticate", `Bearer realm="https://mirror.example/token",service="quay.io"`)
	srv := httptest.NewServer(&moMirror{
		path: "/v2/library/alpine/manifests/latest",
		hits: &f.nativeHits, status: 401, hdr: mirrorHdr,
	})
	defer srv.Close()
	f.addRegistrySource(t, srv, "native")

	w := f.get(t, "/v2/quay.io/library/alpine/manifests/latest")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d，应透传官方 401", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != challenge {
		t.Fatalf("WWW-Authenticate=%q，应透传官方挑战 %q", got, challenge)
	}
}

// 候选 401 的 GET 必须继续回官方，且不能把候选的挑战头冒充成最终结果。
func TestBlobGetMirror401FallsBackOfficial(t *testing.T) {
	f := newMOFixture(t)
	body := []byte("official-blob-after-mirror-401")
	sum := sha256.Sum256(body)
	f.officialBody = body
	digest := "sha256:" + hex.EncodeToString(sum[:])

	hdr := http.Header{}
	hdr.Set("WWW-Authenticate", `Bearer realm="https://auth.example/token",service="quay.io"`)
	srv := httptest.NewServer(&moMirror{
		path: "/v2/library/alpine/blobs/" + digest,
		hits: &f.nativeHits, status: 401, hdr: hdr,
	})
	defer srv.Close()
	regID := f.addRegistrySource(t, srv, "native")

	w := f.get(t, "/v2/quay.io/library/alpine/blobs/"+digest)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d，镜像 401 后应回官方并成功", w.Code)
	}
	if got := w.Body.String(); got != string(body) {
		t.Fatalf("body=%q，应拿到官方完整内容", got)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != "" {
		t.Fatalf("候选源挑战不应透传，得到 %q", got)
	}
	if n := f.officialHits.Load(); n != 1 {
		t.Fatalf("official=%d，应回官方一次", n)
	}
	ev := eventsOf(t, f.dbPath, regID)
	if len(ev) != 1 || ev[0].Code != 401 || ev[0].OK != 1 || ev[0].Neutral != 1 {
		t.Fatalf("候选 401 应记中性事件，实际 %+v", ev)
	}
}

// 候选与官方都要求认证：官方挑战必须透传，候选挑战不能覆盖它。
func TestBlobGetOfficial401PassesChallenge(t *testing.T) {
	f := newMOFixture(t)
	challenge := `Bearer realm="https://official.example/token",service="quay.io"`
	f.officialStatus = http.StatusUnauthorized
	f.officialHdr = http.Header{"Www-Authenticate": {challenge}}
	digest := "sha256:" + headHex

	mirrorHdr := http.Header{}
	mirrorHdr.Set("WWW-Authenticate", `Bearer realm="https://mirror.example/token",service="quay.io"`)
	srv := httptest.NewServer(&moMirror{
		path: "/v2/library/alpine/blobs/" + digest,
		hits: &f.nativeHits, status: 401, hdr: mirrorHdr,
	})
	defer srv.Close()
	f.addRegistrySource(t, srv, "native")

	w := f.get(t, "/v2/quay.io/library/alpine/blobs/"+digest)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d，应透传官方 401", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != challenge {
		t.Fatalf("WWW-Authenticate=%q，应透传官方挑战 %q", got, challenge)
	}
	if n := f.officialHits.Load(); n != 1 {
		t.Fatalf("official=%d，应尝试官方一次", n)
	}
	if got := w.Header().Get("X-ML-Source"); got != "" {
		t.Fatalf("回官方时 X-ML-Source 应为空，得到 %q", got)
	}
	if n := f.nativeHits.Load(); n != 1 {
		t.Fatalf("native=%d，候选应先尝试一次", n)
	}
}

// 镜像 HEAD 404：这是未命中，不是源故障，按既定策略回官方。
func TestBlobHeadMirrorMissFallsBackOfficial(t *testing.T) {
	f := newMOFixture(t)
	srv := httptest.NewServer(&moMirror{path: "/nope", status: 404, hits: &f.nativeHits})
	defer srv.Close()
	regID := f.addRegistrySource(t, srv, "native")

	w := f.head(t, "/v2/quay.io/library/alpine/blobs/sha256:"+headHex)
	if w.Code != 200 || f.officialHits.Load() != 1 {
		t.Fatalf("status=%d official=%d，404 应回官方一次", w.Code, f.officialHits.Load())
	}
	if w.Body.Len() != 0 {
		t.Fatalf("HEAD 不得返回 Body")
	}
	if got := w.Header().Get("X-ML-Source"); got != "" {
		t.Errorf("官方供货不该带镜像名，得到 %q", got)
	}
	ev := eventsOf(t, f.dbPath, regID)
	if len(ev) != 1 || ev[0].Code != 404 || ev[0].OK != 1 {
		t.Fatalf("404 应记成未命中（ok=1），实际 %+v", ev)
	}
	// 未命中不冷却：下一次还能选它。
	if n := len(f.gw.sched.Candidates("registry:quay.io")); n != 1 {
		t.Errorf("404 后候选池=%d，不应冷却", n)
	}
}

// 镜像 HEAD 429：记 registry 维度 + Retry-After，进冷却，官方兜底。
func TestBlobHeadMirror429(t *testing.T) {
	f := newMOFixture(t)
	hdr := http.Header{}
	hdr.Set("Retry-After", "30")
	srv := httptest.NewServer(&moMirror{
		path: "/v2/library/alpine/blobs/sha256:" + headHex,
		hits: &f.nativeHits, hdr: hdr, status: 429,
	})
	defer srv.Close()
	regID := f.addRegistrySource(t, srv, "native")

	w := f.head(t, "/v2/quay.io/library/alpine/blobs/sha256:"+headHex)
	if w.Code != 200 || f.officialHits.Load() != 1 {
		t.Fatalf("status=%d official=%d，429 应回官方一次", w.Code, f.officialHits.Load())
	}
	ev := eventsOf(t, f.dbPath, regID)
	if len(ev) != 1 || ev[0].Kind != "registry:quay.io" || ev[0].Code != 429 || ev[0].OK != 0 {
		t.Fatalf("429 应记成 registry:quay.io 的失败，实际 %+v", ev)
	}
	if n := len(f.gw.sched.Candidates("registry:quay.io")); n != 0 {
		t.Errorf("429 后应进冷却，候选池=%d", n)
	}
	for _, r := range ev {
		if r.Kind == "docker" || r.Kind == "ghcr" {
			t.Errorf("429 写进了别的维度: %+v", r)
		}
	}
}

// 镜像 HEAD 503：熔断并回官方。
func TestBlobHeadMirror503(t *testing.T) {
	f := newMOFixture(t)
	srv := httptest.NewServer(&moMirror{
		path: "/v2/library/alpine/blobs/sha256:" + headHex,
		hits: &f.nativeHits, status: 503,
	})
	defer srv.Close()
	regID := f.addRegistrySource(t, srv, "native")

	w := f.head(t, "/v2/quay.io/library/alpine/blobs/sha256:"+headHex)
	if w.Code != 200 || f.officialHits.Load() != 1 {
		t.Fatalf("status=%d official=%d", w.Code, f.officialHits.Load())
	}
	ev := eventsOf(t, f.dbPath, regID)
	if len(ev) != 1 || ev[0].Code != 503 || ev[0].OK != 0 {
		t.Fatalf("503 应记失败，实际 %+v", ev)
	}
	if n := len(f.gw.sched.Candidates("registry:quay.io")); n != 0 {
		t.Errorf("503 后应熔断，候选池=%d", n)
	}
}

// 镜像连接失败：官方成功。
func TestBlobHeadMirrorDialFailFallsBackOfficial(t *testing.T) {
	f := newMOFixture(t)
	srv := httptest.NewServer(&moMirror{
		path: "/v2/library/alpine/blobs/sha256:" + headHex,
		hits: &f.nativeHits,
	})
	regID := f.addRegistrySource(t, srv, "native")
	srv.Close() // 连不上

	w := f.head(t, "/v2/quay.io/library/alpine/blobs/sha256:"+headHex)
	if w.Code != 200 || f.officialHits.Load() != 1 {
		t.Fatalf("status=%d official=%d，连接失败应回官方", w.Code, f.officialHits.Load())
	}
	ev := eventsOf(t, f.dbPath, regID)
	if len(ev) != 1 || ev[0].OK != 0 || ev[0].Code != 0 {
		t.Fatalf("网络失败应记 ok=0/code=0，实际 %+v", ev)
	}
}

// 所有候选都失败、官方也失败：把最后的状态交出去，不能假装成功。
func TestBlobHeadAllTargetsFail(t *testing.T) {
	f := newMOFixture(t)
	srv := httptest.NewServer(&moMirror{
		path: "/v2/library/alpine/blobs/sha256:" + headHex,
		hits: &f.nativeHits, status: 503,
	})
	defer srv.Close()
	f.addRegistrySource(t, srv, "native")
	f.officialStatus = 503

	w := f.head(t, "/v2/quay.io/library/alpine/blobs/sha256:"+headHex)
	if w.Code != 503 {
		t.Fatalf("status=%d，全失败应返回最后一次上游状态 503", w.Code)
	}
	if f.officialHits.Load() != 1 {
		t.Fatalf("官方应被尝试一次，实际 %d", f.officialHits.Load())
	}
	if w.Body.Len() != 0 {
		t.Fatalf("失败的 HEAD 也不得带 Body，实际 %d 字节", w.Body.Len())
	}
}

// 官方 401：必须把 WWW-Authenticate 原样交给客户端。
func TestBlobHeadPassesAuthChallenge(t *testing.T) {
	f := newMOFixture(t)
	challenge := `Bearer realm="https://auth.example/token",service="quay.io"`
	f.officialStatus = 401
	f.officialHdr = http.Header{"Www-Authenticate": {challenge}}

	w := f.head(t, "/v2/quay.io/library/alpine/blobs/sha256:"+headHex)
	if w.Code != 401 {
		t.Fatalf("status=%d，应透传 401", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != challenge {
		t.Fatalf("WWW-Authenticate=%q，应原样透传", got)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("401 的 HEAD 不得带 Body")
	}
	if got := w.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("失败响应应禁止缓存，Cache-Control=%q", got)
	}
}

// 镜像 401：候选源不能把挑战冒充官方结论，必须继续回退官方；官方 200 则成功。
func TestBlobHeadMirror401FallsBackOfficial(t *testing.T) {
	f := newMOFixture(t)
	challenge := `Bearer realm="https://auth.example/token",service="quay.io"`
	hdr := http.Header{}
	hdr.Set("WWW-Authenticate", challenge)
	srv := httptest.NewServer(&moMirror{
		path: "/v2/library/alpine/blobs/sha256:" + headHex,
		hits: &f.nativeHits, status: 401, hdr: hdr,
	})
	defer srv.Close()
	regID := f.addRegistrySource(t, srv, "native")

	w := f.head(t, "/v2/quay.io/library/alpine/blobs/sha256:"+headHex)
	if w.Code != 200 {
		t.Fatalf("status=%d，镜像 401 后应回官方并成功", w.Code)
	}
	if n := f.officialHits.Load(); n != 1 {
		t.Fatalf("official=%d，应回官方一次", n)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != "" {
		t.Fatalf("候选源的挑战不能透传，得到 %q", got)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("HEAD 不得带 Body")
	}
	ev := eventsOf(t, f.dbPath, regID)
	if len(ev) != 1 || ev[0].Code != 401 || ev[0].OK != 1 || ev[0].Neutral != 1 {
		t.Fatalf("镜像 401 应记中性事件，实际 %+v", ev)
	}
	// 401 不进冷却：下一次仍然会先问镜像。
	_ = f.head(t, "/v2/quay.io/library/alpine/blobs/sha256:"+headHex)
	if n := f.nativeHits.Load(); n != 2 {
		t.Fatalf("mirror hits=%d，401 不应让镜像进冷却", n)
	}
}

// 候选 403 与 401 同口径：不冷却、不冒充官方挑战，继续回官方。
func TestBlobHeadMirror403FallsBackOfficial(t *testing.T) {
	f := newMOFixture(t)
	hdr := http.Header{}
	hdr.Set("WWW-Authenticate", `Bearer realm="https://mirror.example/token",service="quay.io"`)
	srv := httptest.NewServer(&moMirror{
		path: "/v2/library/alpine/blobs/sha256:" + headHex,
		hits: &f.nativeHits, status: 403, hdr: hdr,
	})
	defer srv.Close()
	regID := f.addRegistrySource(t, srv, "native")

	w := f.head(t, "/v2/quay.io/library/alpine/blobs/sha256:"+headHex)
	if w.Code != 200 {
		t.Fatalf("status=%d，镜像 403 后应回官方并成功", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != "" {
		t.Fatalf("候选 403 挑战不应透传，得到 %q", got)
	}
	if f.officialHits.Load() != 1 {
		t.Fatalf("official=%d，应回官方一次", f.officialHits.Load())
	}
	ev := eventsOf(t, f.dbPath, regID)
	if len(ev) != 1 || ev[0].Code != 403 || ev[0].OK != 1 || ev[0].Neutral != 1 {
		t.Fatalf("镜像 403 应记中性事件，实际 %+v", ev)
	}
}

// 候选和官方都要鉴权：最终只能透传官方挑战，候选挑战必须丢弃。
func TestBlobHeadMirror401Official401PassesOfficialChallenge(t *testing.T) {
	f := newMOFixture(t)
	officialChallenge := `Bearer realm="https://official.example/token",service="quay.io"`
	f.officialStatus = 401
	f.officialHdr = http.Header{"Www-Authenticate": {officialChallenge}}
	mirrorHdr := http.Header{}
	mirrorHdr.Set("WWW-Authenticate", `Bearer realm="https://mirror.example/token",service="quay.io"`)
	srv := httptest.NewServer(&moMirror{
		path: "/v2/library/alpine/blobs/sha256:" + headHex,
		hits: &f.nativeHits, status: 401, hdr: mirrorHdr,
	})
	defer srv.Close()
	f.addRegistrySource(t, srv, "native")

	w := f.head(t, "/v2/quay.io/library/alpine/blobs/sha256:"+headHex)
	if w.Code != 401 {
		t.Fatalf("status=%d，应透传官方 401", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != officialChallenge {
		t.Fatalf("WWW-Authenticate=%q，应透传官方挑战 %q", got, officialChallenge)
	}
	if f.nativeHits.Load() != 1 || f.officialHits.Load() != 1 {
		t.Fatalf("native=%d official=%d，期望各一次", f.nativeHits.Load(), f.officialHits.Load())
	}
}

// 候选 401 后官方 5xx：不能把候选 Challenge 当成最终响应。
func TestBlobHeadMirror401Official5xxDoesNotLeakChallenge(t *testing.T) {
	f := newMOFixture(t)
	f.officialStatus = 503
	mirrorHdr := http.Header{}
	mirrorHdr.Set("WWW-Authenticate", `Bearer realm="https://mirror.example/token",service="quay.io"`)
	srv := httptest.NewServer(&moMirror{
		path: "/v2/library/alpine/blobs/sha256:" + headHex,
		hits: &f.nativeHits, status: 401, hdr: mirrorHdr,
	})
	defer srv.Close()
	f.addRegistrySource(t, srv, "native")

	w := f.head(t, "/v2/quay.io/library/alpine/blobs/sha256:"+headHex)
	if w.Code != 503 {
		t.Fatalf("status=%d，应返回官方 503", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != "" {
		t.Fatalf("官方失败时不得回传候选挑战，得到 %q", got)
	}
	if f.nativeHits.Load() != 1 || f.officialHits.Load() != 1 {
		t.Fatalf("native=%d official=%d，期望各一次", f.nativeHits.Load(), f.officialHits.Load())
	}
}

// 429 的 Retry-After 必须进调度：冷却至少到 retry-after 之后。
func TestBlobHead429HonorsRetryAfter(t *testing.T) {
	f := newMOFixture(t)
	hdr := http.Header{}
	hdr.Set("Retry-After", "120")
	srv := httptest.NewServer(&moMirror{
		path: "/v2/library/alpine/blobs/sha256:" + headHex,
		hits: &f.nativeHits, hdr: hdr, status: 429,
	})
	defer srv.Close()
	f.addRegistrySource(t, srv, "native")

	if w := f.head(t, "/v2/quay.io/library/alpine/blobs/sha256:"+headHex); w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	// 冷却期内（测试时钟不前进）该源不应再出现在候选里。
	if n := len(f.gw.sched.Candidates("registry:quay.io")); n != 0 {
		t.Fatalf("Retry-After 未生效，候选池=%d", n)
	}
}

// 保底：HEAD 不会污染另一台 docker 源的健康记录（用户点名的验证项）。
func TestBlobHeadKeepsDockerHealthClean(t *testing.T) {
	f := newMOFixture(t)
	srv := httptest.NewServer(&moMirror{
		path: "/v2/library/alpine/blobs/sha256:" + headHex,
		hits: &f.nativeHits, status: 500,
	})
	defer srv.Close()
	regID := f.addRegistrySource(t, srv, "native")
	dockerID, err := f.st.AddSource(store.Source{
		BaseURL: "https://hub.example", Name: "hub", DockerCap: 1, Status: "usable", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if w := f.head(t, "/v2/quay.io/library/alpine/blobs/sha256:"+headHex); w.Code != 200 {
		t.Fatalf("status=%d，500 后应回官方成功", w.Code)
	}
	for _, v := range mustHealth(t, f.st, dockerID) {
		if v != 0 {
			t.Errorf("docker 源被 HEAD 失败污染: %d", v)
		}
	}
	if v := mustHealth(t, f.st, regID)["docker"]; v != 0 {
		t.Errorf("registry 源在 docker 族留下 %d 条失败", v)
	}
	if n := len(f.gw.sched.Candidates("docker")); n != 1 {
		t.Errorf("docker 候选池=%d，应为 1", n)
	}
}
