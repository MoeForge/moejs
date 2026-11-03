package bench

import (
	"encoding/json"
	"fmt"
	"syscall"
	"testing"
	"time"

	"github.com/MoeForge/moejs/bench/engines"
)

// computeModule holds CPU-bound kernels outside the plugin corpus: each
// export does milliseconds of pure JavaScript work and returns a checksum, so
// the call boundary is noise and the interpreter (or JIT) is what is timed.
// Every result is exact on all engines (integer arithmetic, or IEEE basic
// operations and Math.sqrt only).
const computeModule = `
function fib(n) { return n < 2 ? n : fib(n - 1) + fib(n - 2); }
export function fibRec() { return fib(25); }

export function intLoop() {
  let s = 0;
  for (let i = 0; i < 1000000; i++) s = (s + (i ^ (i >>> 3)) * 7) % 1000003;
  return s;
}

export function floatLoop() {
  let s = 0;
  for (let i = 1; i <= 1000000; i++) s += Math.sqrt(i) / (i + 0.5);
  return s;
}

export function sieve() {
  const n = 500000, a = new Array(n + 1).fill(true);
  a[0] = a[1] = false;
  for (let i = 2; i * i <= n; i++) if (a[i]) for (let j = i * i; j <= n; j += i) a[j] = false;
  let c = 0;
  for (let i = 0; i <= n; i++) if (a[i]) c++;
  return c;
}

export function matmul() {
  const n = 64, a = new Float64Array(n * n), b = new Float64Array(n * n), c = new Float64Array(n * n);
  for (let i = 0; i < n * n; i++) { a[i] = (i % 13) / 13; b[i] = (i % 7) / 7; }
  for (let i = 0; i < n; i++) for (let k = 0; k < n; k++) { const aik = a[i * n + k]; for (let j = 0; j < n; j++) c[i * n + j] += aik * b[k * n + j]; }
  let s = 0;
  for (let i = 0; i < n * n; i++) s += c[i];
  return s;
}

export function sortCmp() {
  const n = 30000, a = new Array(n);
  let x = 2463534242;
  for (let i = 0; i < n; i++) { x ^= x << 13; x ^= x >>> 17; x ^= x << 5; x >>>= 0; a[i] = x % 1000000; }
  a.sort((p, q) => p - q);
  let s = 0;
  for (let i = 0; i < n; i += 7) s = (s + a[i]) % 1000003;
  return s;
}

function makeTree(d) { return d === 0 ? { l: null, r: null } : { l: makeTree(d - 1), r: makeTree(d - 1) }; }
function checkTree(t) { return t.l === null ? 1 : 1 + checkTree(t.l) + checkTree(t.r); }
export function binaryTrees() {
  let s = 0;
  for (let i = 0; i < 16; i++) s += checkTree(makeTree(12));
  return s;
}

class Vec {
  constructor(x, y) { this.x = x; this.y = y; }
  add(o) { return new Vec(this.x + o.x, this.y + o.y); }
  dot(o) { return this.x * o.x + this.y * o.y; }
}
export function classMethods() {
  let acc = new Vec(0, 0), d = 0;
  for (let i = 0; i < 200000; i++) { const v = new Vec(i & 15, i & 7); acc = acc.add(v); d += v.dot(acc) % 97; }
  return acc.x + acc.y + d;
}

export function arrayHOF() {
  const a = [];
  for (let i = 0; i < 100000; i++) a.push(i);
  let s = 0;
  for (let r = 0; r < 3; r++) s += a.map(x => x * 3).filter(x => x % 2 === 0).reduce((p, x) => p + (x % 1000), 0);
  return s;
}

export function stringBuild() {
  let s = "";
  for (let i = 0; i < 100000; i++) s += String.fromCharCode(97 + (i % 26));
  const parts = [];
  for (let i = 0; i < 50000; i++) parts.push("item" + i);
  const joined = parts.join(",");
  let h = 0;
  for (let i = 0; i < s.length; i += 7) h = (h * 31 + s.charCodeAt(i)) % 1000003;
  return h + joined.length;
}

const WORDS = ["alpha", "beta", "gamma", "delta", "lighthouse", "Seagull", "rocks", "dusk"];
const TEXT = (() => { const out = []; for (let i = 0; i < 4000; i++) out.push(WORDS[(i * 7 + (i >> 3)) % WORDS.length]); return out.join(" "); })();
export function stringOps() {
  let n = 0;
  for (let r = 0; r < 10; r++) {
    n += TEXT.split(" ").length;
    n += TEXT.toUpperCase().indexOf("SEAGULL ROCKS");
    n += TEXT.split("gamma").join("GAMMA").length;
    n += TEXT.slice(100, 200).trim().length;
    n += TEXT.lastIndexOf("dusk") + (TEXT.includes("delta alpha") ? 1 : 0);
  }
  return n;
}

const LOG = (() => { const out = []; for (let i = 0; i < 2000; i++) out.push("2026-10-0" + (i % 9 + 1) + " 12:" + String(i % 60).padStart(2, "0") + ":00 user" + i + "@example.com GET /api/v1/items/" + i + "?q=" + (i * 7) + " 200 " + (i * 13 % 997) + "ms"); return out.join("\n"); })();
export function regexScan() {
  const re = /(\w+)@(\w+)\.com GET (\/[\w\/]+)\?q=(\d+) (\d{3}) (\d+)ms/g;
  let n = 0;
  for (let r = 0; r < 3; r++) {
    re.lastIndex = 0;
    let m;
    while ((m = re.exec(LOG)) !== null) n += m[1].length + Number(m[6]);
  }
  n += LOG.replace(/\d+/g, "#").length;
  n += LOG.split(/\s+/).length;
  return n;
}

const BIG = (() => { const items = []; for (let i = 0; i < 4000; i++) items.push({ id: "item-" + i, index: i, ratio: i / 8, active: i % 3 === 0, tags: ["alpha", "beta", "t" + (i % 5)], nested: { url: "https://cdn.example.com/v/" + i + ".mp4", size: "1280x720", meta: { a: 1, b: null, c: "日本語" } } }); return JSON.stringify({ items }); })();
export function jsonBig() { const o = JSON.parse(BIG); return o.items.length + JSON.stringify(o).length; }

const BIGOBJ = JSON.parse(BIG);
function walk(v) {
  if (Array.isArray(v)) { let n = v.length; for (const x of v) n += walk(x); return n; }
  if (v !== null && typeof v === "object") { let n = 0; for (const k of Object.keys(v)) n += k.length + walk(v[k]); return n; }
  if (typeof v === "string") return v.length;
  if (typeof v === "number") return 1;
  return 0;
}
export function walkDoc() { return walk(BIGOBJ); }

export function mapSet() {
  const m = new Map(), st = new Set();
  for (let i = 0; i < 100000; i++) { m.set("k" + i, i); st.add((i * 7) % 50021); }
  let s = 0;
  for (let i = 0; i < 100000; i += 3) s += m.get("k" + i);
  for (const [k, v] of m) if (v % 1000 === 0) s += k.length;
  return s + st.size;
}

export function tryCatch() {
  let n = 0;
  for (let i = 0; i < 20000; i++) {
    try { if (i % 3 === 0) throw new Error("bad " + i); n += 1; } catch (e) { n += e.message.length; }
  }
  return n;
}

export function bigint() {
  let a = 0n, b = 1n;
  for (let i = 0; i < 3000; i++) { const t = a + b; a = b; b = t; }
  let f = 1n;
  for (let i = 1n; i <= 400n; i++) f *= i;
  return (b % 1000000007n).toString() + ":" + f.toString().length;
}

const SOLAR_MASS = 4 * Math.PI * Math.PI, DAYS_PER_YEAR = 365.24;
function nbodySystem() {
  const raw = [
    [0, 0, 0, 0, 0, 0, 1],
    [4.84143144246472090e+00, -1.16032004402742839e+00, -1.03622044471123109e-01, 1.66007664274403694e-03, 7.69901118419740425e-03, -6.90460016972063023e-05, 9.54791938424326609e-04],
    [8.34336671824457987e+00, 4.12479856412430479e+00, -4.03523417114321381e-01, -2.76742510726862411e-03, 4.99852801234917238e-03, 2.30417297573763929e-05, 2.85885980666130812e-04],
    [1.28943695621391310e+01, -1.51111514016986312e+01, -2.23307578892655734e-01, 2.96460137564761618e-03, 2.37847173959480950e-03, -2.96589568540237556e-05, 4.36624404335156298e-05],
    [1.53796971148509165e+01, -2.59193146099879641e+01, 1.79258772950371181e-01, 2.68067772490389322e-03, 1.62824170038242295e-03, -9.51592254519715870e-05, 5.15138902046611451e-05],
  ];
  return raw.map(r => ({ x: r[0], y: r[1], z: r[2], vx: r[3] * DAYS_PER_YEAR, vy: r[4] * DAYS_PER_YEAR, vz: r[5] * DAYS_PER_YEAR, mass: r[6] * SOLAR_MASS }));
}
export function nbody() {
  const bs = nbodySystem(), n = bs.length, dt = 0.01;
  let px = 0, py = 0, pz = 0;
  for (const b of bs) { px += b.vx * b.mass; py += b.vy * b.mass; pz += b.vz * b.mass; }
  bs[0].vx = -px / SOLAR_MASS; bs[0].vy = -py / SOLAR_MASS; bs[0].vz = -pz / SOLAR_MASS;
  for (let step = 0; step < 10000; step++) {
    for (let i = 0; i < n; i++) {
      const bi = bs[i];
      for (let j = i + 1; j < n; j++) {
        const bj = bs[j], dx = bi.x - bj.x, dy = bi.y - bj.y, dz = bi.z - bj.z;
        const d2 = dx * dx + dy * dy + dz * dz, mag = dt / (d2 * Math.sqrt(d2));
        bi.vx -= dx * bj.mass * mag; bi.vy -= dy * bj.mass * mag; bi.vz -= dz * bj.mass * mag;
        bj.vx += dx * bi.mass * mag; bj.vy += dy * bi.mass * mag; bj.vz += dz * bi.mass * mag;
      }
    }
    for (const b of bs) { b.x += dt * b.vx; b.y += dt * b.vy; b.z += dt * b.vz; }
  }
  let e = 0;
  for (let i = 0; i < n; i++) {
    const bi = bs[i];
    e += 0.5 * bi.mass * (bi.vx * bi.vx + bi.vy * bi.vy + bi.vz * bi.vz);
    for (let j = i + 1; j < n; j++) { const bj = bs[j], dx = bi.x - bj.x, dy = bi.y - bj.y, dz = bi.z - bj.z; e -= bi.mass * bj.mass / Math.sqrt(dx * dx + dy * dy + dz * dz); }
  }
  return e;
}
`

