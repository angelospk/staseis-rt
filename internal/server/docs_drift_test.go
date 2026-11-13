package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/angelospk/athens-transit-rt/internal/buildinfo"
	"github.com/angelospk/athens-transit-rt/internal/model"
)

// The error codes the handlers send are exactly the Error enum of the OpenAPI spec, and every
// Source state is in the spec and in CONTRACT.md: clients are written from those texts, so a
// code or state they do not list is a client bug waiting to happen.
func TestDocsMatchErrorCodesAndStates(t *testing.T) {
	codes := errorCodesInCode(t)
	spec := readDoc(t, "openapi/openapi.yaml")
	enum := enumAfter(t, spec, "enum: [unknown_line")
	if !slices.Equal(codes, enum) {
		t.Errorf("error codes in the code %v\n  OpenAPI Error enum     %v", codes, enum)
	}
	states := enumAfter(t, spec, "enum: [ok,")
	contract := readDoc(t, "CONTRACT.md")
	for _, s := range []string{model.OK, model.WarmingUp, model.Stale, model.Indicative, model.Down} {
		if !slices.Contains(states, s) {
			t.Errorf("Source state %q not in the OpenAPI SourceState enum %v", s, states)
		}
		if !strings.Contains(contract, "`"+s+"`") {
			t.Errorf("Source state %q not in CONTRACT.md", s)
		}
	}
}

// errorCodesInCode collects the codes of {"error": "<code>"} bodies in the package's handlers:
// map literals with an "error" key, notFound(w, "<code>"), and JSON written as a literal.
func errorCodesInCode(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	raw := regexp.MustCompile(`\{"error":"([a-z_]+)"\}`)
	set := map[string]bool{}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.KeyValueExpr:
				if k, ok := lit(n.Key); ok && k == "error" {
					if v, ok := lit(n.Value); ok {
						set[v] = true
					}
				}
			case *ast.CallExpr:
				if id, ok := n.Fun.(*ast.Ident); ok && (id.Name == "notFound" || id.Name == "planError") && len(n.Args) == 2 {
					if v, ok := lit(n.Args[1]); ok {
						set[v] = true
					}
				}
			case *ast.BasicLit:
				if v, ok := lit(n); ok {
					for _, m := range raw.FindAllStringSubmatch(v, -1) {
						set[m[1]] = true
					}
				}
			}
			return true
		})
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func lit(e ast.Expr) (string, bool) {
	b, ok := e.(*ast.BasicLit)
	if !ok || b.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(b.Value)
	return s, err == nil
}

func readDoc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// enumAfter is the sorted members of the one-line YAML enum that starts with prefix.
func enumAfter(t *testing.T, doc, prefix string) []string {
	t.Helper()
	i := strings.Index(doc, prefix)
	if i < 0 || strings.Count(doc, prefix) != 1 {
		t.Fatalf("no single %q in the spec", prefix)
	}
	line := doc[i+len("enum: [") : i+strings.IndexByte(doc[i:], ']')]
	var out []string
	for _, m := range strings.Split(line, ",") {
		out = append(out, strings.TrimSpace(m))
	}
	sort.Strings(out)
	return out
}

// The contract revision in the code (/v1/status contract_rev) is the one the OpenAPI spec and
// CONTRACT.md publish, and the Status schema documents the build fields (contract rev 24). The
// fields stay optional in the schema: a rolled-back rev 23 server must still pass live-check.
func TestDocsMatchContractRev(t *testing.T) {
	spec := readDoc(t, "openapi/openapi.yaml")
	want := "version: \"1 (contract rev " + strconv.Itoa(buildinfo.ContractRev) + ")\""
	if !strings.Contains(spec, want) {
		t.Errorf("OpenAPI info.version is not %s", want)
	}
	i := strings.Index(spec, "\n    Status:\n")
	if i < 0 {
		t.Fatal("no Status schema in the spec")
	}
	schema := spec[i+1:]
	schema = schema[:strings.Index(schema, "\n\n")]
	for _, f := range []string{"rev", "contract_rev"} {
		if !strings.Contains(schema, "\n        "+f+":\n") {
			t.Errorf("Status schema has no property %q", f)
		}
	}
	contract := readDoc(t, "CONTRACT.md")
	if want := "\nRevision: " + strconv.Itoa(buildinfo.ContractRev) + " "; !strings.Contains(contract, want) {
		t.Errorf("CONTRACT.md Revision line is not %q", strings.TrimSpace(want))
	}
	for _, f := range []string{`"rev"`, `"contract_rev"`} {
		if !strings.Contains(contract, f) {
			t.Errorf("CONTRACT.md /v1/status example has no %s", f)
		}
	}
}
