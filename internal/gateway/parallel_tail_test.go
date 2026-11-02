package gateway

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
)

// 尾部扣留的核心承诺：哈希没对上时，客户端拿不到完整长度的响应。
// 旧行为是「长度正确的 200 + 错内容」，客户端只能靠自己算摘要才发现。
func TestParallelHoldsTailWhenHashMismatch(t *testing.T) {
	g, st, _, body, _, mirror, os_, ms := parFixture(t, 9<<20)
	g.par.TailVerify = true
	// 破坏点落在最后 1MB 里：这些字节正是被扣住、还没发给客户端的那段。
	mirror.corruptAt = int64(len(body)) - 512<<10

	plan, cand := parPlan(t, os_, body)
	g.parTargets = func(*adapter.Plan) []fetchTarget {
		return []fetchTarget{{u: mustURL(t, ms.URL), c: cand}, {u: plan.Upstream}}
	}
	rec := parRun(g, plan)

	hold := tailHold(int64(len(body)))
	if want := int64(len(body)) - hold; int64(rec.Body.Len()) > want {
		t.Fatalf("校验失败前不该把尾巴发出去：发了 %d，最多 %d", rec.Body.Len(), want)
	}
	if e, _ := st.CacheGet(plan.CacheKey); e != nil {
		t.Fatal("校验失败的内容不该进缓存索引")
	}
}

// 正常路径不能因为扣留而少发字节。
func TestParallelTailVerifyDeliversWholeBody(t *testing.T) {
	g, st, _, body, _, _, os_, ms := parFixture(t, 9<<20)
	g.par.TailVerify = true
	plan, cand := parPlan(t, os_, body)
	g.parTargets = func(*adapter.Plan) []fetchTarget {
		return []fetchTarget{{u: mustURL(t, ms.URL), c: cand}, {u: plan.Upstream}}
	}
	rec := parRun(g, plan)

	if rec.Code != http.StatusOK {
		t.Fatalf("应返回 200，得到 %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("内容不完整或不一致：%d vs %d", rec.Body.Len(), len(body))
	}
	if e, _ := st.CacheGet(plan.CacheKey); e == nil {
		t.Fatal("校验通过后应写入缓存索引")
	}
}

// 关掉开关必须回到旧行为，这是出事时的一键退路。
func TestParallelTailVerifyCanBeDisabled(t *testing.T) {
	g, _, _, body, _, _, os_, ms := parFixture(t, 9<<20)
	g.par.TailVerify = false
	plan, cand := parPlan(t, os_, body)
	g.parTargets = func(*adapter.Plan) []fetchTarget {
		return []fetchTarget{{u: mustURL(t, ms.URL), c: cand}, {u: plan.Upstream}}
	}
	rec := parRun(g, plan)
	if rec.Body.Len() != len(body) {
		t.Fatalf("关掉扣留后应原样发完：%d vs %d", rec.Body.Len(), len(body))
	}
}
