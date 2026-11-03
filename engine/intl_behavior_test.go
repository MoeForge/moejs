package engine

// Intl under load and hostile input: the for-of fast path of segment
// iterators, concurrent use from many runtimes, memory accounting, the cost
// of long tags, locale lists and segments, and the available locales.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIntlNodeProbe runs the script in $MOEJS_INTL_PROBE (a list of probes
// whose completion value is one line per probe, as testdata/intl_probe.js)
// in a mutable and a shared realm and writes the output to
// $MOEJS_INTL_PROBE.moejs.txt, to compare with Node by hand
// (testdata/intl_probe.node.txt is Node 24.22's output for that file,
// intl_probe.moejs.txt moejs's). It is skipped without the
// variable.
func TestIntlNodeProbe(t *testing.T) {
	path := os.Getenv("MOEJS_INTL_PROBE")
	if path == "" {
		t.Skip("MOEJS_INTL_PROBE is not set")
	}
	src, err := os.ReadFile(path)
	require.NoError(t, err)
	var outs []string
	for _, shared := range []bool{false, true} {
		s, err := syntax.ParseScript("probe.js", string(src), syntax.Options{})
		require.NoError(t, err)
		code, err := compiler.CompileScript(s)
		require.NoError(t, err)
		r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
		v, err := r.RunScript(code)
		require.NoError(t, err)
		outs = append(outs, v.String())
	}
	assert.Equal(t, outs[0], outs[1], "shared and mutable realms agree")
	require.NoError(t, os.WriteFile(path+".moejs.txt", []byte(outs[0]), 0o644))
}

// intlEvalBody evaluates body as the body of a function in a fresh realm
// and returns its result as a string, or "!" and the error. The call's jobs
// run before it returns; when they set globalThis.__out, "|" and its
// string follow the result.
func intlEvalBody(t *testing.T, body string, opts RealmOptions) (string, *Realm) {
	t.Helper()
	m, err := syntax.ParseModule("t.js", "export function f() {\n"+body+"\n}\nexport function g() { return globalThis.__out }", syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	r := NewRealmWith(opts)
	env, err := r.EvaluateModule(code)
	require.NoError(t, err)
	fn, _ := env.GetBindingValue("f")
	res, err := r.Call(fn, Undefined(), nil)
	if err != nil {
		return "!" + err.Error(), r
	}
	out := res.String()
	gn, _ := env.GetBindingValue("g")
	if v, err := r.Call(gn, Undefined(), nil); err == nil && !v.IsUndefined() {
		out += "|" + v.String()
	}
	return out, r
}

// intlSlowNext replaces %SegmentIteratorPrototype%.next with a function
// that calls the original, which turns iterStep's fast path off.
const intlSlowNext = `
const __P = Object.getPrototypeOf(new Intl.Segmenter().segment("")[Symbol.iterator]());
const __next = __P.next;
__P.next = function next() { return __next.call(this) };
`

// TestIntlSegmentIteratorFastPathUnobservable runs each case three times: in a
// shared realm and a mutable one (the fast path steps the iterator) and in
// a mutable realm whose next is a wrapper of the original (the iterator
// protocol runs in full). All three must give the same, expected result.
func TestIntlSegmentIteratorFastPathUnobservable(t *testing.T) {
	cases := []struct {
		name, body, want string
		mutableOnly      bool
	}{
		{"break", `const out = []; for (const s of new Intl.Segmenter().segment("abcdef")) { out.push(s.segment); if (s.index === 2) break } return out.join()`, "a,b,c", false},
		{"shared iterator", `const it = new Intl.Segmenter().segment("abcdef")[Symbol.iterator](); const out = []; for (const a of it) { out.push(a.segment); for (const b of it) { out.push("i" + b.segment); break } } return out.join() + JSON.stringify(it.next())`, `a,ib,c,id,e,if{"done":true}`, false},
		{"manual next then for-of", `const it = new Intl.Segmenter().segment("abc")[Symbol.iterator](); it.next(); const out = []; for (const s of it) out.push(s.index); return out.join() + it.next().done`, "1,2true", false},
		{"destructuring", `const [a, , b, ...rest] = new Intl.Segmenter("en", {granularity: "word"}).segment("x y z w"); return [a.segment, b.segment, rest.map(s => s.segment).join("")].join("|")`, "x|y| z w", false},
		{"generator", `function* g(s) { for (const x of s) yield x.segment } function* h(s) { yield* s } const s = new Intl.Segmenter().segment("ab"); const r = h(s).next(); return [...g(s)].join() + "|" + r.value.segment + r.done + Object.keys(r).join()`, "a,b|afalsevalue,done", false},
		{"async body", `async function f(s) { const out = []; for (const x of s) { await null; out.push(x.segment) } return out.join() } f(new Intl.Segmenter().segment("xyz")).then(v => globalThis.__out = v); return "sync"`, "sync|x,y,z", false},
		{"async generator", `async function* ag(s) { for (const x of s) yield x.segment } (async () => { const o = []; for await (const v of ag(new Intl.Segmenter().segment("pq"))) o.push(v); globalThis.__out = o.join() })(); return "sync"`, "sync|p,q", false},
		{"for await over sync", `(async () => { const o = []; for await (const v of new Intl.Segmenter().segment("pq")) o.push(v.segment + v.index); globalThis.__out = o.join() })(); return "sync"`, "sync|p0,q1", false},
		{"Array.from Set spread", `const s = new Intl.Segmenter().segment("aab"); return [Array.from(s, x => x.index).join(), new Set(Array.from(s, x => x.segment)).size, Math.max(...Array.from(s, x => x.index))].join("|")`, "0,1,2|2|2", false},
		{"throw in body", `const out = []; try { for (const s of new Intl.Segmenter().segment("abc")) { out.push(s.segment); throw new Error("x") } } catch (e) { out.push(e.message) } return out.join()`, "a,x", false},
		{"next replaced mid-loop", `const s = new Intl.Segmenter().segment("abcd"), p = Object.getPrototypeOf(s[Symbol.iterator]()); const out = []; for (const x of s) { if (x.index === 1) p.next = function () { return {done: true} }; out.push(x.segment) } return out.join() + "|" + [...s].length`, "a,b,c,d|0", true},
		{"getter on next", `const s = new Intl.Segmenter().segment("abc"), p = Object.getPrototypeOf(s[Symbol.iterator]()), orig = p.next; let n = 0; Object.defineProperty(p, "next", {get() { n++; return orig }, configurable: true}); const out = []; for (const x of s) out.push(x.segment); return out.join() + n`, "a,b,c1", true},
		{"return on break, throw, return, destructuring", `const log = []; Object.getPrototypeOf(Object.getPrototypeOf([].values())).return = function () { log.push("return:" + Object.prototype.toString.call(this)); return {} }; const s = new Intl.Segmenter().segment("abc"); for (const x of s) break; try { for (const x of s) throw 1 } catch {} (() => { for (const x of s) return })(); const [a] = s; for (const x of s) {} return log.join()`, "return:[object Segmenter String Iterator],return:[object Segmenter String Iterator],return:[object Segmenter String Iterator],return:[object Segmenter String Iterator]", true},
		{"return getter throws on break", `Object.defineProperty(Object.getPrototypeOf(Object.getPrototypeOf([].values())), "return", {get() { throw new Error("get return") }, configurable: true}); try { for (const x of new Intl.Segmenter().segment("abc")) break } catch (e) { return e.message } return "no throw"`, "get return", true},
		{"thenables in for await", `const log = []; function then(res) { const self = this; delete Object.prototype.then; log.push(typeof self.segment === "string" ? "seg:" + self.segment : Object.keys(self).join("+")); res(typeof self.segment === "string" ? "T:" + self.segment : self); Object.prototype.then = then } Object.prototype.then = then; (async () => { const o = []; for await (const x of new Intl.Segmenter().segment("ab")) o.push(String(x)); delete Object.prototype.then; globalThis.__out = o.join() + "/" + log.join() })(); return "sync"`, "sync|T:a,T:b/seg:a,value+done,seg:b,value+done,value+done", true},
		{"own next on Segments", `const s = new Intl.Segmenter().segment("abc"), it = s[Symbol.iterator](); s.next = Object.getPrototypeOf(it).next; s[Symbol.iterator] = function () { return this }; try { for (const x of s) return "ran" } catch (e) { return e.name }`, "TypeError", true},
		{"proxy iterator", `const it = new Intl.Segmenter().segment("abc")[Symbol.iterator](); try { for (const x of new Proxy(it, {})) return "ran" } catch (e) { return e.name }`, "TypeError", false},
		{"iterator of another Segments", `const a = new Intl.Segmenter().segment("ab"), b = new Intl.Segmenter("en", {granularity: "word"}).segment("x y"); const ia = a[Symbol.iterator](), ib = b[Symbol.iterator](); const out = []; for (const x of ia) { out.push(x.segment); const y = ib.next(); if (!y.done) out.push(y.value.segment + y.value.isWordLike) } return out.join()`, "a,xtrue,b, false", false},
	}
	if path := os.Getenv("MOEJS_INTL_DUMP"); path != "" {
		var b strings.Builder
		b.WriteString("module.exports = [\n")
		for _, c := range cases {
			fmt.Fprintf(&b, "[%q, %q],\n", c.name, c.body)
		}
		b.WriteString("];\n")
		require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o644))
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !c.mutableOnly {
				got, _ := intlEvalBody(t, c.body, RealmOptions{SharedIntrinsics: true})
				assert.Equal(t, c.want, got, "shared realm")
			}
			got, _ := intlEvalBody(t, c.body, RealmOptions{})
			assert.Equal(t, c.want, got, "mutable realm")
			got, _ = intlEvalBody(t, intlSlowNext+c.body, RealmOptions{})
			assert.Equal(t, c.want, got, "mutable realm, protocol in full")
		})
	}
}

