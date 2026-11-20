package version

import (
	"regexp"
	"strings"
	"testing"
)

func TestVersionIsSemver(t *testing.T) {
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(Version) {
		t.Fatalf("Version 必须是 X.Y.Z 形式，实得 %q", Version)
	}
}

func TestBuildInfoDefaults(t *testing.T) {
	if Commit == "" {
		t.Fatal("Commit 默认值不得为空串（未注入时应为 unknown）")
	}
	if BuildDate == "" {
		t.Fatal("BuildDate 默认值不得为空串（未注入时应为 unknown）")
	}
}

func TestInfoContainsAllFields(t *testing.T) {
	got := Info()
	for _, want := range []string{"mirror-lens " + Version, "commit=" + Commit, "built=" + BuildDate} {
		if !strings.Contains(got, want) {
			t.Fatalf("Info()=%q 缺少 %q", got, want)
		}
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("Info() 不应出现连续空格: %q", got)
	}
}
