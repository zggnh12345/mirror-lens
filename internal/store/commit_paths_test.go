package store

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRegistryProbeWritePathsConverged 锁死「Registry 探测结果只有一条写路径」。
// SetHostCaps / NoteProbeScoped 只允许留在定义处，生产代码不得再调用它们；
// 测试辅助文件（*_test.go）不受限。
func TestRegistryProbeWritePathsConverged(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "docs":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(b)
		rel, _ := filepath.Rel(root, path)
		if rel != filepath.Join("internal", "store", "sources.go") &&
			strings.Contains(src, "NoteProbeScoped(") {
			offenders = append(offenders, rel+" 仍调用 NoteProbeScoped")
		}
		if rel != filepath.Join("internal", "store", "host.go") &&
			strings.Contains(src, "SetHostCaps(") {
			offenders = append(offenders, rel+" 仍调用 SetHostCaps")
		}
		// 待探测任务只能有一个来源：ListDueHostProbes。旧查询留作只读诊断，
		// 任何生产代码再调用它就等于长出第二套调度输入。
		if rel != filepath.Join("internal", "store", "host.go") &&
			strings.Contains(src, "ListPendingHostCaps(") {
			offenders = append(offenders, rel+" 仍调用 ListPendingHostCaps")
		}
		if rel == filepath.Join("internal", "runner", "hostprobe.go") &&
			!strings.Contains(src, "CommitHostProbe(") {
			offenders = append(offenders, rel+" 未走 CommitHostProbe")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("Registry 写路径未收敛:\n%s", strings.Join(offenders, "\n"))
	}
}

// TestTransportReasonConstantMatchesProbe 锁死 store 侧字符串常量与探测层一致。
func TestTransportReasonConstantMatchesProbe(t *testing.T) {
	if probeReasonTransportError != "transport_error" {
		t.Fatalf("probeReasonTransportError=%q", probeReasonTransportError)
	}
}