// TestIntlConcurrentFirstUse has many goroutines create shared and mutable
// realms and use Intl.Segmenter, Intl.Locale and Intl.getCanonicalLocales
// in a process that has decoded none of the Intl tables nor built the
// shared Intl group: a child process, so run it with -race. Every realm
// must compute the same result.
func TestIntlConcurrentFirstUse(t *testing.T) {
	if os.Getenv("MOEJS_INTL_CONCURRENT_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestIntlConcurrentFirstUse$", "-test.v", "-test.count=1")
		cmd.Env = append(os.Environ(), "MOEJS_INTL_CONCURRENT_CHILD=1")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "child process failed:\n%s", out)
		t.Logf("child:\n%s", out)
		return
	}
	require.Nil(t, lateCells[lateIntl].Load(), "a fresh process has not built Intl")
	const src = `export function f(variant) {
  const out = [];
  const text = "Hello, wörld! 👨‍👩‍👧‍👦 é 中文字符。Mr. Smith went. Then 3.14 he left? Yes 🇯🇵🇺🇸 can't";
  const order = variant % 2 ? ["sentence", "word", "grapheme"] : ["grapheme", "word", "sentence"];
  const res = {};
  for (const g of order) {
    const segs = new Intl.Segmenter("en", {granularity: g}).segment(text);
    let n = 0;
    for (const x of segs) n += x.index;
    res[g] = [...segs].map(x => x.segment + (x.isWordLike ? "*" : "")).join("|") + "#" + segs.containing(20).index + "#" + n;
  }
  out.push(res.grapheme, res.word, res.sentence);
  const tags = variant % 3 ? ["zh-CN", "sh-u-ca-islamicc", "en-Latn-US"] : ["en-Latn-US", "zh-CN", "sh-u-ca-islamicc"];
  const loc = {};
  for (const t of tags) {
    const l = new Intl.Locale(t);
    loc[t] = [l.toString(), l.maximize().toString(), l.minimize().toString(), l.baseName, l.calendar].join(",");
  }
  out.push(loc["zh-CN"], loc["sh-u-ca-islamicc"], loc["en-Latn-US"]);
  out.push(Intl.getCanonicalLocales(["EN-us", "ru-SU", "art-lojban", "und-Qaai", "en-u-ks-primary-rg-no23"]).join());
  out.push(Intl.Segmenter.supportedLocalesOf(["zxx", "de-CH", "ja"]).join(), new Intl.Segmenter("zh-Hans").resolvedOptions().locale);
  out.push(Object.isFrozen(Intl), Object.isFrozen(Intl.Segmenter.prototype));
  return out.join("\n");
}`
	const goroutines, rounds = 24, 6
	results := make([][]string, goroutines)
	errs := make([]error, goroutines)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			m, err := syntax.ParseModule("intl.js", src, syntax.Options{})
			if err != nil {
				errs[g] = err
				return
			}
			code, err := compiler.CompileModule(m)
			if err != nil {
				errs[g] = err
				return
			}
			for k := range rounds {
				shared := (g+k)%2 == 0
				r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
				env, err := r.EvaluateModule(code)
				if err != nil {
					errs[g] = err
					return
				}
				fn, _ := env.GetBindingValue("f")
				v, err := r.Call(fn, Undefined(), []Value{IntValue(g + k)})
				if err != nil {
					errs[g] = err
					return
				}
				s := v.String()
				// The last line says whether the realm's Intl is frozen.
				want := "false\nfalse"
				if shared {
					want = "true\ntrue"
				}
				if !strings.HasSuffix(s, want) {
					errs[g] = fmt.Errorf("shared=%v: frozen line %q", shared, s[strings.LastIndexByte(s[:len(s)-6], '\n')+1:])
					return
				}
				results[g] = append(results[g], strings.TrimSuffix(s, want))
			}
		}()
	}
	close(start)
	wg.Wait()
	for g := range goroutines {
		require.NoError(t, errs[g], "goroutine %d", g)
		for _, s := range results[g] {
			require.Equal(t, results[0][0], s, "goroutine %d", g)
		}
	}
	assert.Contains(t, results[0][0], "zh-CN,zh-Hans-CN,zh,zh-CN,\n")
	t.Log(results[0][0])
}

