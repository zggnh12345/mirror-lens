package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/zggnh12345/mirror-lens/internal/gateway"
)

// 18f：GET /api/cross-runs 只读、需登录、有界、字段不含凭据。
func TestCrossRunsAPI(t *testing.T) {
	st, h := newTestServer(t)
	_ = st

	// 匿名读被拒
	if rec := get(h, "/api/cross-runs", nil, nil); rec.Code != 401 && rec.Code != 403 {
		t.Fatalf("匿名读应被拒，得到 %d", rec.Code)
	}

	ck := setupPanel(t, h)
	rec := get(h, "/api/cross-runs", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("读取失败 %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Items []gateway.CrossRunState `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析: %v", err)
	}
	// 尚无跨站调度时返回空数组（非 null）
	if out.Items == nil || len(out.Items) != 0 {
		t.Fatalf("初始应为空数组，实际 %+v", out.Items)
	}

	// 非 GET 方法被拒
	if rec := putJSON(h, "/api/cross-runs", `{}`, ck); rec.Code != 405 {
		t.Fatalf("PUT 应 405，实际 %d", rec.Code)
	}
}

// 18f：limit 上限收敛到环形缓冲容量，防止无限拉取撑高基数。
func TestCrossRunsLimitBounded(t *testing.T) {
	_, h := newTestServer(t)
	ck := setupPanel(t, h)
	// 大 limit 不应报错，服务端收敛到容量上限
	rec := get(h, "/api/cross-runs?limit=99999", nil, ck)
	if rec.Code != 200 {
		t.Fatalf("大 limit 读取失败 %d %s", rec.Code, rec.Body.String())
	}
}
