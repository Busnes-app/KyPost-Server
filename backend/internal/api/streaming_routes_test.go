package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// streamingFuncs maps every function that needs the real connection to a path it serves.
// withDeadline buffers all other routes, and the buffer cannot flush, hijack or extend the
// write deadline: such a route would send nothing, then a 503 at the deadline.
var streamingFuncs = map[string]string{
	"detachedBackup":       "/api/admin/backup/run",
	"handleExportDownload": "/api/export/t",
	"receiveImport":        "/api/import/t",
}

// TestStreamingRoutesAreLongRoutes fails when a function in this package hijacks, flushes
// the response, sends server-sent events or extends its write deadline without being listed
// above, or when its path is not a long route.
func TestStreamingRoutesAreLongRoutes(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if ok && fn.Body != nil && streams(fn.Body) {
				found[fn.Name.Name] = true
			}
		}
	}
	var names []string
	for n := range found {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p, ok := streamingFuncs[n]
		if !ok {
			t.Errorf("%s needs the real connection: add its route to longRoutes and streamingFuncs", n)
			continue
		}
		if _, long := longRouteFor(p); !long {
			t.Errorf("%s serves %s, which is not a long route", n, p)
		}
	}
	for n := range streamingFuncs {
		if !found[n] {
			t.Errorf("streamingFuncs lists %s, which no longer streams; remove it", n)
		}
	}
}

func streams(body *ast.BlockStmt) bool {
	// Variables holding a ResponseController: their Flush reaches the connection. A bufio
	// or csv writer's Flush does not.
	rcs := map[string]bool{}
	isRC := func(e ast.Expr) bool {
		if id, ok := e.(*ast.Ident); ok {
			return rcs[id.Name]
		}
		c, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		s, ok := c.Fun.(*ast.SelectorExpr)
		return ok && s.Sel.Name == "NewResponseController"
	}
	hit := false
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, r := range x.Rhs {
				if i < len(x.Lhs) && isRC(r) {
					if id, ok := x.Lhs[i].(*ast.Ident); ok {
						rcs[id.Name] = true
					}
				}
			}
		case *ast.SelectorExpr:
			switch x.Sel.Name {
			case "Hijack", "Hijacker", "Flusher", "SetWriteDeadline", "EnableFullDuplex":
				hit = true
			case "Flush":
				hit = hit || isRC(x.X)
			}
		case *ast.BasicLit:
			if strings.Contains(x.Value, "text/event-stream") {
				hit = true
			}
		}
		return !hit
	})
	return hit
}
