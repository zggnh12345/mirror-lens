package metrics

import (
	"strings"
	"testing"
)

func TestSourceBytesAccumulatesPerKind(t *testing.T) {
	Enable()
	defer Disable()

	SourceBytes(3, "docker", 100)
	SourceBytes(3, "docker", 50)
	SourceBytes(3, "raw", 10)
	SourceBytes(3, "docker", 0)  // 0 不算
	SourceBytes(0, "docker", 99) // 无 id 不算

	out := string(Get().render())
	if !strings.Contains(out, `source_bytes_total{source_id="3",kind="docker"} 150`) {
		t.Fatalf("docker 字节数不对:\n%s", onlyLines(out, "source_bytes_total"))
	}
	if !strings.Contains(out, `source_bytes_total{source_id="3",kind="raw"} 10`) {
		t.Fatalf("raw 字节数不对:\n%s", onlyLines(out, "source_bytes_total"))
	}
}

func TestSourceBytesDroppedAfterForget(t *testing.T) {
	Enable()
	defer Disable()

	SourceBytes(4, "raw", 10)
	ForgetSource(4)
	SourceBytes(4, "raw", 10) // 迟到回调，不能复活序列

	if out := string(Get().render()); strings.Contains(out, `source_id="4"`) {
		t.Fatalf("删除后仍有序列:\n%s", onlyLines(out, `source_id="4"`))
	}
}

func TestSourceCandidatesHistogram(t *testing.T) {
	Enable()
	defer Disable()

	SourceCandidates(0)
	SourceCandidates(3)
	SourceCandidates(30)

	out := string(Get().render())
	if !strings.Contains(out, "source_candidate_count_count 3") {
		t.Fatalf("样本数不对:\n%s", onlyLines(out, "source_candidate_count"))
	}
	if !strings.Contains(out, `source_candidate_count_bucket{le="1"} 1`) {
		t.Fatalf("0 应落进 le=1:\n%s", onlyLines(out, "source_candidate_count"))
	}
	if !strings.Contains(out, `source_candidate_count_bucket{le="+Inf"} 3`) {
		t.Fatalf("+Inf 应等于总数:\n%s", onlyLines(out, "source_candidate_count"))
	}
}

func onlyLines(s, sub string) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "\n")
}
