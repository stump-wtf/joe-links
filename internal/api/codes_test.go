package api_test

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/joestump/joe-links/internal/api"
	"github.com/joestump/joe-links/internal/auth"
)

// upperSnakeRe matches machine-readable error codes: UPPER_SNAKE only.
var upperSnakeRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// TestErrorCodes_AllWriteErrorCallSitesUseUpperSnakeConstants parses the api
// package source and asserts that (a) every Code* constant in codes.go has an
// UPPER_SNAKE value, and (b) every writeError call site passes one of those
// constants as its code argument — no ad-hoc string literals, and in
// particular no lowercase codes (issue #205).
func TestErrorCodes_AllWriteErrorCallSitesUseUpperSnakeConstants(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read api package dir: %v", err)
	}
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("no api package source files found")
	}

	// Collect the Code* constants and check their values are UPPER_SNAKE.
	codeConsts := map[string]string{}
	for _, file := range files {
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if !strings.HasPrefix(name.Name, "Code") || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					val, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("unquote %s: %v", name.Name, err)
					}
					if !upperSnakeRe.MatchString(val) {
						t.Errorf("constant %s = %q is not UPPER_SNAKE", name.Name, val)
					}
					codeConsts[name.Name] = val
				}
			}
		}
	}
	if len(codeConsts) == 0 {
		t.Fatal("no Code* constants found in the api package")
	}

	// Every writeError call must pass a Code* constant as its 4th argument.
	calls := 0
	for _, file := range files {
		filename := fset.Position(file.Pos()).Filename
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn, ok := call.Fun.(*ast.Ident)
			if !ok || fn.Name != "writeError" || len(call.Args) != 4 {
				return true
			}
			calls++
			pos := fmt.Sprintf("%s:%d", filename, fset.Position(call.Pos()).Line)
			ident, ok := call.Args[3].(*ast.Ident)
			if !ok {
				t.Errorf("%s: writeError code argument is not an identifier — use a Code* constant, not a literal", pos)
				return true
			}
			if _, ok := codeConsts[ident.Name]; !ok {
				t.Errorf("%s: writeError code argument %s is not a Code* constant from codes.go", pos, ident.Name)
			}
			return true
		})
	}
	if calls == 0 {
		t.Fatal("no writeError call sites found — test is broken")
	}
}

// TestBearerMiddleware401_UsesAPIErrorCodeVocabulary pins the one error body
// written OUTSIDE the api package: the bearer middleware hand-writes its 401
// (auth cannot import api without a cycle), so this test keeps its code field
// inside the UPPER_SNAKE vocabulary and equal to CodeUnauthorized — extending
// the writeError AST guard's umbrella across the package boundary (issue #265).
func TestBearerMiddleware401_UsesAPIErrorCodeVocabulary(t *testing.T) {
	// A request with no Authorization header 401s before the middleware
	// touches its stores, so nil dependencies are safe here.
	mw := auth.NewBearerTokenMiddleware(nil, nil)
	handler := mw.Authenticate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler must not run without a bearer token")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/links", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("401 body is not the standard error JSON: %v", err)
	}
	if body.Code != api.CodeUnauthorized {
		t.Errorf("401 code = %q, want api.CodeUnauthorized (%q)", body.Code, api.CodeUnauthorized)
	}
	if !upperSnakeRe.MatchString(body.Code) {
		t.Errorf("401 code %q is not UPPER_SNAKE", body.Code)
	}
}
