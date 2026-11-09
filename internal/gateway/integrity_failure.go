package gateway

import (
	"log"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/logging"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// recordIntegrityFailure 记一条「这次获取的字节和预期摘要不符」的事件。
//
// 只写事件表，不动 blobs：actual 指向的字节未必是坏的，坏的是这次获取过程。
// 事件写失败也不影响下载结果和资源释放——调用方只需要日志。
func (g *Gateway) recordIntegrityFailure(plan *adapter.Plan, actual string, tgt *fetchTarget) {
	source := plan.ExpectSource
	if source == "" {
		source = "unknown"
	}
	f := store.BlobFailure{
		ExpectedDigest: plan.ExpectSHA,
		ActualDigest:   actual,
		DigestSource:   source,
		Protocol:       plan.Kind,
		CacheKey:       logging.SanitizeKey(plan.CacheKey),
		ErrorCode:      "digest_mismatch",
	}
	if tgt != nil && tgt.c != nil {
		f.SourceID = tgt.c.ID
		f.SourceName = tgt.c.Name
	}
	if err := g.st.RecordBlobFailure(f); err != nil {
		log.Printf("记录完整性失败事件失败: %v", logging.SafeErr(err))
	}
}