// TestIntlRealmIsolation checks that a mutable realm's changes to
// its Intl objects stay in it, and that a shared realm cannot change the
// shared ones.
func TestIntlRealmIsolation(t *testing.T) {
	got, _ := intlEvalBody(t, `Intl.Segmenter.prototype.segment = () => "patched"; Intl.Locale.prototype.maximize = null; Intl.x = 1; return new Intl.Segmenter().segment("a")`, RealmOptions{})
	assert.Equal(t, "patched", got)
	got, _ = intlEvalBody(t, `"use strict"; const r = []; for (const f of [() => { Intl.Segmenter.prototype.segment = 1 }, () => { Intl.x = 1 }, () => { Object.getPrototypeOf(new Intl.Segmenter().segment("")).containing = 1 }, () => { Intl.Locale.prototype.foo = 1 }]) { try { f(); r.push("ok") } catch (e) { r.push(e.name) } } return r.join()`, RealmOptions{SharedIntrinsics: true})
	assert.Equal(t, "TypeError,TypeError,TypeError,TypeError", got)
	for _, shared := range []bool{false, true} {
		got, _ = intlEvalBody(t, `return [typeof new Intl.Segmenter().segment("a"), Intl.x, typeof Intl.Locale.prototype.maximize].join()`, RealmOptions{SharedIntrinsics: shared})
		assert.Equal(t, "object,,function", got, "shared=%v", shared)
	}
}

// intlMemLimitRun runs body with a memory limit and reports whether the
// run stopped at the limit.
func intlMemLimitRun(t *testing.T, limit int64, body string) (hit bool, res string, allocated int64) {
	t.Helper()
	got, r := intlEvalBody(t, body, RealmOptions{SharedIntrinsics: true, MemoryLimit: limit})
	err := r.CheckInterrupt()
	hit = errors.Is(err, ErrMemoryLimit) || strings.Contains(got, "memory limit")
	return hit, got, r.Stats().RequestAllocatedBytes
}

// TestIntlMemoryLimitSegments: segment data objects and their strings
// are charged, so collecting the segments of a long text stops at the
// limit.
func TestIntlMemoryLimitSegments(t *testing.T) {
	for _, g := range []string{"grapheme", "word", "sentence"} {
		hit, res, _ := intlMemLimitRun(t, 32<<20, `const t = "Éa. ".repeat(400000); const keep = []; for (const s of new Intl.Segmenter("en", {granularity: "`+g+`"}).segment(t)) keep.push(s); return keep.length`)
		assert.True(t, hit, "%s: %s", g, res)
		hit, res, _ = intlMemLimitRun(t, 32<<20, `const t = "Éa. ".repeat(400000), s = new Intl.Segmenter("en", {granularity: "`+g+`"}).segment(t); const keep = []; for (let i = 0; i < t.length; i++) keep.push(s.containing(i)); return keep.length`)
		assert.True(t, hit, "containing %s: %s", g, res)
	}
}

// TestIntlMemoryLimitTagStrings: the strings Intl makes from language
// tags (getCanonicalLocales, Intl.Locale and its toString, baseName,
// maximize) must be charged like any other string: a script that keeps
// them must stop at the limit. A tag of ~900 KB in a non-canonical case
// makes a new ~900 KB string on each call.
func TestIntlMemoryLimitTagStrings(t *testing.T) {
	const big = `const BIG = "EN-X-" + "abcdefgh-".repeat(100000) + "z";`
	for name, body := range map[string]string{
		"getCanonicalLocales":          big + `const keep = []; for (let i = 0; i < 150; i++) keep.push(Intl.getCanonicalLocales(BIG)[0]); return keep.length`,
		"Locale":                       big + `const keep = []; for (let i = 0; i < 150; i++) keep.push(new Intl.Locale(BIG)); return keep.length`,
		"Locale maximize":              big + `const l = new Intl.Locale(BIG), keep = []; for (let i = 0; i < 150; i++) keep.push(l.maximize()); return keep.length`,
		"Segmenter supportedLocalesOf": big + `const keep = []; for (let i = 0; i < 80; i++) keep.push(Intl.Segmenter.supportedLocalesOf(BIG)); return keep.length`,
	} {
		t.Run(name, func(t *testing.T) {
			hit, res, allocated := intlMemLimitRun(t, 64<<20, body)
			assert.True(t, hit, "kept 72-135 MB under a 64 MB limit: result %s, charged %d bytes", res, allocated)
		})
	}
}

// TestIntlBaseNameCostsOnlyBaseName: Intl.Locale.prototype.baseName of a
// tag that was not canonical builds the whole tag again and slices it, so
// the short base name keeps the whole tag alive (and costs O(tag) time per
// read). It must cost the base name only.
func TestIntlBaseNameCostsOnlyBaseName(t *testing.T) {
	r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
	big := "EN-X-" + strings.Repeat("abcdefgh-", 100000) + "z"
	tag, ok := parseLangTag(big)
	require.True(t, ok)
	tag.canonicalize()
	l := &intlLocale{tag: tag, str: FromGoString(tag.String())}
	o := r.newObject(ClassIntlLocale, r.rootShapeFor(r.intlIntr().LocalePrototype))
	o.internal = l
	v, err := intlLocaleBaseName(r, ObjectValue(o), nil)
	require.NoError(t, err)
	assert.Equal(t, "en", v.String())
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	const calls = 20
	for range calls {
		_, _ = intlLocaleBaseName(r, ObjectValue(o), nil)
	}
	runtime.ReadMemStats(&after)
	perCall := (after.TotalAlloc - before.TotalAlloc) / calls
	assert.Less(t, perCall, uint64(1024), "baseName of a %d-byte tag allocates %d bytes per read", len(big), perCall)
}

// intlBestTime returns how long f takes, the best of three.
func intlBestTime(f func()) time.Duration {
	best := time.Duration(1 << 62)
	for range 3 {
		start := time.Now()
		f()
		best = min(best, time.Since(start))
	}
	return best
}

