package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// adminRouteClasses are the administrator route classes (tenancy design "Admin surface").
// Every other route is a member route, whose auth model TestEveryRouteDeclaresItsAuthModel
// already requires.
var adminRouteClasses = map[string]string{
	"withPlatformAdmin": "platform",
	"withOrgAdmin":      "org-bound",
	"withRefusedAdmin":  "refused",
}

// routeClassErrors names every route in file with no class: an administrator route must
// name exactly one class wrapper, and a bare withAdmin names none. It also returns how
// many routes it read.
func routeClassErrors(file *ast.File) (errs []string, routes int) {
	forEachRoute(file, func(pattern string, handler ast.Expr) {
		routes++
		classes, bare := 0, false
		ast.Inspect(handler, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				if adminRouteClasses[id.Name] != "" {
					classes++
				}
				bare = bare || id.Name == "withAdmin"
			}
			return true
		})
		switch {
		case bare:
			errs = append(errs, pattern+": bare withAdmin; name its class (withPlatformAdmin, withOrgAdmin or withRefusedAdmin)")
		case classes > 1:
			errs = append(errs, pattern+": more than one admin route class")
		}
	})
	return errs, routes
}

// TestEveryAdminRouteHasAClass fails the build on an administrator route with no class.
func TestEveryAdminRouteHasAClass(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "server.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	errs, routes := routeClassErrors(file)
	for _, e := range errs {
		t.Error(e)
	}
	if routes < 100 {
		t.Fatalf("read only %d routes; the route table moved", routes)
	}
}

// The check itself fails a fixture route with no class, or with two.
func TestRouteClassCheckFailsAnUnclassifiedRoute(t *testing.T) {
	src := `package api
func (s *Server) routesFixture(mux *http.ServeMux) {
	mux.HandleFunc("GET /fixture/bare", s.withAdmin(s.handleFixture))
	mux.HandleFunc("GET /fixture/two", s.withOrgAdmin(s.withPlatformAdmin(s.handleFixture)))
	mux.HandleFunc("GET /fixture/platform", s.withPlatformAdmin(s.handleFixture))
	mux.HandleFunc("GET /fixture/member", s.withAuth(s.handleFixture))
}`
	file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	errs, routes := routeClassErrors(file)
	if routes != 4 || len(errs) != 2 || !strings.HasPrefix(errs[0], "GET /fixture/bare:") || !strings.HasPrefix(errs[1], "GET /fixture/two:") {
		t.Fatalf("routes %d, errors %q", routes, errs)
	}
}
