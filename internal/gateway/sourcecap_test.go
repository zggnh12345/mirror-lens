package gateway

import (
	"bytes"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/scheduler"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

func runCapCase(t *testing.T, size, maxPerSource int) (hits, maxCur int32, rec *httptest.ResponseRecorder, body []byte) {
	t.Helper()
	g, st, _, payload, _, mirror, os, ms := parFixture(t, size)
	plan, cand := parPlan(t, os, payload)

	sched := scheduler.New(st, scheduler.Config{Enabled: true, MaxPerSource: maxPerSource})
	g.SetScheduler(sched)

	id, err := st.AddSource(store.Source{
		Name: "镜像A", BaseURL: "http://mirror", Status: "usable", Enabled: true, DockerCap: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	cand.ID = id

	mirror.delay = 15 * time.Millisecond
	g.parTargets = func(*adapter.Plan) []fetchTarget {
		return []fetchTarget{
			{u: mustURL(t, ms.URL), c: cand},
			{u: plan.Upstream},
		}
	}

	rec = parRun(g, plan)
	return mirror.hits, mirror.maxCur, rec, payload
}

// 单源名额=1 时，同一台源同时只能有一个分片在下载；结果仍然必须完整。
func TestParallelChunksRespectPerSourceCap(t *testing.T) {
	hits, maxCur, rec, body := runCapCase(t, 32<<20, 1)
	if rec.Code != 200 {
		t.Fatalf("应正常返回，得到 %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("正文不完整：%d/%d 字节", rec.Body.Len(), len(body))
	}
	if hits < 3 {
		t.Fatalf("应真的走了分片，实际只有 %d 次请求", hits)
	}
	if maxCur > 1 {
		t.Fatalf("单源并发应被压到 1，实际峰值 %d", maxCur)
	}
}

// 对照：不限并发时同一套计数器必须能看到 >1，否则上面那条断言等于没测。
func TestParallelChunksUncappedSeesConcurrency(t *testing.T) {
	_, maxCur, _, _ := runCapCase(t, 32<<20, -1)
	if maxCur < 2 {
		t.Fatalf("不限并发时应观察到并发，实际峰值 %d", maxCur)
	}
}