// TestIntlTagCostIsLinear: canonicalizing and resolving tags must take
// time linear in their length, and Intl.getCanonicalLocales linear in the
// number of locales. Each input is timed at n and 4n subtags; a ratio near
// 16 is quadratic.
func TestIntlTagCostIsLinear(t *testing.T) {
	if raceEnabled {
		t.Skip("timing")
	}
	type input func(n int) string
	variants := func(n int) string {
		var b strings.Builder
		b.WriteString("en")
		for i := range n {
			fmt.Fprintf(&b, "-v%06d", i)
		}
		return b.String()
	}
	uattrs := func(n int) string {
		var b strings.Builder
		b.WriteString("en-u")
		for i := range n {
			fmt.Fprintf(&b, "-a%06d", n-i)
		}
		return b.String() + "-ca-gregory"
	}
	ukeys := func(n int) string {
		var b strings.Builder
		b.WriteString("en-u")
		for i := range n {
			fmt.Fprintf(&b, "-%c%c-%06d", 'a'+i%26, 'a'+i/26%26, n-i)
		}
		return b.String()
	}
	tfields := func(n int) string {
		var b strings.Builder
		b.WriteString("en-t-de")
		for i := range n {
			fmt.Fprintf(&b, "-%c%d-%06d", 'a'+i%26, i%10, n-i)
		}
		return b.String()
	}
	private := func(n int) string { return "EN-X-" + strings.Repeat("a-", n) + "b" }
	for name, in := range map[string]input{"variants": variants, "u attributes": uattrs, "u keywords": ukeys, "t fields": tfields, "private use": private} {
		t.Run(name, func(t *testing.T) {
			const n = 20000
			small, large := in(n), in(4*n)
			canon := func(s string) func() {
				return func() {
					if _, ok := canonicalizeLangTag(s); !ok {
						t.Fatalf("%.40s... is not valid", s)
					}
				}
			}
			ts, tl := intlBestTime(canon(small)), intlBestTime(canon(large))
			t.Logf("canonicalize: %v at n, %v at 4n (x%.1f)", ts, tl, float64(tl)/float64(ts))
			assert.Less(t, float64(tl)/float64(ts), 8.0, "canonicalize is superlinear")
			lookup := func(s string) func() {
				c, _ := canonicalizeLangTag(s)
				r := NewRealm()
				return func() { r.intlResolveLocale([]string{c}) }
			}
			ts, tl = intlBestTime(lookup(small)), intlBestTime(lookup(large))
			t.Logf("resolve: %v at n, %v at 4n (x%.1f)", ts, tl, float64(tl)/float64(ts))
			assert.Less(t, float64(tl)/float64(ts), 8.0, "locale lookup is superlinear")
		})
	}
	t.Run("locale list", func(t *testing.T) {
		run := func(n int) time.Duration {
			body := fmt.Sprintf(`const a = []; for (let i = 0; i < %d; i++) a.push("en-x-" + i.toString(36).padStart(4, "0")); const t0 = Date.now(); Intl.getCanonicalLocales(a); return Date.now() - t0`, n)
			got, _ := intlEvalBody(t, body, RealmOptions{SharedIntrinsics: true})
			var ms int
			_, err := fmt.Sscan(got, &ms)
			require.NoError(t, err, got)
			return time.Duration(ms) * time.Millisecond
		}
		ts, tl := run(10000), run(40000)
		t.Logf("getCanonicalLocales: %v at 10k locales, %v at 40k", ts, tl)
		assert.Less(t, tl, 200*time.Millisecond, "40k distinct locales take %v", tl)
	})
}

// TestIntlContainingLongSegment: one containing() call in a single word
// of n characters scans the word (linear); this measures it and the cost of
// containing() at every index, which is quadratic. The test checks that a
// single call on 1M characters stays under 50 ms.
func TestIntlContainingLongSegment(t *testing.T) {
	if raceEnabled {
		t.Skip("timing")
	}
	for _, g := range []segGranularity{segGrapheme, segWord, segSentence} {
		u := make([]uint16, 1<<20)
		for i := range u {
			u[i] = 'a'
			if g == segGrapheme {
				u[i] = 0x0301 // one cluster of combining marks
			}
		}
		u[0] = 'a'
		text := &segText{u: u}
		d := intlBestTime(func() {
			if s := segStart(g, text, len(u)-1); s != 0 {
				t.Fatalf("%s: start %d", segNames[g], s)
			}
		})
		t.Logf("%s: containing at the end of a 1M-unit segment: %v", segNames[g], d)
		assert.Less(t, d, 50*time.Millisecond)
	}
}

// TestIntlAvailableLanguageRegion: ECMA-402 §9.1 requires [[AvailableLocales]]
// to accompany each language-script-region element with the
// language-region element ("If [[AvailableLocales]] contains "az-Latn-AZ",
// then it must also contain "az-AZ""). moejs has zh-Hant-TW but not zh-TW,
// so new Intl.Segmenter("zh-TW") resolves to "zh" where Node keeps "zh-TW".
func TestIntlAvailableLanguageRegion(t *testing.T) {
	var missing []string
	for rec := range strings.SplitSeq(ltAvailableLocales, ";") {
		lang, list, _ := strings.Cut(rec, ":")
		for rest := range strings.FieldsSeq(list) {
			script, region, ok := strings.Cut(rest, "-")
			if !ok || len(script) != 4 {
				continue
			}
			if want := lang + "-" + region; !langTagAvailable(want) {
				missing = append(missing, want+" (for "+lang+"-"+rest+")")
			}
		}
	}
	assert.Empty(t, missing, "%d language-region locales missing", len(missing))
	for _, shared := range []bool{false, true} {
		got, _ := intlEvalBody(t, `return ["zh-TW", "zh-CN", "zh-HK", "sr-RS", "pa-PK", "az-AZ", "uz-UZ", "sr-BA", "shi-MA"].map(l => new Intl.Segmenter(l).resolvedOptions().locale).join()`, RealmOptions{SharedIntrinsics: shared})
		assert.Equal(t, "zh-TW,zh-CN,zh-HK,sr-RS,pa-PK,az-AZ,uz-UZ,sr-BA,shi-MA", got, "Node 24 resolves them to themselves")
	}
}

// intlLocaleFuzz is the realm and function of FuzzIntlLocale, made
// once per fuzz worker.
var intlLocaleFuzz struct {
	sync.Mutex
	r  *Realm
	fn Value
}

