package registryadapter

import (
	"strings"
	"testing"
)

const (
	typeDockerList  = "application/vnd.docker.distribution.manifest.list.v2+json"
	typeDockerV2    = "application/vnd.docker.distribution.manifest.v2+json"
	typeDockerV1    = "application/vnd.docker.distribution.manifest.v1+prettyjws"
	typeOCIIndex    = "application/vnd.oci.image.index.v1+json"
	typeOCIManifest = "application/vnd.oci.image.manifest.v1+json"
)

func acceptSet(s string) map[string]int {
	out := map[string]int{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out[p]++
		}
	}
	return out
}

// 本次故障的回归断言：客户端没带 Accept（docker 的 HEAD 探测就是如此）时，
// 回源 Accept 必须包含 docker manifest list 类型，否则 registry 3.0 对
// 多架构 tag 直接 400（MANIFEST_INVALID），docker 侧表现为 502。
func TestUpstreamAcceptEmptyClientKeepsDockerTypes(t *testing.T) {
	cases := []struct {
		name    string
		client  string
		foreign bool
		must    []string
	}{
		{"缺失", "", false, []string{typeDockerList, typeDockerV2, typeOCIIndex, typeOCIManifest}},
		{"缺失-foreign", "", true, []string{typeDockerList, typeDockerV2, typeOCIIndex, typeOCIManifest}},
		{"空串", " ", true, []string{typeDockerList, typeDockerV2, typeOCIIndex, typeOCIManifest}},
		{"只有无效类型", "*/*", true, []string{typeDockerList, typeDockerV2}},
		{"只有 schema1", typeDockerV1, true, []string{typeDockerList, typeDockerV2}},
		{"只给 docker list", typeDockerList, false, []string{typeDockerList}},
		{"只给 OCI index", typeOCIIndex, false, []string{typeOCIIndex}},
		{"docker+OCI", typeDockerList + ", " + typeOCIManifest, false, []string{typeDockerList, typeOCIManifest}},
	}
	for _, c := range cases {
		got := acceptSet(UpstreamAccept(c.client, c.foreign))
		for _, want := range c.must {
			if got[want] == 0 {
				t.Errorf("%s: UpstreamAccept(%q, foreign=%v) 缺 %s", c.name, c.client, c.foreign, want)
			}
		}
		for typ, n := range got {
			if n > 1 {
				t.Errorf("%s: %s 重复 %d 次", c.name, typ, n)
			}
		}
	}
}

// 客户端明确给了类型时只能收窄，不能擅自扩大：否则会改变协商语义。
func TestUpstreamAcceptKeepsClientNarrowing(t *testing.T) {
	if got := UpstreamAccept(typeDockerV2, false); got != typeDockerV2 {
		t.Errorf("Hub 且客户端明确指定时不应改写: %q", got)
	}
	// foreign 站原有行为：补 OCI 两种，且去重。
	got := acceptSet(UpstreamAccept(typeOCIIndex, true))
	if len(got) != 2 || got[typeOCIIndex] != 1 || got[typeOCIManifest] != 1 {
		t.Errorf("foreign 站应只补 OCI manifest: %v", got)
	}
	// GHCR（foreign=true）客户端只给 docker list 时：保留原类型 + 补 OCI。
	got = acceptSet(UpstreamAccept(typeDockerList, true))
	if got[typeDockerList] != 1 || got[typeOCIIndex] != 1 || got[typeOCIManifest] != 1 {
		t.Errorf("GHCR 行为变化: %v", got)
	}
	// Hub（foreign=false）空 Accept 时仍必须能取到 docker 清单。
	got = acceptSet(UpstreamAccept("", false))
	if got[typeDockerList] == 0 || got[typeDockerV2] == 0 {
		t.Errorf("Hub 空 Accept 兜底不完整: %v", got)
	}
}
