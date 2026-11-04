package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestReadsSettingsOnlyThroughDeployconfig is half of #241's "a test fails
// when cmd/web reads a setting the contract doesn't list": cmd/web may not
// read the environment, or load config with viper, itself. Every setting it
// uses comes from deployconfig.Config, whose reads
// internal/deployconfig's TestLoadReadsOnlyListedSettings holds to the
// deployconfig.Settings table, the contract docs/Deployment.md documents.
func TestReadsSettingsOnlyThroughDeployconfig(t *testing.T) {
	forbidden := map[string]map[string]bool{
		"os":      {"Getenv": true, "LookupEnv": true, "Environ": true, "ExpandEnv": true, "Expand": true},
		"syscall": {"Getenv": true, "Environ": true},
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			if path, _ := strconv.Unquote(imp.Path.Value); strings.Contains(path, "spf13/viper") {
				t.Errorf("%s imports %s: load settings through internal/deployconfig", fset.Position(imp.Pos()), path)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && forbidden[pkg.Name][sel.Sel.Name] {
				t.Errorf("%s reads the environment with %s.%s: add the setting to internal/deployconfig's Settings and docs/Deployment.md, and read it from deployconfig.Config", fset.Position(sel.Pos()), pkg.Name, sel.Sel.Name)
			}
			return true
		})
	}
}