// FuzzIntlLocale fuzzes the Intl.Locale constructor with its options
// (UpdateLanguageId, the keyword options, MakeLocaleRecord), which
// FuzzLangTag does not reach: no panic, only RangeError or TypeError, a
// canonical result that parses back to itself with the same getters, and
// maximize, minimize, getCanonicalLocales and Intl.Segmenter accepting it.
func FuzzIntlLocale(f *testing.F) {
	for _, s := range [][3]string{
		{"en-US", "fr|Latn|CA|fonipa|gregory|phonebk|1|h23|upper|x|arab", "\xff\x07"},
		{"zh-Hant-TW-u-ca-chinese-co-pinyin", "sh||SU|hepburn-heploc|islamicc|dictionary|7|h11|false||traditional", "\xaa\x05"},
		{"art-lojban", "und|Qaai|DD|1994-biske-rozaj|ethiopic-amete-alem|true|0|h24|lower|1|true", "\x55\x07"},
		{"ja-Latn-hepburn-u-kn-false", "JA|LATN|jp|HEPBURN-HEPLOC|GREGORY|yes|SUN|h12|FALSE||latn", "\x00\x01"},
		{"sgn-GR-u-sd-grcrete-rg-no23", "hy|Armn|SU|arevela|buddhist|search|mon|h23|upper|0|", "\xff\xff"},
		{"en-t-iw-m0-names-x-u-ca", "cnr|||polytoni|islamic-umalqura|eor|8|h25|true||thai", "\x1f\x03"},
		{"und-Zzzz-ZZ-u-va-posix", "zh|Zzzz|ZZ|||||||", "\x07\x00"},
	} {
		f.Add(s[0], s[1], uint16(s[2][0])|uint16(s[2][1])<<8)
	}
	f.Fuzz(func(t *testing.T, tag, opts string, which uint16) {
		if len(tag) > 200 || len(opts) > 400 {
			return
		}
		st := &intlLocaleFuzz
		st.Lock()
		defer st.Unlock()
		if st.r == nil {
			m, err := syntax.ParseModule("fuzz.js", `export function f(tag, opts, which) {
  const keys = ["language", "script", "region", "variants", "calendar", "collation", "firstDayOfWeek", "hourCycle", "caseFirst", "numeric", "numberingSystem"];
  const vals = opts.split("|"), o = {};
  for (let i = 0; i < keys.length; i++) if (which & (1 << i)) o[keys[i]] = i === 9 ? vals[i] === "1" : vals[i];
  let l;
  try { l = new Intl.Locale(tag, o) } catch (e) { if (e instanceof RangeError || e instanceof TypeError) return "err"; throw e }
  const s = l.toString(), again = new Intl.Locale(s);
  if (again.toString() !== s) throw new Error("not canonical: " + s + " -> " + again);
  if (new Intl.Locale(l).toString() !== s || Intl.getCanonicalLocales(s)[0] !== s) throw new Error("copy of " + s);
  for (const k of ["baseName", "language", "script", "region", "variants", "numeric"]) if (l[k] !== again[k]) throw new Error(k + ": " + l[k] + " vs " + again[k] + " for " + s);
  for (const k of ["calendar", "collation", "firstDayOfWeek", "hourCycle", "caseFirst", "numberingSystem"]) if (l[k] !== again[k] && l[k] !== "true" && l[k] !== "") throw new Error(k + ": " + l[k] + " vs " + again[k] + " for " + s);
  for (const m of [l.maximize(), l.minimize()]) if (new Intl.Locale(m.toString()).toString() !== m.toString()) throw new Error("maximize/minimize of " + s + ": " + m);
  new Intl.Segmenter(s).resolvedOptions();
  Intl.Segmenter.supportedLocalesOf([s, tag].filter(x => { try { Intl.getCanonicalLocales(x); return true } catch { return false } }));
  return s;
}`, syntax.Options{})
			require.NoError(t, err)
			code, err := compiler.CompileModule(m)
			require.NoError(t, err)
			st.r = NewRealmWith(RealmOptions{SharedIntrinsics: true})
			env, err := st.r.EvaluateModule(code)
			require.NoError(t, err)
			st.fn, _ = env.GetBindingValue("f")
		}
		if _, err := st.r.Call(st.fn, Undefined(), []Value{StringValue(FromGoString(tag)), StringValue(FromGoString(opts)), IntValue(int(which))}); err != nil {
			t.Fatalf("new Intl.Locale(%q, %q / %#x): %v", tag, opts, which, err)
		}
	})
}

// TestIntlUnusedDecodesNothing checks, in a fresh process, that realms that
// never name Intl build nothing of it and decode no table, that defining
// Intl decodes no table either, and that the tables decode on first use.
func TestIntlUnusedDecodesNothing(t *testing.T) {
	if os.Getenv("MOEJS_INTL_UNUSED_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestIntlUnusedDecodesNothing$", "-test.v", "-test.count=1")
		cmd.Env = append(os.Environ(), "MOEJS_INTL_UNUSED_CHILD=1")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "child process failed:\n%s", out)
		return
	}
	decoded := func() string {
		var d []string
		for name, ok := range map[string]bool{
			"grapheme": segGraphemeTab.data != nil, "word": segWordTab.data != nil, "sentence": segSentenceTab.data != nil,
			"likely": ltLikelyData != nil, "available": ltAvailableData != nil,
		} {
			if ok {
				d = append(d, name)
			}
		}
		return strings.Join(d, ",")
	}
	for _, shared := range []bool{false, true} {
		got, r := intlEvalBody(t, `const o = {}; for (const k in {a: 1}) o[k] = 1; return [typeof Map, JSON.stringify([1, "a"]), typeof globalThis.Intlx, "abc".localeCompare("abd"), (1234.5).toLocaleString(), new Date(0).toLocaleString("en-US", {timeZone: "UTC"}).length > 0].join()`, RealmOptions{SharedIntrinsics: shared})
		assert.Equal(t, "function,[1,\"a\"],undefined,-1,1,234.5,true", got)
		assert.NotZero(t, r.coldGlobals&(1<<lateIntl), "shared=%v: Intl is still pending", shared)
	}
	assert.Nil(t, lateCells[lateIntl].Load(), "no shared Intl group")
	assert.Empty(t, decoded())
	got, _ := intlEvalBody(t, `return [typeof Intl, typeof Intl.Segmenter, new Intl.Segmenter().resolvedOptions().locale, [...new Intl.Segmenter().segment("ab\r\nc")].length].join()`, RealmOptions{SharedIntrinsics: true})
	assert.Equal(t, "object,function,en-US,4", got)
	assert.Empty(t, decoded(), "defining Intl and segmenting ASCII graphemes decodes nothing")
	got, _ = intlEvalBody(t, `return [[...new Intl.Segmenter("en", {granularity: "word"}).segment("a b")].length, new Intl.Locale("en").maximize().toString()].join()`, RealmOptions{})
	assert.Equal(t, "3,en-Latn-US", got)
	assert.Equal(t, "available,likely,word", func() string { d := strings.Split(decoded(), ","); slices.Sort(d); return strings.Join(d, ",") }())
}

// TestIntlInterruptLongTag: resolving a locale walks the tag's prefixes
// one subtag at a time and scans CLDR's list for the language at each, about
// 1.6 µs per subtag with no interrupt check, so a 10 MB tag of 1.4 million
// variants keeps an interrupted realm busy for seconds. Every available
// locale has at most a few subtags, so the walk could start there.
func TestIntlInterruptLongTag(t *testing.T) {
	if raceEnabled {
		t.Skip("timing")
	}
	var b strings.Builder
	b.WriteString("en")
	for i := range 1400000 {
		fmt.Fprintf(&b, "-v%06d", i)
	}
	tag := StringValue(FromGoString(b.String()))
	for _, src := range []string{`t => new Intl.Segmenter(t)`, `t => Intl.Segmenter.supportedLocalesOf(t)`} {
		t.Run(src, func(t *testing.T) {
			m, err := syntax.ParseModule("t.js", "export const f = "+src, syntax.Options{})
			require.NoError(t, err)
			code, err := compiler.CompileModule(m)
			require.NoError(t, err)
			r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
			env, err := r.EvaluateModule(code)
			require.NoError(t, err)
			fn, _ := env.GetBindingValue("f")
			timer := time.AfterFunc(50*time.Millisecond, func() { r.Interrupt(errors.New("timeout")) })
			defer timer.Stop()
			start := time.Now()
			_, _ = r.Call(fn, Undefined(), []Value{tag})
			d := time.Since(start)
			t.Logf("%s on a %d-byte tag returned after %v", src, b.Len(), d)
			assert.Less(t, d, time.Second, "an interrupt at 50 ms takes effect after %v", d)
		})
	}
}

