package metrics

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// 高并发下计数必须正确且无 race：64 个 goroutine 各打 256 次，
// 总数应恰好 16384，且 -race 下不报数据竞争。
func TestConcurrentIncrements(t *testing.T) {
	setup(t)
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-ML-Cache", "HIT")
		w.WriteHeader(200)
	}))
	const workers, per = 64, 256
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			for j := 0; j < per; j++ {
				h.ServeHTTP(rec, httptest.NewRequest("GET", "/v2/x/blobs/sha256:a", nil))
			}
		}()
	}
	wg.Wait()
	SnapshotSceneHotness()
	want := uint64(workers * per)
	if got := Get().sceneTotal["docker"].Load(); got != want {
		t.Fatalf("并发计数应为 %d，实得 %d", want, got)
	}
}
