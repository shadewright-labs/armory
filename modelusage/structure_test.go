package modelusage

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestStreamHasOneExit reads the package source and checks the shape that keeps every ParseStream result in
// one place:
//
//   - ParseStream is exactly parseStreamSource: the refusal of invalid limits, before any byte is read, the
//     loop over step, and return p.result();
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

// parseStreamSource is the whole of ParseStream, as go/printer prints it without its doc comment. Any change
// to it, even to the condition of the first guard, fails the test, so that a second way out cannot slip in.
const parseStreamSource = `func ParseStream(f Family, r io.Reader, lim Limits) (Usage, error) {
	if err := lim.check(f, true); err != nil {
		return Usage{}, err
	}
	p := newStreamParser(f, r, lim)
	for p.step() {
	}
	return p.result()
}`

// checkParseStream checks that ParseStream is exactly parseStreamSource: it returns only on invalid limits,
// before reading, and through p.result().
func checkParseStream(t *testing.T, fset *token.FileSet, fn *ast.FuncDecl) {
	t.Helper()
	bare := *fn
	bare.Doc = nil
	var b strings.Builder
	if err := printer.Fprint(&b, fset, &bare); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != parseStreamSource {
		t.Errorf("%s: ParseStream is\n%s\nwant exactly\n%s", fset.Position(fn.Pos()), got, parseStreamSource)
	}
}
