package metrics

import (
	"strings"
	"testing"
	"time"
)

// 中性结果（404/410、401/403）不进 429 率分母，但请求计数照记，且单独成 neutral 桶。
func TestNeutralResultNotIn429Denominator(t *testing.T) {
	setup(t)

	SourceResult("registry:quay.io", SourceOK, 200, "single", 10*time.Millisecond)
	SourceResult("registry:quay.io", SourceFail, 429, "single", 10*time.Millisecond)
	SourceResult("registry:quay.io", SourceNeutral, 404, "single", 5*time.Millisecond)
	SourceResult("registry:quay.io", SourceNeutral, 410, "single", 5*time.Millisecond)
	SourceResult("registry:quay.io", SourceNeutral, 401, "single", 5*time.Millisecond)
	SourceResult("registry:quay.io", SourceNeutral, 403, "single", 5*time.Millisecond)

	out := string(Get().render())

	if !strings.Contains(out, `source_http_responses_total{kind="registry:quay.io"} 2`) {
		t.Fatalf("429 率分母应只含 200 与 429 两次，期望 2：\n%s", out)
	}
	if !strings.Contains(out, `source_429_total{kind="registry:quay.io"} 1`) {
		t.Fatalf("429 计数应为 1：\n%s", out)
	}
	if !strings.Contains(out, `source_requests_total{kind="registry:quay.io",result="neutral"} 4`) {
		t.Fatalf("中性结果应单独计数为 4：\n%s", out)
	}
	if !strings.Contains(out, `source_requests_total{kind="registry:quay.io",result="ok"} 1`) {
		t.Fatalf("成功计数应为 1：\n%s", out)
	}
	if !strings.Contains(out, `source_requests_total{kind="registry:quay.io",result="fail"} 1`) {
		t.Fatalf("失败计数应为 1：\n%s", out)
	}
}

// 各协议族的分母与计数互不影响。
func TestNeutralStatsIsolatedByKind(t *testing.T) {
	setup(t)

	SourceResult("docker", SourceOK, 200, "single", time.Millisecond)
	SourceResult("registry:quay.io", SourceNeutral, 404, "single", time.Millisecond)
	SourceResult("registry:quay.io", SourceNeutral, 401, "single", time.Millisecond)

	out := string(Get().render())
	if !strings.Contains(out, `source_http_responses_total{kind="docker"} 1`) {
		t.Fatalf("docker 分母不该被 registry 的中性结果影响：\n%s", out)
	}
	if strings.Contains(out, `source_http_responses_total{kind="registry:quay.io"}`) {
		t.Fatalf("registry 全是中性结果时不该有分母样本：\n%s", out)
	}
}
