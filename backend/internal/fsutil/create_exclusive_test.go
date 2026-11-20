package fsutil

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestCreateExclusiveIsCrashDurable pins the order a power cut cannot be tested for: the
// temp file is synced (in writeTemp) before it is linked into place, and the directory is
// synced after the link.
func TestCreateExclusiveIsCrashDurable(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "fsutil.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string][]string{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || (fn.Name.Name != "CreateExclusive" && fn.Name.Name != "writeTemp") {
			continue
		}
		type call struct {
			name string
			pos  token.Pos
		}
		var cs []call
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if e, ok := n.(*ast.CallExpr); ok {
				switch f := e.Fun.(type) {
				case *ast.Ident:
					cs = append(cs, call{f.Name, e.Pos()})
				case *ast.SelectorExpr:
					if x, ok := f.X.(*ast.Ident); ok {
						cs = append(cs, call{x.Name + "." + f.Sel.Name, e.Pos()})
					}
				}
			}
			return true
		})
		sort.Slice(cs, func(i, j int) bool { return cs[i].pos < cs[j].pos })
		for _, c := range cs {
			calls[fn.Name.Name] = append(calls[fn.Name.Name], c.name)
		}
	}
	index := func(fn, name string) int {
		for i, c := range calls[fn] {
			if c == name {
				return i
			}
		}
		return -1
	}
	if index("writeTemp", "Sync") < 0 {
		t.Error("writeTemp does not fsync the temp file")
	}
	temp, link, dir := index("CreateExclusive", "writeTemp"), index("CreateExclusive", "os.Link"), index("CreateExclusive", "SyncDir")
	if temp < 0 || link < 0 || dir < 0 || temp > link || dir < link {
		t.Errorf("CreateExclusive must write the synced temp file, link it, then sync the directory: %v", calls["CreateExclusive"])
	}
}

func TestCreateExclusiveKeepsTheFirstFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "f")
	if created, err := CreateExclusive(path, []byte("first"), 0o600); !created || err != nil {
		t.Fatal(created, err)
	}
	if created, err := CreateExclusive(path, []byte("second"), 0o600); created || err != nil {
		t.Fatal(created, err)
	}
	b, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	entries, _ := os.ReadDir(filepath.Dir(path))
	if string(b) != "first" || info.Mode().Perm() != 0o600 || len(entries) != 1 {
		t.Fatalf("content %q mode %v entries %d", b, info.Mode().Perm(), len(entries))
	}
}
