package jobqueue

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// The queue works on any *sql.DB it is given. It must never name the SQLite
// driver (only sqlstore does) and never depend on the task store, so it can
// be tested, replaced or reused without either.
func TestQueueDoesNotImportTheDriverOrTheTaskStore(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.Contains(path, "go-sqlite3") || strings.HasPrefix(path, "offgrid/internal/store") || strings.HasPrefix(path, "offgrid/internal/task") {
				t.Errorf("%s imports %s", f, path)
			}
		}
	}
}