var computeNames = []string{"fibRec", "intLoop", "floatLoop", "sieve", "matmul", "sortCmp", "binaryTrees", "classMethods", "arrayHOF", "stringBuild", "stringOps", "regexScan", "jsonBig", "walkDoc", "mapSet", "tryCatch", "bigint", "nbody"}

// callFloorModule measures what one call costs apart from the work inside it.
const callFloorModule = `
export function noop() { return 1; }
export function echo(v) { return v; }
export function count(v) { return v.items.length; }
`

func instantiateSource(tb testing.TB, e engines.Engine, name, source string) engines.Runtime {
	mod, err := e.Compile(name, source)
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

func checksum(tb testing.TB, out any) string {
	n, err := Normalize(out)
	if err != nil {
		tb.Fatal(err)
	}
	return fmt.Sprint(n)
}

// processCPU is the user+system CPU time of the whole process, so it includes
// the Go collector's background workers that wall time on one goroutine hides.
func processCPU() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// TestComputeAgree runs every kernel once on every engine and requires one
// checksum per kernel, so BenchmarkCompute times identical work.
func TestComputeAgree(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the compute kernels on every engine")
	}
	want := map[string]string{}
	first := map[string]string{}
	for _, e := range benchEngines() {
		rt := instantiateSource(t, e, "compute.js", computeModule)
		for _, name := range computeNames {
			out, err := rt.Call(name, nil)
			if err != nil {
				t.Errorf("%s/%s: %v", name, e.Name(), err)
				continue
			}
			got := checksum(t, out)
			if w, ok := want[name]; !ok {
				want[name], first[name] = got, e.Name()
			} else if got != w {
				t.Errorf("%s: %s returns %s, %s returned %s", name, e.Name(), got, first[name], w)
			}
		}
		rt.Close()
	}
}

