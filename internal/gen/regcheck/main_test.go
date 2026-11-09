package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGenerated checks that engine/interp_run_regcheck.go is the checked
// copy of the current engine/interp_run.go: a change to run that is not
// generated again fails here (go generate ./engine).
func TestGenerated(t *testing.T) {
	src, err := os.ReadFile("../../../engine/interp_run.go")
	if err != nil {
		t.Fatal(err)
	}
	want, err := generate(src)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("../../../engine/interp_run_regcheck.go")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatal("engine/interp_run_regcheck.go is not the checked copy of engine/interp_run.go: run go generate ./engine")
	}
	cp := string(got)
	for _, s := range []string{"st.window(", "regs := st.", "//go:generate", "nonNilWindow("} {
		if strings.Contains(cp, s) {
			t.Errorf("the copy still has %q", s)
		}
	}
	if !strings.HasPrefix(cp, "// Code generated") || !strings.Contains(cp, "//go:build moejsregcheck\n") {
		t.Error("the copy lacks its header or build constraint")
	}
	for _, s := range []string{"regs.dispatch(insns, ipc)", "regs.handler(h)", "regs := newCheckedRegs(st, base, code)"} {
		if strings.Count(cp, s) != 1 {
			t.Errorf("the copy has %q %d times, want once", s, strings.Count(cp, s))
		}
	}
	if strings.Count(cp, "regs.at(") < 100 {
		t.Errorf("the copy checks %d register accesses", strings.Count(cp, "regs.at("))
	}
}

// TestGenerateRejects checks that the generator stops on a source it
// could not check completely.
func TestGenerateRejects(t *testing.T) {
	const ok = fastTag + "\n" + directive + "\npackage engine\n\nfunc (r *Realm) run() {\n\tregs := st.window(base)\n\tfor {\n\t\tipc := pc\n\t\tregs[a] = regs[uint8(insns[pc])]\n\t\tif h.Kind == bytecode.HandlerCatch {\n\t\t}\n\t}\n}\n"
	if _, err := generate([]byte(ok)); err != nil {
		t.Fatalf("the minimal source: %v", err)
	}
	for name, src := range map[string]string{
		"a refresh":           strings.Replace(ok, "\t\tipc := pc\n", "\t\tipc := pc\n\t\tregs = st.window(base)\n", 1),
		"a field named regs":  strings.Replace(ok, "\t\tipc := pc\n", "\t\tipc := pc\n\t\tg.regs = nil\n", 1),
		"regs in a comment":   strings.Replace(ok, "\t\tipc := pc\n", "\t\tipc := pc\n\t\t// regs, the window\n", 1),
		"regs in a string":    strings.Replace(ok, "\t\tipc := pc\n", "\t\tipc := pc\n\t\t_ = \"regs\"\n", 1),
		"an address of regs[": strings.Replace(ok, "\t\tipc := pc\n", "\t\tipc := pc\n\t\tp := &regs[a]\n", 1),
		"the non-nil window":  strings.Replace(ok, "\t\tipc := pc\n", "\t\t"+nonNil+"\n\t\tipc := pc\n", 1),
	} {
		if _, err := generate([]byte(src)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, src := range map[string]string{
		"no build constraint":  strings.Replace(ok, fastTag, "", 1),
		"no generate line":     strings.Replace(ok, directive, "", 1),
		"no first window":      strings.Replace(ok, "regs := st.window(base)", "regs := other(base)", 1),
		"another window":       strings.Replace(ok, "\tfor {", "\tx := st.stack.window(1)\n\tfor {", 1),
		"a slice of regs":      strings.Replace(ok, "regs[a] =", "_ = regs[1:2]\n\t\tregs[a] =", 1),
		"no dispatch anchor":   strings.Replace(ok, "ipc := pc", "ipc := pc + 0", 1),
		"no handler anchor":    strings.Replace(ok, "if h.Kind == bytecode.HandlerCatch {", "if h.Kind != 0 {", 1),
		"two dispatch anchors": strings.Replace(ok, "ipc := pc\n", "ipc := pc\n\t\tipc := pc\n", 1),
		"a window outside run": ok + "\nfunc f() { _ = st.window(base) }\n",
		// The window used other than through an index escapes the checks.
		"regs passed on":         strings.Replace(ok, "regs[a] =", "use(regs)\n\t\tregs[a] =", 1),
		"regs as a pointer":      strings.Replace(ok, "regs[a] =", "_ = unsafe.Pointer(regs)\n\t\tregs[a] =", 1),
		"regs compared":          strings.Replace(ok, "regs[a] =", "if regs == nil {\n\t\t}\n\t\tregs[a] =", 1),
		"regs as an any":         strings.Replace(ok, "regs[a] =", "var x any = regs\n\t\tregs[a] =", 1),
		"a copy of the window":   strings.Replace(ok, "regs[a] =", "w := regs\n\t\tregs[a] =", 1),
		"regs assigned another":  strings.Replace(ok, "regs[a] =", "regs = other\n\t\tregs[a] =", 1),
		"regs with another pair": strings.Replace(ok, "regs[a] =", "regs, fr = st.window(base), nil\n\t\tregs[a] =", 1),
		"another base":           strings.Replace(ok, "regs[a] =", "regs = st.window(base+1)\n\t\tregs[a] =", 1),
		"non-nil of another":     strings.Replace(ok, "regs[a] =", "regs = nonNilWindow(other)\n\t\tregs[a] =", 1),
		"non-nil as a value":     strings.Replace(ok, "regs[a] =", "x := nonNilWindow(regs)\n\t\tregs[a] =", 1),
		"non-nil in a line":      strings.Replace(ok, "regs[a] =", "regs = nonNilWindow(regs); use(regs)\n\t\tregs[a] =", 1),
	} {
		if _, err := generate([]byte(src)); err == nil {
			t.Errorf("%s: generated", name)
		}
	}
}

// TestWindowOnlyInRun checks that run is the only function of the engine
// that takes a window (interpState.window): the checked copy checks run's
// accesses only, so a window anywhere else would go unchecked.
func TestWindowOnlyInRun(t *testing.T) {
	files, err := filepath.Glob("../../../engine/*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no engine sources: %v", err)
	}
	fs := token.NewFileSet()
	runs := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fs, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			uses := 0
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if se, ok := n.(*ast.SelectorExpr); ok && se.Sel.Name == "window" {
					uses++
				}
				return true
			})
			if uses == 0 {
				continue
			}
			if fd.Name.Name != "run" || filepath.Base(path) != "interp_run.go" {
				t.Errorf("%s: %s takes a window", fs.Position(fd.Pos()), fd.Name.Name)
				continue
			}
			runs++
		}
	}
	if runs != 1 {
		t.Errorf("run takes its windows in %d functions of interp_run.go, want 1", runs)
	}
}