// TestIntlContainingEveryIndex: containing() at every index of one
// 20,000-character word (or sentence, or cluster) costs a scan back to the
// segment's start and one forward to its end at each index. On the VM moejs
// took 5.6 s for 20k indices and 22.4 s for 40k (word), Node 24 92 ms and
// 372 ms: ICU keeps the boundaries around the last position. Remembering the
// last segment found (and stepping forward from it) would make a cursor
// that walks the text cost a step per index.
func TestIntlContainingEveryIndex(t *testing.T) {
	if raceEnabled {
		t.Skip("timing")
	}
	got, _ := intlEvalBody(t, `const out = []; for (const g of ["word", "sentence", "grapheme"]) { const s = new Intl.Segmenter("en", {granularity: g}).segment(g === "grapheme" ? "a" + "́".repeat(20000) : "a".repeat(20000)); const t0 = Date.now(); for (let i = 0; i < 20000; i++) s.containing(i); out.push(Date.now() - t0) } return out.join()`, RealmOptions{SharedIntrinsics: true})
	var w, s, g int
	_, err := fmt.Sscanf(got, "%d,%d,%d", &w, &s, &g)
	require.NoError(t, err, got)
	t.Logf("containing at every index of 20k units: word %d ms, sentence %d ms, grapheme %d ms (Node 24: 92, 90, 99)", w, s, g)
	for name, ms := range map[string]int{"word": w, "sentence": s, "grapheme": g} {
		assert.Less(t, ms, 500, "%s: %d ms", name, ms)
	}
}

// TestIntlToLocaleUsesIntrinsics: the toLocale methods construct the
// intrinsic %Intl.NumberFormat% / %Intl.DateTimeFormat% (ECMA-402 §20),
// so replacing or deleting the globals and patching their prototypes in a
// mutable realm changes nothing, and Array.prototype.toLocaleString passes
// locales and options (two arguments) to every element.
func TestIntlToLocaleUsesIntrinsics(t *testing.T) {
	r := NewRealmWith(RealmOptions{TimeZone: time.UTC})
	got := evalIn(t, r, `const NF = Intl.NumberFormat, DTF = Intl.DateTimeFormat;
Intl.NumberFormat = function () { throw new Error("patched") };
Intl.DateTimeFormat = function () { throw new Error("patched") };
Object.defineProperty(NF.prototype, "format", {get() { throw new Error("patched format") }});
DTF.prototype.formatToParts = () => { throw new Error("patched parts") };
delete globalThis.Intl;
return [(1234.5).toLocaleString("de"), (12n).toLocaleString("ja", {style: "currency", currency: "JPY"}), new Date(0).toLocaleString("ko"),
  [1234.5, new Date(0), 5n, null, {toLocaleString(...a) { return a.length + typeof a[0] }}].toLocaleString("de"),
  new Float64Array([1.5]).toLocaleString("fr")].join(" | ")`)
	assert.Equal(t, "1.234,5 | ￥12 | 1970. 1. 1. 오전 12:00:00 | 1.234,5,1.1.1970, 00:00:00,5,,2string | 1,5", got)
}

// TestIntlHostileDecimalsLinear: digit strings of millions of digits,
// exponents of a million, 10^100000 as a BigInt and their ranges and parts
// format in linear time (each well under a second, without the race
// detector).
func TestIntlHostileDecimalsLinear(t *testing.T) {
	r := NewRealmWith(RealmOptions{})
	for _, c := range []struct{ body, want string }{
		{`return nf.format(big)`, "∞"},
		{`return nf.format("0." + big)`, "1"},
		{`return nf.format("1e-1000000") + nf.format("1e1000000") + nf.format("-1e-1000000")`, "0∞-0"},
		{`return nf.format(10n ** 100000n - 1n).length`, "133333"},
		{`return nf.formatToParts(10n ** 100000n - 1n).length`, "66667"},
		{`return nf.formatRange("-" + big, big)`, "-∞ – ∞"},
		{`return nf.formatRange(-(10n ** 100000n), 10n ** 100000n).length`, "266672"},
		{`return pr.select(big) + pr.selectRange(big, big + "1")`, "otherother"},
		{`return nf.format(" ".repeat(1000000) + "1" + " ".repeat(1000000))`, "1"},
		{`return nf.format("0x" + "f".repeat(2000000))`, "∞"},
		{`return new Intl.NumberFormat("en", {notation: "engineering", maximumFractionDigits: 100}).format("1e-320")`, "10E-321"},
	} {
		start := time.Now()
		got := evalIn(t, r, `const big = "9".repeat(2000000); const nf = new Intl.NumberFormat("en", {maximumFractionDigits: 100}); const pr = new Intl.PluralRules("en", {maximumFractionDigits: 100});`+c.body)
		assert.Equal(t, c.want, got, c.body)
		if !raceEnabled {
			assert.Less(t, time.Since(start), time.Second, c.body)
		}
	}
}

// TestIntlFormattersConcurrentFirstUse: in a fresh process, 24 goroutines
// with shared and mutable realms format in every data locale and some
// regional ones, load every zone in rotated orders and fill the shared
// pattern caches past their bound at once (run with -race); every goroutine
// gets the same text.
func TestIntlFormattersConcurrentFirstUse(t *testing.T) {
	if os.Getenv("MOEJS_INTL_CONC_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestIntlFormattersConcurrentFirstUse$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), "MOEJS_INTL_CONC_CHILD=1")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "child:\n%s", out)
		return
	}
	const src = `export function f(g) {
  const locales = ["en", "zh-Hans", "zh-Hant", "ja", "ko", "de", "fr", "es", "pt", "ru", "it", "en-GB", "zh-HK", "de-CH", "pt-PT"];
  const zones = Intl.supportedValuesOf("timeZone");
  const rot = (a, k) => a.slice(k % a.length).concat(a.slice(0, k % a.length));
  const out = [];
  const d = Date.UTC(2026, 2, 29, 1, 30, 15, 250);
  for (const l of rot(locales, g)) {
    out.push(l,
      new Intl.NumberFormat(l, {style: "currency", currency: "JPY", currencySign: "accounting"}).format(-1234567.891),
      new Intl.NumberFormat(l, {notation: "compact", compactDisplay: "long"}).format(123456789),
      new Intl.NumberFormat(l, {style: "unit", unit: "liter-per-kilometer", unitDisplay: "long"}).formatRange(1, 2),
      new Intl.PluralRules(l, {type: "ordinal"}).select(22) + new Intl.PluralRules(l).selectRange(1, 5),
      new Intl.DateTimeFormat(l, {dateStyle: "full", timeStyle: "full", timeZone: "Australia/Lord_Howe"}).format(d),
      new Intl.DateTimeFormat(l, {month: "short", day: "numeric", hour: "numeric", timeZone: "UTC"}).formatRange(d, d + 3 * 86400e3),
      (1234.5).toLocaleString(l), (12n ** 20n).toLocaleString(l), new Date(d).toLocaleString(l, {timeZone: "Asia/Kolkata"}));
  }
  for (const z of rot(zones, g * 37)) out.push(new Intl.DateTimeFormat("en", {timeZone: z, timeZoneName: "longGeneric", hour: "numeric"}).format(d));
  // More distinct patterns than the caches hold, from every goroutine.
  const fields = [["weekday", ["narrow", "short", "long"]], ["era", ["short", "long"]], ["year", ["numeric", "2-digit"]], ["month", ["numeric", "2-digit", "short", "long", "narrow"]], ["day", ["numeric", "2-digit"]], ["hour", ["numeric", "2-digit"]], ["minute", ["numeric", "2-digit"]], ["second", ["numeric"]], ["timeZoneName", ["short", "long", "shortOffset"]]];
  let h = 0;
  for (let i = 0; i < 1200; i++) {
    const o = {timeZone: "America/New_York"};
    let k = (i * 7919 + g * 104729) % 1000003;
    for (const [name, vals] of fields) { const c = k % (vals.length + 1); k = Math.floor(k / (vals.length + 1)); if (c) o[name] = vals[c - 1] }
    const s = new Intl.DateTimeFormat(["en", "ja", "de"][i % 3], o).formatRange(d, d + i * 3600e3);
    for (let j = 0; j < s.length; j++) h = (h * 31 + s.charCodeAt(j)) | 0;
  }
  out.push("patterns " + h);
  return out.join("\n");
}`
	m, err := syntax.ParseModule("conc.js", src, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	const goroutines = 24
	results := make([]string, goroutines)
	errs := make([]error, goroutines)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r := NewRealmWith(RealmOptions{SharedIntrinsics: g%2 == 0, TimeZone: time.UTC, Locale: []string{"", "ja-JP", "de"}[g%3]})
			env, err := r.EvaluateModule(code)
			if err != nil {
				errs[g] = err
				return
			}
			fn, _ := env.GetBindingValue("f")
			v, err := r.Call(fn, Undefined(), []Value{IntValue(g % 8)})
			if err != nil {
				errs[g] = err
				return
			}
			results[g] = v.String()
		}()
	}
	close(start)
	wg.Wait()
	for g := range goroutines {
		require.NoError(t, errs[g], "goroutine %d", g)
	}
	// Goroutines with the same rotation and pattern seed (g mod 8) agree
	// line for line; across rotations the sorted lines agree.
	for g := 8; g < goroutines; g++ {
		require.Equal(t, results[g%8], results[g], "goroutine %d", g)
	}
	sorted := func(s string) string {
		l := strings.Split(s, "\n")
		l = l[:len(l)-1] // the patterns hash depends on g
		for i := 1; i < len(l); i++ {
			for j := i; j > 0 && l[j] < l[j-1]; j-- {
				l[j], l[j-1] = l[j-1], l[j]
			}
		}
		return strings.Join(l, "\n")
	}
	want := sorted(results[0])
	for g := 1; g < 8; g++ {
		require.Equal(t, want, sorted(results[g]), "goroutine %d", g)
	}
	t.Logf("%d lines per goroutine", strings.Count(results[0], "\n")+1)
}

