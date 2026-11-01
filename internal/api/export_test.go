package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// /api/export 至少返回 docker / github / containerd / k3s 四类配置片段，
// 且空源站时给出占位文案而不是 panic 或残缺 JSON。
func TestExportReturnsAllFormats(t *testing.T) {
	_, h := newTestServer(t)
	_, ck := login(t, h, "admin", "admin")
	// 默认密码态下面板接口被拦；先完成设置让面板可用。
	postJSON(h, "/api/setup", `{"user":"alex","pass":"correct-horse"}`, ck)
	// 设置后旧会话作废，用新账号重新登录拿有效会话。
	_, ck = login(t, h, "alex", "correct-horse")

	req := httptest.NewRequest(http.MethodGet, "/api/export", nil)
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("/api/export 应 200，得到 %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	for _, k := range []string{"docker", "github", "containerd", "k3s"} {
		sec, ok := out[k].(map[string]any)
		if !ok {
			t.Fatalf("缺少配置段 %q", k)
		}
		snip, _ := sec["snippet"].(string)
		if snip == "" {
			t.Fatalf("%q 片段为空", k)
		}
	}
	// containerd 片段应是合法 hosts.toml 骨架（无源站时为占位，有源站时为 server= 开头）
	c := out["containerd"].(map[string]any)["snippet"].(string)
	if !strings.Contains(c, "server = ") && !strings.Contains(c, "暂无可用源") {
		t.Fatalf("containerd 片段异常：%s", c)
	}
	// k3s 片段应是 registries.yaml 骨架
	k := out["k3s"].(map[string]any)["snippet"].(string)
	if !strings.Contains(k, "mirrors:") && !strings.Contains(k, "暂无可用源") {
		t.Fatalf("k3s 片段异常：%s", k)
	}
}
