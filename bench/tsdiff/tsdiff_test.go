package tsdiff

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/MoeForge/moejs/syntax"
	"github.com/evanw/esbuild/pkg/api"
)

// tsCorpusDir is the TypeScript corpus of TestTSDifferential and
// TestPiExtensions: MOEJS_TS_CORPUS, default ~/.cache/moejs/ts-corpus,
// holding the checkouts bench/tscorpus/fetch.sh makes of pi-mono, the src
// directory of TypeScript (release-6.0: main holds its Go port) and
// TypeBox. The tests skip without it.
func tsCorpusDir(t *testing.T) string {
	dir := os.Getenv("MOEJS_TS_CORPUS")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".cache", "moejs", "ts-corpus")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("TypeScript corpus not found at %s", dir)
	}
	return dir
}

// TestTSDifferential parses every .ts file of the corpus with moejs's
// TypeScript erasure and compares the tree with the one moejs parses from
// esbuild's output for the file (TransformOptions: LoaderTS, ESNext, and
// no Format: the ESM format moves every export into one clause at the
// end, which no comparison of trees can follow). Both trees go through
// tsNormalDump, which undoes what esbuild changes beyond erasing types.
//
// A file is filtered, not failed, when moejs rejects it for TypeScript
// with run-time semantics, which esbuild compiles (enums, namespaces with
// values, parameter properties, import aliases used as values), or when
// moejs rejects esbuild's output too, for a JavaScript feature it does not
// support (import attributes, await using; see TODO.md). Every other
// difference fails.
func TestTSDifferential(t *testing.T) {
	root := tsCorpusDir(t)
	for _, sub := range []string{"pi-mono", filepath.Join("TypeScript", "src"), "typebox"} {
		dir := filepath.Join(root, sub)
		if _, err := os.Stat(dir); err != nil {
			t.Logf("%s: not found", sub)
			continue
		}
		var files, same int
		filtered := make(map[string]int)
		var failing []string
		filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "node_modules" || d.Name() == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".ts") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files++
			rel, _ := filepath.Rel(root, path)
			why, ok := tsCompare(rel, string(src))
			switch {
			case ok:
				same++
			case why == "non-erasable" || why == "unsupported-js":
				filtered[why]++
			default:
				failing = append(failing, rel+": "+why)
			}
			return nil
		})
		t.Logf("%s: %d files, %d identical, %d filtered %v, %d failing", sub, files, same, filtered["non-erasable"]+filtered["unsupported-js"], filtered, len(failing))
		for _, f := range failing {
			t.Error(f)
		}
	}
}

// tsCompare compares the trees of src and of esbuild's output for it,
// reporting why they differ when they do: "non-erasable", "unsupported-js",
// or a description of the difference.
func tsCompare(name, src string) (why string, same bool) {
	mt, terr := syntax.ParseModule(name, src, syntax.Options{TypeScript: true})
	res := api.Transform(src, api.TransformOptions{Loader: api.LoaderTS, Target: api.ESNext, Sourcefile: name})
	if len(res.Errors) > 0 {
		if terr != nil {
			return "both fail: " + terr.Error() + " / esbuild: " + res.Errors[0].Text, false
		}
		return "esbuild fails: " + res.Errors[0].Text, false
	}
	mj, jerr := syntax.ParseModule(name+".js", string(res.Code), syntax.Options{})
	switch {
	case terr != nil && strings.Contains(terr.Error(), "only erasable TypeScript"):
		return "non-erasable", false
	case terr != nil && jerr != nil && terr.(*syntax.Error).Msg == jerr.(*syntax.Error).Msg:
		return "unsupported-js", false
	case terr != nil:
		return terr.Error(), false
	case jerr != nil:
		return "esbuild's output: " + jerr.Error(), false
	}
	a, b := strings.Split(tsNormalDump(mt), "\n"), strings.Split(tsNormalDump(mj), "\n")
	for i := range max(len(a), len(b)) {
		var x, y string
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return "line " + strconv.Itoa(i+1) + " of the dump:\n  moejs:   " + x + "\n  esbuild: " + y, false
		}
	}
	return "", true
}

