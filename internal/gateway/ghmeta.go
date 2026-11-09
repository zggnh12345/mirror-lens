package gateway

import (
	"context"
	"strings"
	"time"

	"github.com/zggnh12345/mirror-lens/internal/adapter"
	"github.com/zggnh12345/mirror-lens/internal/ghapi"
	"github.com/zggnh12345/mirror-lens/internal/metrics"
	"github.com/zggnh12345/mirror-lens/internal/store"
)

// enrichReleaseDigest 在回源前补一个可信摘要（GitHub Release Asset API）。
//
// 硬约束：
//   - 未配置 Token：这里什么都不做，连一次 API 请求都不发。
//   - 超时/限流/结构异常：一律降级为「无摘要」，绝不阻塞下载。
//   - 只接受 sha256；其他算法只记指标，不设 ExpectSHA。
func (g *Gateway) enrichReleaseDigest(ctx context.Context, plan *adapter.Plan) {
	if g.ghMeta == nil || !g.ghMeta.Enabled() {
		return
	}
	if plan.Kind != "release" || plan.ExpectSHA != "" {
		return
	}
	owner, repo, tag, file, ok := parseReleaseKey(plan.CacheKey)
	if !ok {
		return
	}
	// 查询最多等 1.5s：元数据是加分项，不能把下载拖成慢请求。
	cctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	d := g.ghMeta.AssetDigest(cctx, owner, repo, tag, file)
	metrics.GHMetaResult(d.Result)
	if d.Result == ghapi.ResultOK {
		plan.ExpectSHA = d.Hex
		switch d.Source {
		case ghapi.SourceSUMS:
			plan.ExpectSource = adapter.DigestSourceSUMS
		default:
			plan.ExpectSource = adapter.DigestSourceGitHub
		}
	}
}

// parseReleaseKey 从 "gh:owner/repo/releases/download/tag/file" 拆出查询所需字段。
// latest 形式的 tag 返回空串（走 /releases/latest）。
func parseReleaseKey(key string) (owner, repo, tag, file string, ok bool) {
	if !strings.HasPrefix(key, "gh:") {
		return "", "", "", "", false
	}
	segs := strings.Split(strings.TrimPrefix(key, "gh:"), "/")
	if len(segs) < 6 || segs[2] != "releases" {
		return "", "", "", "", false
	}
	owner, repo = segs[0], segs[1]
	switch segs[3] {
	case "download":
		tag = segs[4]
	case "latest":
		tag = ""
	default:
		return "", "", "", "", false
	}
	file = segs[len(segs)-1]
	if owner == "" || repo == "" || file == "" {
		return "", "", "", "", false
	}
	return owner, repo, tag, file, true
}

// recordBlobProvenance 在内容落盘后补记摘要来源，供面板区分「可信」与「仅完整」。
func (g *Gateway) recordBlobProvenance(sha string, plan *adapter.Plan) {
	if sha == "" || plan.ExpectSHA == "" || plan.ExpectSource == "" {
		return
	}
	_ = g.st.BlobUpsert(store.Blob{
		SHA256: sha, IntegrityStatus: store.BlobVerified,
		ExpectedDigest: plan.ExpectSHA, ActualDigest: sha,
		DigestSource: plan.ExpectSource, DigestAlgorithm: "sha256",
		VerifiedAt: time.Now().Unix(),
	})
}
