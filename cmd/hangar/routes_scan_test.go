package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// routeSites are the packages whose handlers record a route template with
// httpx.SetRoute or register routes on a mux, relative to the module root.
var routeSites = []string{
	"internal/hfapi", "internal/gitserver", "internal/ollama", "internal/xet", "internal/datasets/viewer",
	"internal/auth", "internal/web", "internal/httpx", "internal/api", "internal/upstream", "cmd/hangar",
}

// routeCallNames are the calls that declare a route: httpx.SetRoute, the
// route helpers of the Hub API, Xet and auth handlers, and ServeMux.Handle
// and HandleFunc.
var routeCallNames = map[string]bool{"SetRoute": true, "route": true, "HandleFunc": true, "Handle": true}

// scanRouteSites returns a fingerprint of every route declaration in
// routeSites: the file and the call, with function literals elided. A new
// route, or a new method on an existing one, changes the set.
func scanRouteSites(t *testing.T) []string {
	t.Helper()
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var out []string
	for _, dir := range routeSites {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		require.NoError(t, err)
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			rel := dir + "/" + name
			f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
			require.NoError(t, err)
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var fn string
				switch x := call.Fun.(type) {
				case *ast.SelectorExpr:
					fn = x.Sel.Name
				case *ast.Ident:
					fn = x.Name
				}
				if !routeCallNames[fn] {
					return true
				}
				out = append(out, rel+": "+fingerprint(fset, call))
				return true
			})
		}
	}
	sort.Strings(out)
	return out
}

// fingerprint prints a call with every function literal replaced by `fn`
// and whitespace collapsed.
func fingerprint(fset *token.FileSet, call *ast.CallExpr) string {
	c := *call
	c.Args = make([]ast.Expr, len(call.Args))
	for i, a := range call.Args {
		c.Args[i] = elideFuncs(a)
	}
	var b bytes.Buffer
	_ = printer.Fprint(&b, fset, &c)
	return strings.Join(strings.Fields(b.String()), " ")
}

func elideFuncs(e ast.Expr) ast.Expr {
	switch x := e.(type) {
	case *ast.FuncLit:
		return ast.NewIdent("fn")
	case *ast.CompositeLit:
		cp := *x
		cp.Elts = make([]ast.Expr, len(x.Elts))
		for i, el := range x.Elts {
			cp.Elts[i] = elideFuncs(el)
		}
		return &cp
	case *ast.KeyValueExpr:
		cp := *x
		cp.Value = elideFuncs(x.Value)
		return &cp
	}
	return e
}

// scanMapKeys returns the string keys of the map literal assigned to the
// variable name in file, relative to the module root.
func scanMapKeys(t *testing.T, file, name string) []string {
	t.Helper()
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, file), nil, 0)
	require.NoError(t, err)
	var keys []string
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); !ok || id.Name != name {
			return true
		}
		lit, ok := as.Rhs[0].(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, el := range lit.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if bl, ok := kv.Key.(*ast.BasicLit); ok && bl.Kind == token.STRING {
					keys = append(keys, strings.Trim(bl.Value, `"`))
				}
			}
		}
		return false
	})
	require.NotEmpty(t, keys, "no map literal %s in %s", name, file)
	return keys
}