var undefinedRe = regexp.MustCompile(`\bundefined\b`)

// tsNormalDump dumps m after undoing what esbuild changes in its output
// beyond erasing types:
//   - it drops an empty export {} (moejs drops it in TypeScript; it has no
//     effect in a module);
//   - it prints a block-level function declaration as let f = function
//     at the start of its block (strict code, where that is the same);
//   - it renames local and import bindings that shadow or collide with
//     other names (stream2), so every such binding gets a name by its
//     order of appearance, and its kind (let, param, import...). Which
//     identifiers share a binding, and its kind, are compared that way,
//     but not the scope it is in: two trees whose identifiers group alike
//     into bindings of the same kinds in other scopes compare equal;
//   - it prints {a: a} as {a}, so shorthand properties are printed as
//     key: value;
//   - it prints +1 as 1, undefined as void 0, folds string concatenations
//     and templates, and drops the parentheses of (a?.b)?.c.
func tsNormalDump(m *syntax.Module) string {
	m.Body = slices.DeleteFunc(m.Body, func(s syntax.Stmt) bool {
		e, ok := s.(*syntax.ExportNamed)
		return ok && e.Source == nil && len(e.Specs) == 0
	})
	hoisted := hoistBlockFunctions(m)
	names := make(map[*syntax.Binding]string)
	syntax.Inspect(m, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.ImportSpec:
			if n.Local == n.Imported {
				imported := *n.Imported
				imported.Binding = nil
				n.Imported = &imported
			}
		case *syntax.Ident:
			if b := n.Binding; b != nil && b.Scope != nil && (b.Scope.Kind != syntax.ScopeModule || b.Kind == syntax.BindImport || b.Kind == syntax.BindImportNS) {
				name, ok := names[b]
				if !ok {
					kind := b.Kind.String()
					if hoisted[b] {
						kind = "let"
					}
					name = "$" + strconv.Itoa(len(names)) + kind
					names[b] = name
				}
				n.Name = name
			}
		case *syntax.Property:
			if n.Kind == syntax.PropShorthand {
				n.Kind = syntax.PropInit
			}
		}
		return true
	})
	rewriteExprs(m, func(x syntax.Expr) syntax.Expr {
		if u, ok := x.(*syntax.UnaryExpr); ok && u.Op == syntax.Plus {
			if n, ok := u.X.(*syntax.NumberLit); ok {
				return n
			}
		}
		return x
	})
	foldStrings(m)
	unwrapOptChains(m)
	return undefinedRe.ReplaceAllString(syntax.Dump(m), "(void 0)")
}

// foldStrings folds string + string and templates with templates or
// strings, as esbuild does.
func foldStrings(m *syntax.Module) {
	stringish := func(x syntax.Expr) (*syntax.TemplateLit, bool) {
		switch x := x.(type) {
		case *syntax.StringLit:
			return &syntax.TemplateLit{Quasis: []syntax.TemplateElement{{Cooked: x.Value}}}, true
		case *syntax.TemplateLit:
			for _, q := range x.Quasis {
				if q.Cooked == syntax.InvalidCooked {
					return nil, false
				}
			}
			return x, true
		}
		return nil, false
	}
	var fold func(x syntax.Expr) syntax.Expr
	fold = func(x syntax.Expr) syntax.Expr {
		switch e := x.(type) {
		case *syntax.BinaryExpr:
			if e.Op != syntax.Plus {
				return x
			}
			l, ok1 := stringish(e.X)
			r, ok2 := stringish(e.Y)
			if !ok1 || !ok2 {
				return x
			}
			t := &syntax.TemplateLit{Span: e.Span}
			t.Quasis = append(t.Quasis, l.Quasis...)
			t.Exprs = append(t.Exprs, l.Exprs...)
			last := &t.Quasis[len(t.Quasis)-1]
			last.Cooked += r.Quasis[0].Cooked
			t.Quasis = append(t.Quasis, r.Quasis[1:]...)
			t.Exprs = append(t.Exprs, r.Exprs...)
			return fold(t)
		case *syntax.TemplateLit:
			if len(e.Exprs) == 0 {
				return &syntax.StringLit{Span: e.Span, Value: e.Quasis[0].Cooked}
			}
		}
		return x
	}
	rewriteExprs(m, fold)
}

