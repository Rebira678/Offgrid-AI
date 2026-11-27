package governor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// Importing net/http/pprof registers its handlers on http.DefaultServeMux. That is harmless only while
// nothing in the product serves the default mux. This test reads every non-test source file and fails
// if something could: an http.Server literal with no Handler, a package-level ListenAndServe with a nil
// handler, or any mention of DefaultServeMux in code. A future "quick" server would otherwise publish
// goroutine stacks and heap contents to the network without anyone having asked for a profiler.
func TestNothingInTheProductServesTheDefaultMux(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var bad []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "vendor" || d.Name() == "bench" || d.Name() == "scratch" || d.Name() == "my_perispective_not_tracked_by_git") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0) // comments are skipped on purpose
		if err != nil {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok && id.Name == "http" && x.Sel.Name == "DefaultServeMux" {
					bad = append(bad, fset.Position(x.Pos()).String()+": http.DefaultServeMux")
				}
			case *ast.CompositeLit:
				if sel, ok := x.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Server" {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "http" {
						has := false
						for _, e := range x.Elts {
							if kv, ok := e.(*ast.KeyValueExpr); ok {
								if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Handler" {
									has = true
								}
							}
						}
						if !has {
							bad = append(bad, fset.Position(x.Pos()).String()+": http.Server without a Handler serves the default mux")
						}
					}
				}
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "http" && (sel.Sel.Name == "ListenAndServe" || sel.Sel.Name == "ListenAndServeTLS" || sel.Sel.Name == "Serve") {
						if last, ok := x.Args[len(x.Args)-1].(*ast.Ident); ok && last.Name == "nil" {
							bad = append(bad, fset.Position(x.Pos()).String()+": "+sel.Sel.Name+" with a nil handler serves the default mux")
						}
					}
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "http" && (sel.Sel.Name == "Handle" || sel.Sel.Name == "HandleFunc") {
						bad = append(bad, fset.Position(x.Pos()).String()+": http."+sel.Sel.Name+" registers on the default mux")
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Fatalf("something may serve the default mux, where net/http/pprof registers itself:\n  %s", strings.Join(bad, "\n  "))
	}
}
