package runner

import "testing"

func TestResolveProbeRepo(t *testing.T) {
	r := NewHostProbeRunner(HostProbeOptions{Repos: map[string]string{
		"quay.io": "prometheus/busybox:latest",
		"*.pkg.dev": "google-samples/containers/gke/hello-app:1.0",
	}})
	for _, tc := range []struct{ host, want string; ok bool }{
		{"quay.io", "prometheus/busybox:latest", true},
		{"QUAY.IO.", "prometheus/busybox:latest", true},
		{"us-docker.pkg.dev", "google-samples/containers/gke/hello-app:1.0", true},
		{"gcr.io", "", false},
	} {
		got, ok := r.resolveProbeRepo(tc.host)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("resolveProbeRepo(%q)=(%q,%v) 期望 (%q,%v)", tc.host, got, ok, tc.want, tc.ok)
		}
	}
}