var (
	exprType = reflect.TypeFor[syntax.Expr]()
	nodeType = reflect.TypeFor[syntax.Node]()
	skipType = map[reflect.Type]bool{
		reflect.TypeFor[*syntax.Binding](): true,
		reflect.TypeFor[*syntax.Scope]():   true,
		reflect.TypeFor[*syntax.File]():    true,
	}
)

// rewriteExprs replaces every expression below n by f of it, children
// first.
func rewriteExprs(n any, f func(syntax.Expr) syntax.Expr) {
	v := reflect.ValueOf(n)
	if v.Kind() != reflect.Pointer || v.IsNil() || skipType[v.Type()] {
		return
	}
	e := v.Elem()
	if e.Kind() != reflect.Struct {
		return
	}
	set := func(fv reflect.Value) {
		if fv.IsNil() {
			return
		}
		rewriteExprs(fv.Interface(), f)
		if x, ok := fv.Interface().(syntax.Expr); ok && (fv.Type() == exprType || fv.Type() == nodeType) {
			fv.Set(reflect.ValueOf(f(x)))
		}
	}
	for i := range e.NumField() {
		fv := e.Field(i)
		if !e.Type().Field(i).IsExported() {
			continue
		}
		switch fv.Kind() {
		case reflect.Interface:
			set(fv)
		case reflect.Pointer:
			rewriteExprs(fv.Interface(), f)
		case reflect.Struct:
			if fv.CanAddr() {
				rewriteExprs(fv.Addr().Interface(), f)
			}
		case reflect.Slice:
			for j := range fv.Len() {
				el := fv.Index(j)
				switch el.Kind() {
				case reflect.Interface:
					set(el)
				case reflect.Pointer:
					rewriteExprs(el.Interface(), f)
				}
			}
		}
	}
}

// hoistBlockFunctions moves the function declarations of a block that is
// not a function body to its start, as let name = function () {}, and
// returns their bindings.
func hoistBlockFunctions(m *syntax.Module) map[*syntax.Binding]bool {
	hoisted := make(map[*syntax.Binding]bool)
	bodies := make(map[*syntax.BlockStmt]bool)
	syntax.Inspect(m, func(n syntax.Node) bool {
		if fn, ok := n.(*syntax.Function); ok && fn.Body != nil {
			bodies[fn.Body] = true
		}
		return true
	})
	syntax.Inspect(m, func(n syntax.Node) bool {
		b, ok := n.(*syntax.BlockStmt)
		if !ok || bodies[b] {
			return true
		}
		var decls, rest []syntax.Stmt
		for _, s := range b.Body {
			fd, ok := s.(*syntax.FuncDecl)
			if !ok {
				rest = append(rest, s)
				continue
			}
			fn := *fd.Func
			name := fn.Name
			fn.Name = nil
			hoisted[name.Binding] = true
			decls = append(decls, &syntax.VarDecl{Kind: syntax.DeclLet, Decls: []*syntax.Declarator{{Target: name, Init: &fn}}})
		}
		if decls != nil {
			b.Body = append(decls, rest...)
		}
		return true
	})
	return hoisted
}

// unwrapOptChains drops the parentheses of (a?.b)?.c, which is a?.b?.c.
func unwrapOptChains(m *syntax.Module) {
	rewriteExprs(m, func(x syntax.Expr) syntax.Expr {
		oc, ok := x.(*syntax.OptChain)
		if !ok {
			return x
		}
		link := oc.X
		for {
			var inner *syntax.Expr
			optional := false
			switch l := link.(type) {
			case *syntax.MemberExpr:
				inner, optional = &l.Object, l.Optional
			case *syntax.CallExpr:
				inner, optional = &l.Callee, l.Optional
			default:
				return x
			}
			if c, ok := (*inner).(*syntax.OptChain); ok {
				if optional {
					*inner = c.X
				}
				return x
			}
			link = *inner
		}
	})
}
