package cache

import "testing"

func TestBlobPathRejectsBadHash(t *testing.T) {
	s := &Store{root: t.TempDir()}
	good := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if p := s.BlobPath(good); p == "" {
		t.Fatal("合法哈希不该被拒")
	}
	for _, bad := range []string{
		"",
		"../../etc/passwd",
		"0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF",
		"0123456789abcdef",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcde/",
		"../../../0123456789abcdef0123456789abcdef0123456789abcdef0123456",
	} {
		if p := s.BlobPath(bad); p != "" {
			t.Fatalf("%q 应被拒，实际 %q", bad, p)
		}
	}
}
