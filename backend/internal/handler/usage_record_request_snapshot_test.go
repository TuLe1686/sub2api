package handler

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsageRecordTasksDoNotCaptureGinContext(t *testing.T) {
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range matches {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || !isUsageRecordTaskSubmit(call.Fun) {
					return true
				}
				for _, arg := range call.Args {
					fn, ok := arg.(*ast.FuncLit)
					if !ok {
						continue
					}
					ast.Inspect(fn.Body, func(node ast.Node) bool {
						ident, ok := node.(*ast.Ident)
						if ok && ident.Name == "c" {
							t.Errorf("usage task closure captures Gin context at %s; snapshot request values before submit", token.NewFileSet().Position(ident.Pos()))
							return false
						}
						return true
					})
				}
				return true
			})
		})
	}
}

func isUsageRecordTaskSubmit(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return strings.HasPrefix(sel.Sel.Name, "submit") && strings.Contains(sel.Sel.Name, "UsageRecordTask")
}
