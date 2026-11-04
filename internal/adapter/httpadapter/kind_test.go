package httpadapter

import (
	"net/url"
	"testing"
)

// archive 是 github_cap 池的活，归成 http 就只剩直连 github.com，国内必挂。
func TestGithubKindRoutesArchiveToPool(t *testing.T) {
	cases := map[string]string{
		"https://github.com/SagerNet/sing-box-dashboard/archive/refs/heads/gh-pages.zip": "release",
		"https://github.com/o/r/archive/refs/tags/v1.0.0.tar.gz":                        "release",
		"https://github.com/o/r/releases/download/v1/a.tar.gz":                          "release",
		"https://github.com/o/r/releases/latest/download/a.tar.gz":                      "release",
		"https://codeload.github.com/o/r/zip/refs/heads/main":                           "release",
		"https://raw.githubusercontent.com/o/r/main/f.go":                                "raw",
		"https://github.com/o/r":                                                        "http",
		"https://example.com/a.zip":                                                     "http",
	}
	for raw, want := range cases {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := githubKind(u); got != want {
			t.Fatalf("%s → %q，期望 %q", raw, got, want)
		}
	}
}