// intlAvailableDataTags returns every tag of the NumberFormat/DateTimeFormat
// available locales.
func intlAvailableDataTags() []string {
	var tags []string
	for rec := range strings.SplitSeq(intlDataAvailable, ";") {
		if tag, _, ok := strings.Cut(rec, ">"); ok {
			tags = append(tags, tag)
		}
	}
	return tags
}

const intlRegionalSrc = `export function f(tags, g) {
  const d = Date.UTC(2026, 9, 11, 14, 5, 9), out = [];
  const k = g % tags.length, order = tags.slice(k).concat(tags.slice(0, k));
  for (const l of order) {
    const c = new Intl.NumberFormat(l, {style: "currency", currency: "EUR", currencyDisplay: "name"});
    out.push(l + " " + [new Intl.NumberFormat(l).format(-1234567.891), c.format(2.5), new Intl.NumberFormat(l, {style: "currency", currency: "USD"}).format(-1234.5),
      new Intl.NumberFormat(l, {notation: "compact", compactDisplay: "long"}).format(123456789), new Intl.NumberFormat(l, {style: "unit", unit: "liter", unitDisplay: "long"}).format(2),
      new Intl.DateTimeFormat(l, {dateStyle: "full", timeStyle: "full", timeZone: "America/Sao_Paulo"}).format(d),
      new Intl.DateTimeFormat(l, {month: "short", day: "numeric", hour: "numeric", timeZone: "Asia/Tokyo", timeZoneName: "longGeneric"}).formatRange(d, d + 5 * 86400e3),
      new Intl.PluralRules(l).select(1) + new Intl.PluralRules(l, {type: "ordinal"}).select(2),
      new Intl.DateTimeFormat(l).resolvedOptions().locale].join(" | "));
  }
  return out.sort().join("\n");
}`

// TestIntlRegionalConcurrentFirstUse: the delta records of all
// 198 regional data indexes are first built by 24 goroutines at once
// (shared and mutable realms, rotated orders) in one process, and one by
// one in another; both processes print the same text (run with -race).
func TestIntlRegionalConcurrentFirstUse(t *testing.T) {
	if mode := os.Getenv("MOEJS_INTL_REGIONAL_CHILD"); mode != "" {
		m, err := syntax.ParseModule("regional.js", intlRegionalSrc, syntax.Options{})
		require.NoError(t, err)
		code, err := compiler.CompileModule(m)
		require.NoError(t, err)
		tags := intlAvailableDataTags()
		n := 1
		if mode == "conc" {
			n = 24
		}
		results := make([]string, n)
		errs := make([]error, n)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for g := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				r := NewRealmWith(RealmOptions{SharedIntrinsics: g%2 == 0, TimeZone: time.UTC})
				env, err := r.EvaluateModule(code)
				if err != nil {
					errs[g] = err
					return
				}
				fn, _ := env.GetBindingValue("f")
				local := make([]Value, len(tags))
				for i, tag := range tags {
					local[i] = StringValue(FromGoString(tag))
				}
				v, err := r.Call(fn, Undefined(), []Value{ObjectValue(r.NewArrayFromSlice(local)), IntValue(g * 37)})
				if err != nil {
					errs[g] = err
					return
				}
				results[g] = v.String()
			}()
		}
		close(start)
		wg.Wait()
		for g := range n {
			require.NoError(t, errs[g])
			require.Equal(t, results[0], results[g], "goroutine %d", g)
		}
		require.NoError(t, os.WriteFile(os.Getenv("MOEJS_INTL_REGIONAL_OUT"), []byte(results[0]), 0o644))
		return
	}
	dir := t.TempDir()
	out := map[string]string{}
	for _, mode := range []string{"seq", "conc"} {
		path := dir + "/" + mode + ".txt"
		cmd := exec.Command(os.Args[0], "-test.run=^TestIntlRegionalConcurrentFirstUse$", "-test.count=1")
		cmd.Env = append(os.Environ(), "MOEJS_INTL_REGIONAL_CHILD="+mode, "MOEJS_INTL_REGIONAL_OUT="+path)
		b, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s child:\n%s", mode, b)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		out[mode] = string(data)
	}
	require.Equal(t, out["seq"], out["conc"])
	lines := strings.Split(out["seq"], "\n")
	assert.Equal(t, len(intlAvailableDataTags()), len(lines))
	t.Logf("%d locales, e.g. %s", len(lines), lines[len(lines)/2])
}

// intlDataIndexTags returns one available tag per data index.
func intlDataIndexTags() []string {
	seen := map[string]bool{}
	var tags []string
	for rec := range strings.SplitSeq(intlDataAvailable, ";") {
		if tag, idx, ok := strings.Cut(rec, ">"); ok && !seen[idx] {
			seen[idx] = true
			tags = append(tags, tag)
		}
	}
	return tags
}

