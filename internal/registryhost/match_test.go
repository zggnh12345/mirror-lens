package registryhost

import "testing"

func TestMatchExactAndArtifactRegistry(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		host     string
		want     bool
	}{
		{"exact", []string{"gcr.io"}, "gcr.io", true},
		{"exact-miss", []string{"gcr.io"}, "quay.io", false},
		{"case-and-space", []string{" GCR.IO "}, "gcr.io", true},
		{"artifact-wildcard", []string{"*.pkg.dev"}, "asia-east1-docker.pkg.dev", true},
		{"artifact-wildcard-us", []string{"*.pkg.dev"}, "us-docker.pkg.dev", true},
		{"not-artifact-shape", []string{"*.pkg.dev"}, "evil.pkg.dev", false},
		{"artifact-two-segments", []string{"*.pkg.dev"}, "pkg.dev", false},
		{"empty-patterns", nil, "gcr.io", false},
		{"blank-pattern", []string{" "}, "gcr.io", false},
		{"exact-and-wildcard", []string{"quay.io", "*.pkg.dev"}, "europe-west1-docker.pkg.dev", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Match(c.patterns, c.host); got != c.want {
				t.Fatalf("Match(%v, %q)=%v want %v", c.patterns, c.host, got, c.want)
			}
		})
	}
}
