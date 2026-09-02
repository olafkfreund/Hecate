package ops

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// declaredConstants returns the string values of every const in explain.go
// declared with the given type, e.g. "BlockerKind" or "State" — read straight
// off each declaration's literal, not hand-mapped from name to value, so this
// test is not itself a third copy of the set.
//
// It parses the source rather than using reflection because Go constants have
// no runtime existence to enumerate — the only way to check that a canonical
// list (AllBlockerKinds, AllStates) has not been left behind by a newly added
// constant is to read the declarations themselves.
func declaredConstants(t *testing.T, typeName string) []string {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "explain.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing explain.go: %v", err)
	}

	var values []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			ident, ok := vs.Type.(*ast.Ident)
			if !ok || ident.Name != typeName {
				continue
			}
			for i, n := range vs.Names {
				if i >= len(vs.Values) {
					t.Fatalf("const %s has no literal value; declaredConstants can't read it", n.Name)
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("const %s is not a plain string literal; declaredConstants can't read it", n.Name)
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("const %s: %v", n.Name, err)
				}
				values = append(values, v)
			}
		}
	}
	return values
}

// TestAllBlockerKindsIsComplete is the guard the MCP why_stuck outputSchema
// (D33) depends on transitively: pkg/mcp builds its enum from AllBlockerKinds,
// so a BlockerKind constant added here without a matching entry in
// AllBlockerKinds would silently reach that schema too — the exact drift D49
// (#30) caused when BlockerChangeHeld was added and tools.go's own hand-typed
// copy was never touched. See D64.
func TestAllBlockerKindsIsComplete(t *testing.T) {
	declared := declaredConstants(t, "BlockerKind")

	listed := map[string]bool{}
	for _, k := range AllBlockerKinds {
		listed[string(k)] = true
	}
	for _, v := range declared {
		if !listed[v] {
			t.Errorf("BlockerKind %q is declared in explain.go but missing from AllBlockerKinds "+
				"(and so from the why_stuck outputSchema)", v)
		}
	}
	if got, want := len(AllBlockerKinds), len(declared); got != want {
		t.Errorf("AllBlockerKinds has %d entries, but %d BlockerKind constants are declared", got, want)
	}
}

func TestAllStatesIsComplete(t *testing.T) {
	declared := declaredConstants(t, "State")

	listed := map[string]bool{}
	for _, s := range AllStates {
		listed[string(s)] = true
	}
	for _, v := range declared {
		if !listed[v] {
			t.Errorf("State %q is declared in explain.go but missing from AllStates "+
				"(and so from the why_stuck outputSchema)", v)
		}
	}
	if got, want := len(AllStates), len(declared); got != want {
		t.Errorf("AllStates has %d entries, but %d State constants are declared", got, want)
	}
}
