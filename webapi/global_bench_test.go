package webapi_test

import (
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/eventloop"
	"github.com/MoeForge/moejs/webapi"
)

// globalCases are the runtimes of the global-read benchmarks: bare, with a
// loop, after touching Intl, with the Web APIs installed, and with both.
var globalCases = []struct {
	name          string
	loop, install bool
	intl          bool
}{
	{"bare", false, false, false},
	{"intl", false, false, true},
	{"loop", true, false, false},
	{"webapi", true, true, false},
	{"webapi+intl", true, true, true},
}

// globalRuntime returns the runtime of case i, shared or with mutable
// intrinsics, with the script that defines read(n): n iterations that read
// JSON, Math and Object.
func globalRuntime(b *testing.B, mutable bool, i int) *moejs.Runtime {
	c := globalCases[i]
	rt := moejs.NewRuntime(moejs.Options{MutableIntrinsics: mutable})
	if c.loop {
		loop, err := eventloop.New(rt, eventloop.Options{})
		if err != nil {
			b.Fatal(err)
		}
		if c.install {
			if _, err := webapi.Install(loop, webapi.Options{}); err != nil {
				b.Fatal(err)
			}
		}
	}
	src := `
function read(n) {
  let x;
  for (let i = 0; i < n; i++) {
    x = JSON;
    x = Math;
    x = Object;
  }
  return x;
}`
	if c.intl {
		src += "\nIntl.NumberFormat;"
	}
	s, err := moejs.CompileScript("read.js", src)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := rt.RunScript(s); err != nil {
		b.Fatal(err)
	}
	return rt
}

// runRead calls read(n) once.
func runRead(b *testing.B, rt *moejs.Runtime, call *moejs.Script, n int) {
	if err := rt.SetGlobal("N", n); err != nil {
		b.Fatal(err)
	}
	if _, err := rt.RunScript(call); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkGlobalRead is one iteration of read: three reads of global
// variables, in each runtime of globalCases.
func BenchmarkGlobalRead(b *testing.B) {
	call, err := moejs.CompileScript("call.js", "read(N)")
	if err != nil {
		b.Fatal(err)
	}
	for _, mutable := range []bool{false, true} {
		name := "shared"
		if mutable {
			name = "mutable"
		}
		for i, c := range globalCases {
			b.Run(name+"/"+c.name, func(b *testing.B) {
				rt := globalRuntime(b, mutable, i)
				runRead(b, rt, call, 100)
				b.ReportAllocs()
				b.ResetTimer()
				runRead(b, rt, call, b.N)
			})
		}
	}
}

// BenchmarkGlobalLoop is read(1e6): a million iterations, three million
// global reads.
func BenchmarkGlobalLoop(b *testing.B) {
	call, err := moejs.CompileScript("call.js", "read(1e6)")
	if err != nil {
		b.Fatal(err)
	}
	for _, mutable := range []bool{false, true} {
		name := "shared"
		if mutable {
			name = "mutable"
		}
		for i, c := range globalCases {
			b.Run(name+"/"+c.name, func(b *testing.B) {
				rt := globalRuntime(b, mutable, i)
				b.ReportAllocs()
				for b.Loop() {
					if _, err := rt.RunScript(call); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// TestGlobalsKeepShape checks that installing the Web APIs defines nothing
// on the global object of a runtime with the shared builtins, and that the
// global object stays in shape mode once a script has read every global,
// so global variables keep their inline caches.
func TestGlobalsKeepShape(t *testing.T) {
	for _, mutable := range []bool{false, true} {
		rt := moejs.NewRuntime(moejs.Options{MutableIntrinsics: mutable})
		loop, err := eventloop.New(rt, eventloop.Options{})
		if err != nil {
			t.Fatal(err)
		}
		g := rt.Realm().Global
		n := g.Shape().Count()
		if _, err := webapi.Install(loop, webapi.Options{}); err != nil {
			t.Fatal(err)
		}
		if !mutable && g.Shape().Count() != n {
			t.Errorf("Install defined %d globals", g.Shape().Count()-n)
		}
		s, err := moejs.CompileScript("all.js", `Object.getOwnPropertyNames(globalThis).map(k => typeof globalThis[k]).length`)
		if err != nil {
			t.Fatal(err)
		}
		v, err := rt.RunScript(s)
		if err != nil {
			t.Fatal(err)
		}
		if g.IsDictionaryMode() || g.Shape().Count() != int(v.AsNumber()) {
			t.Errorf("mutable=%v: %v globals, dictionary mode %v", mutable, v, g.IsDictionaryMode())
		}
	}
}
