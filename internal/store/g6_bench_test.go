package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// G6 针对性基准：AddSourceHost 单事务吞吐（3.5.25 在事务内读 base_url）。
// 同一 DB 内循环，排除建库开销；每轮新增独立 source 避免冲突路径。
func BenchmarkAddSourceHostG6(b *testing.B) {
	st, err := Open(filepath.Join(b.TempDir(), "g6.db"))
	if err != nil { b.Fatal(err) }
	defer st.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id, err := st.AddSource(Source{Name: fmt.Sprintf("g6-%d-%d", time.Now().UnixNano(), i), BaseURL: fmt.Sprintf("https://g6-%d.example", i), Status: "usable", Enabled: true})
		if err != nil { b.Fatal(err) }
		if err := st.AddSourceHost(id, "quay.io", "https://quay.io", nil); err != nil { b.Fatal(err) }
	}
}