// BenchmarkCompute times each kernel per engine (<kernel>/<engine>) and
// validates the checksum on every iteration. cpu-ns/op is the process CPU per
// call: above ns/op when the Go collector runs on other cores.
func BenchmarkCompute(b *testing.B) {
	for _, name := range computeNames {
		for _, e := range benchEngines() {
			b.Run(name+"/"+e.Name(), func(b *testing.B) {
				rt := instantiateSource(b, e, "compute.js", computeModule)
				defer rt.Close()
				first, err := rt.Call(name, nil)
				if err != nil {
					b.Fatal(err)
				}
				want := checksum(b, first)
				b.ReportAllocs()
				b.ResetTimer()
				cpu := processCPU()
				for range b.N {
					out, err := rt.Call(name, nil)
					if err != nil {
						b.Fatal(err)
					}
					if got := checksum(b, out); got != want {
						b.Fatalf("checksum %s, want %s", got, want)
					}
				}
				b.ReportMetric(float64(processCPU()-cpu)/float64(b.N), "cpu-ns/op")
			})
		}
	}
}

// BenchmarkCallFloor times calls that do almost nothing, so the cost is the
// engine boundary: noop() with no arguments, a ~200-byte object in and out,
// the ~10 KiB micro document in with only items.length read, and the same
// document in and out.
func BenchmarkCallFloor(b *testing.B) {
	small := map[string]any{"model": "wan2.5-t2i", "prompt": "a lighthouse at dusk", "size": "1280*720", "n": 1, "seed": 42, "extra": map[string]any{"watermark": false, "tags": []any{"a", "b", "c"}}}
	var doc any
	if err := json.Unmarshal([]byte(microJSON), &doc); err != nil {
		b.Fatal(err)
	}
	cases := []struct {
		name, export string
		args         []any
	}{
		{"noop", "noop", nil},
		{"echoSmall", "echo", []any{small}},
		{"count10KiB", "count", []any{doc}},
		{"echo10KiB", "echo", []any{doc}},
	}
	for _, c := range cases {
		for _, e := range benchEngines() {
			b.Run(c.name+"/"+e.Name(), func(b *testing.B) {
				rt := instantiateSource(b, e, "floor.js", callFloorModule)
				defer rt.Close()
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if _, err := rt.Call(c.export, nil, c.args...); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
