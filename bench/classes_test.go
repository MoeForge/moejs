package bench

import (
	"fmt"
	"strings"
	"testing"

	"github.com/MoeForge/moejs/bench/engines"
)

// classModule holds the class micro programs (shaped like BenchmarkMicro:
// one call = microIterations iterations, checksum compared with Sobek's):
// construction of a base class, of a class with field initializers and of a
// derived class, method calls, private field reads and writes, and super
// method calls.
var classModule = strings.ReplaceAll(`
const N = __N__;
class Vec {
  constructor(x, y) { this.x = x; this.y = y; }
  dot(o) { return this.x * o.x + this.y * o.y; }
}
class Particle {
  vx = 0;
  vy = 1;
  constructor(x) { this.x = x; }
}
class Vec3 extends Vec {
  constructor(x, y, z) { super(x, y); this.z = z; }
}
class Counter {
  #n = 0;
  inc() { this.#n = this.#n + 1; return this.#n; }
}
class Shape { area() { return 1; } }
class Square extends Shape { area() { return super.area() + 1; } }
const V = new Vec(1, 2), S = new Square();
export function classNew() { let s = 0; for (let i = 0; i < N; i++) s += new Vec(i, 1).x; return s; }
export function classNewFields() { let s = 0; for (let i = 0; i < N; i++) s += new Particle(i).vy; return s; }
export function classNewDerived() { let s = 0; for (let i = 0; i < N; i++) s += new Vec3(i, 1, 2).z; return s; }
export function classMethod() { let s = 0; for (let i = 0; i < N; i++) s += V.dot(V); return s; }
export function privateGetSet() { const c = new Counter(); let s = 0; for (let i = 0; i < N; i++) s += c.inc(); return s; }
export function superMethod() { let s = 0; for (let i = 0; i < N; i++) s += S.area(); return s; }
`, "__N__", fmt.Sprint(microIterations))

var classNames = []string{"classNew", "classNewFields", "classNewDerived", "classMethod", "privateGetSet", "superMethod"}

func classRuntime(tb testing.TB, e engines.Engine) engines.Runtime {
	mod, err := e.Compile("classes.js", classModule)
	if err != nil {
		tb.Fatal(err)
	}
	rt, err := e.NewRuntime()
	if err != nil {
		tb.Fatal(err)
	}
	if err := rt.Instantiate(mod); err != nil {
		tb.Fatal(err)
	}
	return rt
}

// BenchmarkMicroClasses compares moejs with Sobek on the class workloads;
// the checksum is validated against Sobek's on every iteration.
func BenchmarkMicroClasses(b *testing.B) {
	oracle := classRuntime(b, engines.NewSobekEngine())
	defer oracle.Close()
	expected := map[string]any{}
	for _, name := range classNames {
		out, err := oracle.Call(name, nil)
		if err != nil {
			b.Fatalf("sobek %s: %v", name, err)
		}
		expected[name], _ = Normalize(out)
	}
	for _, e := range engines.PureGo() {
		rt := classRuntime(b, e)
		for _, name := range classNames {
			want := expected[name]
			b.Run(name+"/"+e.Name(), func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					out, err := rt.Call(name, nil)
					if err != nil {
						b.Fatal(err)
					}
					got, _ := Normalize(out)
					if got != want {
						b.Fatalf("%s: expected %v, got %v", name, want, got)
					}
				}
			})
		}
		rt.Close()
	}
}
