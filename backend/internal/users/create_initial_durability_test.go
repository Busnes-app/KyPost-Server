package users

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"testing"
)

// TestCreateInitialIsCrashDurable pins the fsync pair in createInitial.
//
// A static check, for the same reason TestLockOrderIsRespected in package api
// is one: the bug is a power cut between two syscalls, and no Go test can
// provoke that. What a test CAN do is assert that the calls are there and in
// the right order, because that is the whole of the property — an fsync that
// runs after the link does not protect the link, and one that never runs
// protects nothing.
//
// What it guards: users.json holds the ONLY copy of the bootstrap admin's ID,
// and everything else written to the volume on first boot is keyed to that ID.
// Losing the file to an unflushed writeback re-bootstraps a DIFFERENT admin on
// the next start and orphans all of it. createInitial handled exclusive
// creation and not durable creation; deleting either call below silently
// restores that.
func TestCreateInitialIsCrashDurable(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "users.go", nil, 0)
	if err != nil {
		t.Fatalf("parse users.go: %v", err)
	}

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "createInitial" {
			body = fn.Body
		}
	}
	if body == nil {
		// Renamed or moved: this test names the function it protects, so make
		// that a failure rather than a silent pass over nothing.
		t.Fatal("createInitial not found in users.go; if it moved, move this test with it")
	}

	type call struct {
		name string
		pos  token.Pos
	}
	var calls []call
	ast.Inspect(body, func(n ast.Node) bool {
		expr, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := expr.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		calls = append(calls, call{name: recv.Name + "." + sel.Sel.Name, pos: expr.Pos()})
		return true
	})
	sort.Slice(calls, func(i, j int) bool { return calls[i].pos < calls[j].pos })

	posOf := func(name string) int {
		for i, c := range calls {
			if c.name == name {
				return i
			}
		}
		return -1
	}

	// The fsync pair and the exclusive link live in fsutil.CreateExclusive, which
	// TestCreateExclusiveIsCrashDurable pins; this pins that createInitial uses it.
	if posOf("fsutil.CreateExclusive") < 0 {
		t.Fatal("createInitial no longer creates users.json through fsutil.CreateExclusive")
	}
}
