package modelusage

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestStreamHasOneExit reads the package source and checks the shape that keeps every ParseStream result in
// one place:
//
//   - ParseStream has two return statements: the refusal of invalid limits, before any byte is read, and
//     return p.result();
//   - no method of streamParser except result returns a Usage or an error, so reading, line splitting, event
//     assembly and the handlers of each family can only update the state or report a problem;
//   - result is the only code that builds a *PartialError or a *TrailingError.
func TestStreamHasOneExit(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	foundParseStream, foundResult := false, false
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			recv := receiverType(fn)
			if recv == "" && name == "ParseStream" {
				foundParseStream = true
				checkParseStream(t, fset, fn)
			}
			if recv == "streamParser" {
				if name == "result" {
					foundResult = true
				} else if returnsUsageOrError(fn) {
					t.Errorf("%s: streamParser.%s returns a Usage or an error; only result may",
						fset.Position(fn.Pos()), name)
				}
			}
			if recv == "streamParser" && name == "result" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if lit, ok := n.(*ast.CompositeLit); ok {
					if id, ok := lit.Type.(*ast.Ident); ok && (id.Name == "PartialError" || id.Name == "TrailingError") {
						t.Errorf("%s: %s builds a %s; only streamParser.result may", fset.Position(lit.Pos()), name,
							id.Name)
					}
				}
				if call, ok := n.(*ast.CallExpr); ok {
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "new" && len(call.Args) == 1 {
						if arg, ok := call.Args[0].(*ast.Ident); ok &&
							(arg.Name == "PartialError" || arg.Name == "TrailingError") {
							t.Errorf("%s: %s allocates a %s; only streamParser.result may",
								fset.Position(call.Pos()), name, arg.Name)
						}
					}
				}
				return true
			})
		}
	}
	if !foundParseStream || !foundResult {
		t.Fatalf("ParseStream found %v, streamParser.result found %v", foundParseStream, foundResult)
	}
}

func receiverType(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func returnsUsageOrError(fn *ast.FuncDecl) bool {
	if fn.Type.Results == nil {
		return false
	}
	for _, field := range fn.Type.Results.List {
		if id, ok := field.Type.(*ast.Ident); ok && (id.Name == "Usage" || id.Name == "error") {
			return true
		}
	}
	return false
}

// checkParseStream checks that ParseStream returns only on invalid limits and through p.result().
func checkParseStream(t *testing.T, fset *token.FileSet, fn *ast.FuncDecl) {
	t.Helper()
	var returns []*ast.ReturnStmt
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		if r, ok := n.(*ast.ReturnStmt); ok {
			returns = append(returns, r)
		}
		return true
	})
	if len(returns) != 2 {
		t.Fatalf("%s: ParseStream has %d return statements, want 2", fset.Position(fn.Pos()), len(returns))
	}
	// The first is inside the if statement that checks the limits, the first statement of the body.
	first, ok := fn.Body.List[0].(*ast.IfStmt)
	if !ok || !contains(first, returns[0]) || !strings.Contains(exprString(first.Init), "lim.check") {
		t.Errorf("%s: the first return of ParseStream is not the refusal of invalid limits",
			fset.Position(returns[0].Pos()))
	}
	last := returns[1]
	if len(last.Results) != 1 || exprString(last.Results[0]) != "p.result()" ||
		fn.Body.List[len(fn.Body.List)-1] != last {
		t.Errorf("%s: the last statement of ParseStream is not return p.result()", fset.Position(last.Pos()))
	}
}

func contains(outer, inner ast.Node) bool {
	return outer.Pos() <= inner.Pos() && inner.End() <= outer.End()
}

// exprString renders the few node shapes checkParseStream compares.
func exprString(n ast.Node) string {
	switch n := n.(type) {
	case *ast.CallExpr:
		return exprString(n.Fun) + "()"
	case *ast.SelectorExpr:
		return exprString(n.X) + "." + n.Sel.Name
	case *ast.Ident:
		return n.Name
	case *ast.AssignStmt:
		if len(n.Rhs) == 1 {
			return exprString(n.Rhs[0])
		}
	}
	return ""
}
