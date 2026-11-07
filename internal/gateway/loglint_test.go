package gateway

import (
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 这个是回归防线，不是完整审计：禁止网关业务代码把 plan.Upstream /
// plan.CacheKey 直接交给日志函数。允许先过 logging.SanitizeURL /
// SanitizeKey / SafeErr，其它路径一律报错。
var allowWrappers = map[string]bool{
	"SanitizeURL": true,
	"SanitizeKey": true,
	"SafeErr":     true,
}

func isLogCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	if x.Name == "log" {
		switch sel.Sel.Name {
		case "Printf", "Println", "Print", "Fatal", "Fatalf":
			return true
		}
	}
	if x.Name == "slog" {
		switch sel.Sel.Name {
		case "Info", "Warn", "Error", "Debug":
			return true
		}
	}
	return false
}

// unsafeArg 判断一个实参里是否直接出现敏感字段；遇到白名单包装就整棵跳过。
func unsafeArg(e ast.Expr) bool {
	if call, ok := e.(*ast.CallExpr); ok {
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			if x, ok := fn.X.(*ast.Ident); ok && x.Name == "logging" && allowWrappers[fn.Sel.Name] {
				return false
			}
		case *ast.Ident:
			// safeUpstream 内部已经过 SanitizeURL。
			if fn.Name == "safeUpstream" {
				return false
			}
		}
	}
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if found {
			return false
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		x, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if x.Name == "plan" && (sel.Sel.Name == "Upstream" || sel.Sel.Name == "CacheKey") {
			found = true
			return false
		}
		return true
	})
	return found
}

func scanLogLeaks(fset *gotoken.FileSet, files []*ast.File) []string {
	var bad []string
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isLogCall(call) {
				return true
			}
			for _, a := range call.Args {
				if unsafeArg(a) {
					p := fset.Position(call.Pos())
					bad = append(bad, p.String())
					break
				}
			}
			return true
		})
	}
	return bad
}

func TestGatewayLogsDoNotLeakURLs(t *testing.T) {
	fset := gotoken.NewFileSet()
	dir := "."
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("%s 解析失败: %v", name, err)
		}
		files = append(files, f)
	}
	if bad := scanLogLeaks(fset, files); len(bad) > 0 {
		t.Fatalf("日志直接打印了 plan.Upstream/plan.CacheKey，改用 logging.SanitizeURL/SanitizeKey：\n%s",
			strings.Join(bad, "\n"))
	}
}

// 自检：确保检测器真的能抓到违规，而不是因为没匹配上而永远通过。
func TestLogLeakScannerCatchesDirectArg(t *testing.T) {
	src := `package gateway
import (
	"log"
	"example/logging"
)
func f(plan struct{ Upstream, CacheKey string }) {
	log.Printf("回源失败 %s", plan.Upstream)
	log.Printf("缓存 %s", plan.CacheKey)
	log.Printf("已脱敏 %s", logging.SanitizeURL(plan.Upstream.String()))
}
func sanitize(s string) string { return s }
`
	fset := gotoken.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	bad := scanLogLeaks(fset, []*ast.File{f})
	if len(bad) != 2 {
		t.Fatalf("应抓到 2 处违规，实际 %d：%v", len(bad), bad)
	}
}