// TestIntlPatternCachesProcessBound: the pattern, compiled-pattern and
// interval caches are process-wide and charged to no realm, so they share
// one budget over all the data indexes: scripts that each stay within a
// 32 MB MemoryLimit (fresh runtimes, as successive requests) in every data
// index keep the process well under 64 MB. What does not fit the budget is
// the formatter's own, charged to its realm (about 3 KB a formatter with a
// range), so a request takes 10 locales of 700 option sets. Run in a child
// process for a clean heap.
func TestIntlPatternCachesProcessBound(t *testing.T) {
	if raceEnabled || testing.Short() {
		t.Skip("about 12 s of formatting")
	}
	if os.Getenv("MOEJS_INTL_CACHE_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestIntlPatternCachesProcessBound$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), "MOEJS_INTL_CACHE_CHILD=1")
		out, err := cmd.CombinedOutput()
		t.Logf("child:\n%s", out)
		require.NoError(t, err)
		return
	}
	const src = `export function f(list) {
  const fields = [["weekday", ["narrow", "short", "long"]], ["era", ["short", "long"]], ["year", ["numeric", "2-digit"]], ["month", ["numeric", "2-digit", "short", "long", "narrow"]], ["day", ["numeric", "2-digit"]], ["hour", ["numeric", "2-digit"]], ["minute", ["numeric", "2-digit"]], ["second", ["numeric"]], ["timeZoneName", ["short", "long", "shortOffset"]], ["hourCycle", ["h11", "h23"]]];
  const d = Date.UTC(2026, 9, 11, 14, 5, 9);
  for (const l of list) for (let i = 0; i < 700; i++) {
    const o = {timeZone: "UTC"}; let k = i * 7919 % 9999991;
    for (const [name, vals] of fields) { const c = k % (vals.length + 1); k = Math.floor(k / (vals.length + 1)); if (c) o[name] = vals[c - 1] }
    try { new Intl.DateTimeFormat(l, o).formatRange(d, d + i * 3600e3) } catch (e) { if (!(e instanceof RangeError || e instanceof TypeError)) throw e }
  }
}`
	m, err := syntax.ParseModule("cache.js", src, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	heap := func() uint64 {
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return ms.HeapAlloc
	}
	before := heap()
	tags := intlDataIndexTags()
	for start := 0; start < len(tags); start += 10 {
		r := NewRealmWith(RealmOptions{MemoryLimit: 32 << 20, TimeZone: time.UTC})
		env, err := r.EvaluateModule(code)
		require.NoError(t, err)
		fn, _ := env.GetBindingValue("f")
		var list []Value
		for _, tag := range tags[start:min(start+10, len(tags))] {
			list = append(list, StringValue(FromGoString(tag)))
		}
		_, err = r.Call(fn, Undefined(), []Value{ObjectValue(r.NewArrayFromSlice(list))})
		require.NoError(t, err, "each request stays within its 32 MB limit")
	}
	grown := int64(heap()-before) >> 20
	t.Logf("%d data indexes: the process keeps %d MB more", len(tags), grown)
	assert.Less(t, grown, int64(64), "requests within their limit grow the process-wide caches without a process-wide bound")
}

// intlCacheHeld sums what the caches of every distinct decoded data index
// hold, estimated as intlCacheTake's callers estimate it.
func intlCacheHeld() (bytes int64, entries int) {
	seen := map[*intlDateData]bool{}
	for i := range intlDateTables {
		d := intlDateTables[i]
		if d == nil || seen[d] {
			continue
		}
		seen[d] = true
		d.patterns.Range(func(k, v any) bool {
			key := k.(intlPatternKey)
			bytes += int64(int(unsafe.Sizeof(key)) + len(key.skeleton) + len(key.decimal) + len(v.(string)) + intlDateCacheEntry)
			entries++
			return true
		})
		d.compiled.Range(func(k, v any) bool {
			items := v.([]intlDateItem)
			n := len(k.(string)) + len(items)*int(unsafe.Sizeof(intlDateItem{}))
			for _, it := range items {
				n += len(it.lit)
			}
			bytes += int64(n + intlDateCacheEntry)
			entries++
			return true
		})
		d.ranges.Range(func(k, v any) bool {
			key, g := k.(intlRangeKey), v.(*intlDateRange)
			n := int(unsafe.Sizeof(key)) + len(key.skeleton) + len(key.decimal) + int(unsafe.Sizeof(intlDateRange{})) + len(g.datePattern) + len(g.timePattern)
			items := len(g.dateItems) + len(g.timeItems)
			for _, p := range g.patterns {
				n += len(p.first) + len(p.second)
				items += len(p.firstItems) + len(p.secondItems)
			}
			bytes += int64(n + items*int(unsafe.Sizeof(intlDateItem{})) + intlDateCacheEntry)
			entries++
			return true
		})
	}
	return bytes, entries
}

// TestIntlPatternCacheDuplicates: goroutines that miss the same key at
// once each take budget for it, but the map keeps one entry, so all but
// the one stored give it back: 16 goroutines building the same 600
// formatters in one locale spend about what the caches hold, not several
// times it (which would leave every later pattern of the process
// uncached).
func TestIntlPatternCacheDuplicates(t *testing.T) {
	if os.Getenv("MOEJS_INTL_DUP_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestIntlPatternCacheDuplicates$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), "MOEJS_INTL_DUP_CHILD=1")
		out, err := cmd.CombinedOutput()
		t.Logf("child:\n%s", out)
		require.NoError(t, err)
		return
	}
	const src = `export function f() {
  const fields = [["weekday", ["narrow", "short", "long"]], ["era", ["short", "long"]], ["year", ["numeric", "2-digit"]], ["month", ["numeric", "2-digit", "short", "long", "narrow"]], ["day", ["numeric", "2-digit"]], ["hour", ["numeric", "2-digit"]], ["minute", ["numeric", "2-digit"]], ["second", ["numeric"]], ["timeZoneName", ["short", "long", "shortOffset"]], ["hourCycle", ["h11", "h23"]]];
  const d = Date.UTC(2026, 9, 11, 14, 5, 9);
  for (let i = 0; i < 600; i++) {
    const o = {timeZone: "UTC"}; let k = i * 7919 % 9999991;
    for (const [name, vals] of fields) { const c = k % (vals.length + 1); k = Math.floor(k / (vals.length + 1)); if (c) o[name] = vals[c - 1] }
    try { new Intl.DateTimeFormat("fr-CA", o).formatRange(d, d + i * 3600e3) } catch (e) { if (!(e instanceof RangeError || e instanceof TypeError)) throw e }
  }
}`
	m, err := syntax.ParseModule("dup.js", src, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := NewRealmWith(RealmOptions{TimeZone: time.UTC})
			env, err := r.EvaluateModule(code)
			if err != nil {
				return
			}
			fn, _ := env.GetBindingValue("f")
			<-start
			r.Call(fn, Undefined(), nil)
		}()
	}
	close(start)
	wg.Wait()
	held, entries := intlCacheHeld()
	used := intlDateCacheUsed.Load()
	t.Logf("caches hold %d entries, %d bytes; the budget counter took %d (%.1fx)", entries, held, used, float64(used)/float64(held))
	assert.LessOrEqual(t, used, 2*held+1<<16, "the budget is spent on duplicate entries")
}
