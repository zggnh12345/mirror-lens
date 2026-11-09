package metrics

import (
	"fmt"
	"strings"
	"sync"
)

// GitHub Release 元数据查询的计数。按结果分类，便于区分「正常降级」和「真的出问题」：
//
//	ok / no_digest / not_found        → 正常
//	api_schema_unexpected            → API 结构变了，需要人看
//	unsupported_algorithm            → 上游换了算法，明确降级
//	auth_failed                      → Token 失效/被吊销，需改配置
//	rate_limited / timeout / unavailable → 外部故障，走降级路径
var (
	ghMu   sync.Mutex
	ghHits = map[string]uint64{}
)

func GHMetaResult(result string) {
	ghMu.Lock()
	ghHits[labelResult(result)]++
	ghMu.Unlock()
}

// labelResult 把内部结果名映射成指标标签值。
// "no_token" 不能原样进指标：凭据相关的字样不出现在 /metrics 里。
func labelResult(r string) string {
	if r == "no_token" {
		return "no_credentials"
	}
	return r
}

func (r *Registry) writeGHMeta(b *strings.Builder) {
	ghMu.Lock()
	snapshot := make(map[string]uint64, len(ghHits))
	for k, v := range ghHits {
		snapshot[k] = v
	}
	ghMu.Unlock()
	b.WriteString("# HELP " + prefix + "github_meta_total GitHub Release 摘要查询次数（按结果）\n")
	b.WriteString("# TYPE " + prefix + "github_meta_total counter\n")
	for _, k := range []string{
		"ok", "no_digest", "not_found", "api_schema_unexpected",
		"unsupported_algorithm", "auth_failed", "rate_limited", "timeout", "unavailable", "no_credentials",
	} {
		fmt.Fprintf(b, "%sgithub_meta_total{result=%q} %d\n", prefix, k, snapshot[k])
	}
}
