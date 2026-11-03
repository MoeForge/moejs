package engine

// Audit tests for the compiler / bytecode / interpreter contract. Every
// semantic claim is a JS program run end-to-end through
// syntax -> compiler -> engine. Programs export `r` (the observed result,
// usually a JSON string); the expected value is what the ECMAScript spec
// gives. Tests that demonstrate a bug are kept but start with t.Skip so the
// suite stays green; remove the Skip to see the failure.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// auditCompilerPrelude is prepended to every module: a log array and a helper that
// turns thrown values into "Name: message" strings.
const auditCompilerPrelude = `
const log = [];
function tryRun(f) { try { return f() } catch (e) { return String(e) } }
function J(v) { return JSON.stringify(v) }
`

// auditCompilerRun parses, compiles and evaluates src (module goal) in a fresh
// realm and returns ToGo(export r) or a non-empty error string:
// "compile: <msg>" for parse/compile errors, "Name: message" for thrown
// values.
func auditCompilerRun(t *testing.T, src string) (any, string) {
	t.Helper()
	m, err := syntax.ParseModule("audit.js", auditCompilerPrelude+src, syntax.Options{})
	if err != nil {
		return nil, "compile: " + err.Error()
	}
	code, err := compiler.CompileModule(m)
	if err != nil {
		return nil, "compile: " + err.Error()
	}
	r := NewRealm()
	env, err := r.EvaluateModule(code)
	if err != nil {
		var exc *Exception
		if errors.As(err, &exc) {
			return nil, errorDisplayString(exc.Value)
		}
		return nil, "go: " + err.Error()
	}
	v, ok := env.GetBindingValue("r")
	if !ok {
		return nil, "no export r"
	}
	return r.ToGo(v), ""
}

// auditCompilerCase is one JS program with its spec-mandated result. want is
// compared against ToGo(r); wantErr (when set) is a substring of the error
// string instead.
type auditCompilerCase struct {
	name    string
	src     string
	want    any
	wantErr string
}

func runAuditCompilerCases(t *testing.T, cases []auditCompilerCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, errStr := auditCompilerRun(t, c.src)
			if c.wantErr != "" {
				// wantErr matches the compile/runtime error string, or a string
				// result produced by tryRun (the caught error rendered by String(e)).
				if s, ok := got.(string); ok && errStr == "" && strings.Contains(s, c.wantErr) {
					return
				}
				if !strings.Contains(errStr, c.wantErr) {
					t.Fatalf("want error containing %q, got value %#v error %q", c.wantErr, got, errStr)
				}
				return
			}
			if errStr != "" {
				t.Fatalf("unexpected error %q", errStr)
			}
			assert.Equal(t, c.want, got)
		})
	}
}

// --- 1. TDZ --------------------------------------------------------------------

func TestAuditCompilerTDZ(t *testing.T) {
	runAuditCompilerCases(t, []auditCompilerCase{
		{"let-before-init-same-scope", `export const r = tryRun(() => { x; let x = 1; });`,
			"ReferenceError: Cannot access 'x' before initialization", ""},
		{"closure-called-before-init", `export const r = tryRun(() => f()); let x = 1; function f() { return x }`,
			"ReferenceError: Cannot access 'x' before initialization", ""},
		{"for-let-body-closure-before-init", `let r0; for (let i = 0; i < 1; i++) { const g = () => j; r0 = tryRun(g); let j = 1; } export const r = r0;`,
			"ReferenceError: Cannot access 'j' before initialization", ""},
		{"typeof-in-tdz-throws", `export const r = tryRun(() => { typeof x; let x; });`,
			"ReferenceError: Cannot access 'x' before initialization", ""},
		{"switch-case-boundary", `export const r = tryRun(() => { switch (1) { case 0: let x = 1; case 1: return x; } });`,
			"ReferenceError: Cannot access 'x' before initialization", ""},
		{"switch-const-case-boundary-captured", `export const r = tryRun(() => { switch (1) { case 0: const x = 1; case 1: return (() => x)(); } });`,
			"ReferenceError: Cannot access 'x' before initialization", ""},
		{"let-x-eq-x", `export const r = tryRun(() => { let x = x; });`,
			"ReferenceError: Cannot access 'x' before initialization", ""},
		{"module-hoisted-fn-before-let", `function f() { return x } export const r = tryRun(f); let x;`,
			"ReferenceError: Cannot access 'x' before initialization", ""},
		{"export-let-undefined", `export let a; export const r = J([a, typeof a]);`, `[null,"undefined"]`, ""},
		{"let-reinit-each-iteration", `for (let i = 0; i < 2; i++) { let z; if (i === 0) z = 5; log.push(z === undefined ? 'u' : z) } export const r = J(log);`, `[5,"u"]`, ""},
		{"block-after-continue", `for (let i = 0; i < 2; i++) { { if (i === 0) continue; let y = i; log.push(y); } } export const r = J(log);`, `[1]`, ""},
		{"assign-before-let", `export const r = tryRun(() => { x = 1; let x; });`,
			"ReferenceError: Cannot access 'x' before initialization", ""},
		{"const-in-nested-block-closure", `export const r = tryRun(() => { const g = () => { return y }; { g(); } const y = 1; });`,
			"ReferenceError: Cannot access 'y' before initialization", ""},
		{"catch-param-not-tdz", `export const r = tryRun(() => { try { throw 1 } catch (e) { return e } });`, int64(1), ""},
	})
}

// TestAuditCompilerTDZExportedHoleNote records (does not assert) what
// ModuleEnv.GetBindingValue returns for an export whose initializer never
// ran because the module threw first.
func TestAuditCompilerTDZExportedHoleNote(t *testing.T) {
	m, err := syntax.ParseModule("audit.js", `export function g() {} throw new Error('boom'); export let a = 1;`, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	r := NewRealm()
	env, err := r.EvaluateModule(code)
	require.Error(t, err)
	v, ok := env.GetBindingValue("a")
	t.Logf("GetBindingValue(a) after module threw before init (no TDZ marker needed): ok=%v isHole=%v type=%v", ok, v.IsHole(), v.Type())
	// Variant where the scope pass marks the binding NeedsTDZ (a hoisted
	// function reading it is referenced before its initializer), so the slot
	// holds the Hole marker when the module throws.
	m, err = syntax.ParseModule("audit.js", `export function g() { return b } g; throw new Error('boom'); export let b = 1;`, syntax.Options{})
	require.NoError(t, err)
	code, err = compiler.CompileModule(m)
	require.NoError(t, err)
	env, err = NewRealm().EvaluateModule(code)
	require.Error(t, err)
	v, ok = env.GetBindingValue("b")
	t.Logf("GetBindingValue(b) after module threw before init (NeedsTDZ binding): ok=%v isHole=%v type=%v", ok, v.IsHole(), v.Type())
}

// TestAuditCompilerTDZForOfPerIteration: spec creates a fresh TDZ environment
// per for-of iteration (ForIn/OfBodyEvaluation step 1.k.iii), so a pattern
// default that reads a later element of the same pattern must throw on every
// iteration, not only the first. moejs writes the TDZ marker once at loop
// entry and CopyEnv/registers carry the previous iteration's value.
func TestAuditCompilerTDZForOfPerIteration(t *testing.T) {
	got, errStr := auditCompilerRun(t, `
let seen = [];
try { for (const [b = a, a] of [[0, 1], [undefined, 2]]) { seen.push(b) } } catch (e) { seen.push(String(e)) }
export const r = J(seen);`)
	require.Empty(t, errStr)
	assert.Equal(t, `[0,"ReferenceError: Cannot access 'a' before initialization"]`, got)
	// for-in too, and a closure-captured loop binding (environment slots).
	got, errStr = auditCompilerRun(t, `
let seen = [];
try { for (const [b = a, a] in {xy: 1, zw: 2}) { seen.push(b) } } catch (e) { seen.push(String(e)) }
try { for (const [b = a, a] of [[0, 1], [undefined, 2]]) { seen.push(() => b) } } catch (e) { seen.push(String(e)) }
export const r = J(seen.map(x => typeof x === "function" ? x() : x));`)
	require.Empty(t, errStr)
	// for-in destructures the key strings: "xy" gives b = "x" and no default
	// read, so no TDZ error there.
	assert.Equal(t, `["x","z",0,"ReferenceError: Cannot access 'a' before initialization"]`, got)
	// Loops over existing bindings (no declaration, no loop scope) and var
	// declarations are unaffected.
	got, errStr = auditCompilerRun(t, `
let x, s = 0, o = {}; for (x of [1, 2]) s += x; for (o.k in {a: 1}) s += o.k.length; for (var v of [3]) s += v;
export const r = J([s, x, o.k, v]);`)
	require.Empty(t, errStr)
	assert.Equal(t, `[7,2,"a",3]`, got)
}

// --- 2. const assignment --------------------------------------------------------

func TestAuditCompilerConst(t *testing.T) {
	const ca = "TypeError: Assignment to constant variable."
	runAuditCompilerCases(t, []auditCompilerCase{
		{"plain", `const x = 1; export const r = tryRun(() => { x = 2 });`, ca, ""},
		{"rhs-evaluated-first", `const x = 1; try { x = (log.push(1), 2) } catch (e) { log.push(String(e)) } export const r = J(log);`, `[1,"` + ca + `"]`, ""},
		{"postfix-inc", `const x = 1; export const r = tryRun(() => { x++ });`, ca, ""},
		{"prefix-dec", `const x = 1; export const r = tryRun(() => { --x });`, ca, ""},
		{"compound", `const x = 1; export const r = tryRun(() => { x += 1 });`, ca, ""},
		{"array-pattern", `const x = 1; export const r = tryRun(() => { [x] = [1] });`, ca, ""},
		{"array-pattern-default", `const x = 1; export const r = tryRun(() => { [x = 5] = [] });`, ca, ""},
		{"object-pattern", `const x = 1; export const r = tryRun(() => { ({x} = {x: 1}) });`, ca, ""},
		{"for-of-const-assign", `export const r = tryRun(() => { for (const x of [1, 2]) x = 3 });`, ca, ""},
		{"for-in-const-ok", `export const r = tryRun(() => { let n = 0; for (const x in {a: 1, b: 2}) n++; return n });`, int64(2), ""},
		{"named-fn-expr-self-assign", `export const r = (function f() { return tryRun(() => { f = 1 }) })();`, ca, ""},
		{"named-fn-expr-self-update", `export const r = (function f() { return tryRun(() => { f++ }) })();`, ca, ""},
		{"for-const-update", `export const r = tryRun(() => { for (const i = 0; i < 1; i++) {} });`, ca, ""},
		{"nullish-assign-nonnullish-no-throw", `const x = 1; export const r = tryRun(() => x ??= 2);`, int64(1), ""},
		{"nullish-assign-null-throws", `const x = null; export const r = tryRun(() => x ??= 2);`, ca, ""},
		{"logical-or-assign-truthy-no-throw", `const x = 1; export const r = tryRun(() => x ||= 2);`, int64(1), ""},
		{"logical-and-assign-falsy-no-throw", `const x = 0; export const r = tryRun(() => x &&= 2);`, int64(0), ""},
		{"captured-const-in-closure", `const x = 1; const g = () => { x = 2 }; export const r = tryRun(g);`, ca, ""},
		{"local-const-in-function", `export const r = tryRun(() => { const y = 1; y = 2 });`, ca, ""},
	})
}

// TestAuditCompilerConstUpdateToNumericOrder: 13.4.2.1 PostfixIncrement
// evaluates ToNumeric(oldValue) (step 2) before PutValue throws (step 4), so
// valueOf must run. moejs emits ThrowConstAssign without ToNumeric.
func TestAuditCompilerConstUpdateToNumericOrder(t *testing.T) {
	got, errStr := auditCompilerRun(t, `
const o = { valueOf() { log.push('v'); return 1 } };
try { o++ } catch (e) { log.push(String(e)) }
export const r = J(log);`)
	require.Empty(t, errStr)
	assert.Equal(t, `["v","TypeError: Assignment to constant variable."]`, got)
	got, errStr = auditCompilerRun(t, `
const o = { valueOf() { log.push('v'); return 1 } };
try { --o } catch (e) { log.push(String(e)) }
export const r = J(log);`)
	require.Empty(t, errStr)
	assert.Equal(t, `["v","TypeError: Assignment to constant variable."]`, got)
}

// --- 3. labelled break/continue and per-iteration bindings -------------------------

func TestAuditCompilerLabels(t *testing.T) {
	runAuditCompilerCases(t, []auditCompilerCase{
		{"continue-outer-for-of", `outer: for (const a of [1, 2]) { for (const b of [1, 2]) { if (b === 2) continue outer; log.push(a + '' + b) } } export const r = J(log);`, `["11","21"]`, ""},
		{"continue-outer-for-in", `outer: for (const k in {a: 1, b: 2}) { for (const v of [1, 2]) { if (v === 2) continue outer; log.push(k + v) } } export const r = J(log);`, `["a1","b1"]`, ""},
		{"continue-outer-while", `let n = 0; w: while (n < 2) { n++; for (const v of [1, 2]) { if (v === 2) continue w; log.push('w' + n + v) } } export const r = J(log);`, `["w11","w21"]`, ""},
		{"continue-outer-do-while", `let m = 0; d: do { m++; for (const v of [1, 2]) { if (v === 2) continue d; log.push('d' + m + v) } } while (m < 2); export const r = J(log);`, `["d11","d21"]`, ""},
		{"continue-outer-for-let-closures", `const fs = []; c: for (let i = 0; i < 2; i++) { for (const v of [1, 2]) { if (v === 2) continue c; fs.push(() => i + '' + v) } } export const r = J(fs.map(f => f()));`, `["01","11"]`, ""},
		{"per-iteration-basic", `const fs = []; for (let i = 0; i < 3; i++) fs.push(() => i); export const r = J(fs.map(f => f()));`, `[0,1,2]`, ""},
		{"per-iteration-with-continue", `const fs = []; for (let i = 0; i < 3; i++) { fs.push(() => i); if (i === 1) continue; } export const r = J(fs.map(f => f()));`, `[0,1,2]`, ""},
		{"per-iteration-with-break", `const fs = []; for (let i = 0; i < 3; i++) { fs.push(() => i); if (i === 1) break; } export const r = J(fs.map(f => f()));`, `[0,1]`, ""},
		{"per-iteration-closure-in-update", `const fs = []; for (let i = 0; i < 3; fs.push(() => i), i++); export const r = J(fs.map(f => f()));`, `[1,2,3]`, ""},
		{"per-iteration-body-increment", `const fs = []; for (let i = 0;;) { fs.push(() => i); i++; if (i > 2) break } export const r = J(fs.map(f => f()));`, `[1,2,3]`, ""},
		{"per-iteration-init-closure", `const fs = []; for (let i = 0, g = () => i; i < 2; i++) { fs.push(g) } fs.push(() => 'x'); export const r = J([fs[0](), fs[1]()]);`, `[0,0]`, ""},
		{"labelled-block", `lbl: { log.push(1); break lbl; log.push(2) } export const r = J(log);`, `[1]`, ""},
		{"break-switch-in-loop", `for (let i = 0; i < 3; i++) { switch (i) { case 1: break; default: log.push(i) } } export const r = J(log);`, `[0,2]`, ""},
		{"break-labelled-loop-from-switch", `outer: for (let i = 0; i < 3; i++) { switch (i) { case 1: break outer; } log.push(i) } export const r = J(log);`, `[0]`, ""},
		{"continue-in-switch-in-loop", `for (let i = 0; i < 3; i++) { switch (i) { case 1: continue; } log.push(i) } export const r = J(log);`, `[0,2]`, ""},
		{"continue-through-finally", `for (let i = 0; i < 2; i++) { try { continue } finally { log.push('f') } } export const r = J(log);`, `["f","f"]`, ""},
		{"break-for-of-through-finally", `for (const x of [1, 2]) { try { break } finally { log.push(x) } } export const r = J(log);`, `[1]`, ""},
		{"labelled-if-break", `lbl: if (true) { log.push('a'); break lbl; } export const r = J(log);`, `["a"]`, ""},
		{"nested-labels-same-loop", `a: b: for (let i = 0; i < 3; i++) { if (i === 1) continue a; if (i === 2) break b; log.push(i) } export const r = J(log);`, `[0]`, ""},
	})
}

// --- 4. try/finally completion routing ---------------------------------------------

func TestAuditCompilerTryFinally(t *testing.T) {
	runAuditCompilerCases(t, []auditCompilerCase{
		{"return-overridden-by-finally-return", `export const r = (() => { try { return 1 } finally { return 2 } })();`, int64(2), ""},
		{"return-with-finally-side-effect", `const v = (() => { try { return 1 } finally { log.push(1) } })(); export const r = J([v, log]);`, `[1,[1]]`, ""},
		{"throw-swallowed-by-finally-return", `export const r = (() => { try { throw 1 } finally { return 2 } })();`, int64(2), ""},
		{"catch-rethrow-finally-runs", `let c; try { try { throw 1 } catch (e) { throw 2 } finally { log.push(3) } } catch (e) { c = e } export const r = J([c, log]);`, `[2,[3]]`, ""},
		{"return-then-finally-throws", `export const r = tryRun(() => { try { return 1 } finally { throw 3 } });`, "3", ""},
		{"break-in-finally-overrides-return", `export const r = (() => { for (;;) { try { return 1 } finally { break } } return 2 })();`, int64(2), ""},
		{"labelled-try-break-runs-finally", `label: try { break label } finally { log.push(1) } export const r = J(log);`, `[1]`, ""},
		{"finally-inner-caught-throw-keeps-return", `const v = (() => { try { return 1 } finally { try { throw 2 } catch (e) { log.push(e) } } })(); export const r = J([v, log]);`, `[1,[2]]`, ""},
		{"try-finally-inside-catch", `try { throw 1 } catch (e) { try { log.push('a') } finally { log.push('b') } } export const r = J(log);`, `["a","b"]`, ""},
		{"continue-in-for-of-through-finally-keeps-iterating", `for (const x of [1, 2, 3]) { try { if (x === 2) continue } finally { log.push(x) } } export const r = J(log);`, `[1,2,3]`, ""},
		{"return-value-not-clobbered-by-finally-call", `function f() { return 'f' } function g() { return 'g' } export const r = (() => { try { return f() } finally { g() } })();`, "f", ""},
		{"nested-3-deep-return-innermost", `const v = (() => { try { try { try { return 1 } finally { log.push(1) } } finally { log.push(2) } } finally { log.push(3) } })(); export const r = J([v, log]);`, `[1,[1,2,3]]`, ""},
		{"throw-in-finally-replaces-throw", `let c; try { try { throw 1 } finally { throw 2 } } catch (e) { c = e } export const r = c;`, int64(2), ""},
		{"throw-in-catch-goes-to-outer", `let c; try { try { throw 1 } catch (e) { throw 2 } } catch (e) { c = e } export const r = c;`, int64(2), ""},
		{"break-in-finally-overrides-throw", `export const r = (() => { for (;;) { try { throw 1 } finally { break } } return 'ok' })();`, "ok", ""},
		{"break-through-inner-finally-inside-outer-try", `try { for (;;) { try { break } finally { log.push('in') } } } finally { log.push('out') } export const r = J(log);`, `["in","out"]`, ""},
		{"return-in-catch-runs-finally", `const v = (() => { try { throw 1 } catch (e) { return 'c' } finally { log.push('f') } })(); export const r = J([v, log]);`, `["c",["f"]]`, ""},
		{"continue-in-finally-overrides-return", `export const r = (() => { for (let i = 0; i < 2; i++) { try { return 'r' + i } finally { if (i === 0) continue } } return 'end' })();`, "r1", ""},
		{"return-in-try-with-block-env-inside", `export const r = (() => { try { let k = 1; const g = () => k; return g() } finally { log.push('f') } })();`, int64(1), ""},
		{"finally-normal-completion-value-preserved", `const v = (() => { try { return 5 } finally { let t = 7; t++ } })(); export const r = v;`, int64(5), ""},
		{"return-from-loop-inside-try-finally-with-envs", `export const r = (() => { try { for (const x of [1, 2]) { const g = () => x; if (x === 2) return g() } } finally { log.push('f') } return 0 })();`, int64(2), ""},
		{"throw-in-catch-param-destructure-goes-to-finally", `let c; try { try { throw null } catch ({message}) { } finally { log.push('f') } } catch (e) { c = String(e).slice(0, 9) } export const r = J([c, log]);`, `["TypeError",["f"]]`, ""},
		{"catch-then-outer-finally-nested-try-in-catch", `try { try { throw 1 } catch (e) { try { throw 2 } catch (e2) { log.push(e2) } } } finally { log.push('f') } export const r = J(log);`, `[2,"f"]`, ""},
		{"finally-normal-after-catch", `try { throw 1 } catch (e) { log.push('c') } finally { log.push('f') } log.push('after'); export const r = J(log);`, `["c","f","after"]`, ""},
		{"return-undefined-through-finally", `const v = (() => { try { return } finally { log.push('f') } })(); export const r = J([v === undefined, log]);`, `[true,["f"]]`, ""},
	})
}

// --- 5. closures and this ------------------------------------------------------------

func TestAuditCompilerClosures(t *testing.T) {
	runAuditCompilerCases(t, []auditCompilerCase{
		{"for-of-fresh-binding", `const fs = []; for (const x of [1, 2]) fs.push(() => x); export const r = J(fs.map(f => f()));`, `[1,2]`, ""},
		{"for-in-fresh-binding", `const fs = []; for (const k in {a: 1, b: 2}) fs.push(() => k); export const r = J(fs.map(f => f()));`, `["a","b"]`, ""},
		{"for-var-shared", `const fs = []; for (var i = 0; i < 2; i++) fs.push(() => i); export const r = J(fs.map(f => f()));`, `[2,2]`, ""},
		{"let-in-block-in-loop", `const fs = []; for (let i = 0; i < 2; i++) { let j = i * 10; fs.push(() => j) } export const r = J(fs.map(f => f()));`, `[0,10]`, ""},
		{"param-captured-then-reassigned", `function f(a) { const g = () => a; a = 2; return g() } export const r = f(1);`, int64(2), ""},
		{"iife", `export const r = (function () { return 5 })();`, int64(5), ""},
		{"named-fn-expr-recursion", `export const r = (function fact(n) { return n <= 1 ? 1 : n * fact(n - 1) })(5);`, int64(120), ""},
		{"arrow-this-module-top", `export const r = (() => this)() === undefined;`, true, ""},
		{"arrow-in-method-this", `const o = { m() { return (() => this)() } }; export const r = o.m() === o;`, true, ""},
		{"plain-fn-this-undefined", `function f() { return this } export const r = f() === undefined;`, true, ""},
		{"method-this", `const o = { m() { return this } }; export const r = o.m() === o;`, true, ""},
		{"extracted-method-loses-this", `const o = { m() { return this.x } }; const m = o.m; export const r = tryRun(() => m());`, nil, "TypeError"},
		{"arrow-ignores-call-this", `const a = () => this; export const r = a.call({}) === undefined && a.apply({}, []) === undefined;`, true, ""},
		{"nested-arrows-3-deep", `const o = { m() { return (() => (() => (() => this)())())() } }; export const r = o.m() === o;`, true, ""},
		{"arrow-in-object-literal-top", `export const r = ({ f: () => this }).f() === undefined;`, true, ""},
		{"new-sets-this", `function F() { this.a = 1 } export const r = new F().a;`, int64(1), ""},
		{"new-arrow-type-error", `const A = () => {}; export const r = tryRun(() => new A());`, nil, "TypeError"},
		{"new-method-type-error", `const o = { m() {} }; export const r = tryRun(() => new o.m());`, nil, "TypeError"},
		{"this-in-method-called-via-call", `const o = { m() { return this } }; const t = {}; export const r = o.m.call(t) === t;`, true, ""},
		{"closure-counter", `function mk() { let n = 0; return () => ++n } const c = mk(); c(); c(); export const r = c();`, int64(3), ""},
		{"closure-captures-catch-param", `let g; try { throw 7 } catch (e) { g = () => e } export const r = g();`, int64(7), ""},
	})
}

// --- 6. destructuring and evaluation order ----------------------------------------------

func TestAuditCompilerDestructuring(t *testing.T) {
	runAuditCompilerCases(t, []auditCompilerCase{
		{"defaults-in-order", `const {a = log.push('a'), b = log.push('b')} = {}; export const r = J(log);`, `["a","b"]`, ""},
		{"default-only-when-undefined", `const {a = 1} = {a: null}; export const r = a;`, nil, ""},
		{"nested-defaults", `const {a: {b = 2} = {}} = {}; export const r = b;`, int64(2), ""},
		{"array-holes", `const [a, , b] = [1, 2, 3]; export const r = J([a, b]);`, `[1,3]`, ""},
		{"array-default-undefined", `const [a = 1] = [undefined]; export const r = a;`, int64(1), ""},
		{"array-rest", `const [a, ...rest] = [1, 2, 3]; export const r = J([a, rest]);`, `[1,[2,3]]`, ""},
		{"object-rest-excludes-and-fresh", `const src = {a: 1, b: 2, c: 3}; const {a, ...rest} = src; export const r = J([a, rest, rest !== src]);`, `[1,{"b":2,"c":3},true]`, ""},
		{"computed-key-before-source-get", `const o = {}; Object.defineProperty(o, 'a', {get() { log.push('get'); return 1 }}); const {[(log.push('k'), 'a')]: v} = o; export const r = J([v, log]);`, `[1,["k","get"]]`, ""},
		{"destructure-null-type-error", `export const r = tryRun(() => { const {a} = null });`, nil, "TypeError"},
		{"destructure-undefined-array-type-error", `export const r = tryRun(() => { const [a] = undefined });`, nil, "TypeError"},
		{"param-defaults-order", `function f({a} = {a: 1}, [b] = [2]) { return [a, b] } export const r = J(f());`, `[1,2]`, ""},
		{"swap", `let a = 1, b = 2; [a, b] = [b, a]; export const r = J([a, b]);`, `[2,1]`, ""},
		{"param-default-refs-earlier", `function f(a, b = a) { return b } export const r = f(1);`, int64(1), ""},
		{"rest-param-length", `function f(...r) {} function g(a, b = 1, c) {} export const r = J([f.length, g.length]);`, `[0,1]`, ""},
		{"arguments-object", `function f(a) { const g = () => arguments; return J([arguments.length, g()[2], a]) } export const r = f(1, 2, 3);`, `[3,3,1]`, ""},
		{"object-pattern-member-target", `const o = {}; ({a: o.x, b: o['y']} = {a: 1, b: 2}); export const r = J(o);`, `{"x":1,"y":2}`, ""},
		{"array-pattern-nested-rest-default", `const [a, [b, ...c] = [9, 8, 7]] = [1]; export const r = J([a, b, c]);`, `[1,9,[8,7]]`, ""},
		{"object-rest-computed-excluded", `const k = 'b'; const {[k]: v, ...rest} = {a: 1, b: 2}; export const r = J([v, rest]);`, `[2,{"a":1}]`, ""},
		{"string-destructure", `const [a, b] = 'xy'; export const r = a + b;`, "xy", ""},
		{"for-of-destructure-entries", `const out = []; for (const [i, v] of ['a', 'b'].entries()) out.push(i + v); export const r = J(out);`, `["0a","1b"]`, ""},
		{"catch-destructure", `let m; try { throw {message: 'm'} } catch ({message}) { m = message } export const r = m;`, "m", ""},
		{"default-fn-name-inference", `const {f = function () {}} = {}; const [g = () => {}] = []; export const r = J([f.name, g.name]);`, `["f","g"]`, ""},
	})
}

// TestAuditCompilerDestructureEmptyPatternNull: 8.6.2 BindingInitialization of
// ObjectBindingPattern `{}` performs RequireObjectCoercible(value) (via
// 14.3.3.1 / 13.15.5.2 for assignment), so `const {} = null` and
// `({} = undefined)` throw TypeError even with no properties. moejs emits no
// instruction for an empty pattern.
func TestAuditCompilerDestructureEmptyPatternNull(t *testing.T) {
	got, errStr := auditCompilerRun(t, `
export const r = J([tryRun(() => { const {} = null; return 'no-throw' }), tryRun(() => { ({} = undefined); return 'no-throw' })]);`)
	require.Empty(t, errStr)
	assert.Contains(t, got, "TypeError")
	assert.NotContains(t, got, "no-throw")
}

// TestAuditCompilerDestructureRestNull: `const {...rest} = null` must throw
// TypeError (RequireObjectCoercible in 14.3.3.1 BindingInitialization);
// moejs's CopyDataPropsEx treats a nullish source as empty (spread
// semantics), so rest destructuring of null yields {}.
func TestAuditCompilerDestructureRestNull(t *testing.T) {
	got, errStr := auditCompilerRun(t, `export const r = tryRun(() => { const {...rest} = null; return J(rest) });`)
	require.Empty(t, errStr)
	assert.Contains(t, got, "TypeError")
	got, errStr = auditCompilerRun(t, `export const r = J([tryRun(() => { const {...rest} = undefined; return 'no-throw' }), tryRun(() => { let rest; ({...rest} = null); return 'no-throw' }), tryRun(() => { const {...rest} = {a: 1}; return rest }), tryRun(() => { const {} = 0; const {...r} = "s"; return [r] })]);`)
	require.Empty(t, errStr)
	assert.Equal(t, `["TypeError: Cannot destructure 'undefined' as it is undefined.","TypeError: Cannot destructure 'null' as it is null.",{"a":1},[{"0":"s"}]]`, got)
}

// TestAuditCompilerDestructureAssignTargetOrder: 13.15.5.6
// IteratorDestructuringAssignmentEvaluation evaluates the target reference
// (lref) before IteratorStep for each element, and 13.15.5.5
// KeyedDestructuringAssignmentEvaluation evaluates lref before GetV. moejs
// reads the source value first, then evaluates the target object.
func TestAuditCompilerDestructureAssignTargetOrder(t *testing.T) {
	got, errStr := auditCompilerRun(t, `
const o = {};
const src = {};
Object.defineProperty(src, 'a', {get() { log.push('get-a'); return 1 }});
({a: (log.push('target-a'), o).x} = src);
export const r = J(log);`)
	require.Empty(t, errStr)
	assert.Equal(t, `["target-a","get-a"]`, got)
	// Computed key first, then the target, then the read; array elements
	// evaluate the target before the iterator step; defaults come after the
	// read; the target's object is captured before a getter reassigns it;
	// rest targets follow the same order.
	got, errStr = auditCompilerRun(t, `
let o = {}, other = {};
const orig = o;
const arr = [0, 0];
Object.defineProperty(arr, 0, {get() { log.push('get0'); return undefined }});
Object.defineProperty(arr, 1, {get() { log.push('get1'); return 2 }});
const src = {};
Object.defineProperty(src, 'k', {get() { log.push('get-k'); o = other; return 1 }});
({[(log.push('key'), 'k')]: (log.push('target'), o).x = (log.push('default'), 9)} = src);
[(log.push('t0'), o).y = (log.push('d0'), 7), (log.push('t1'), o)[(log.push('k1'), 'z')]] = arr;
export const r = J([log, orig.x, orig.y, o === other, other.y, other.z]);`)
	require.Empty(t, errStr)
	assert.Equal(t, `[["key","target","get-k","t0","get0","d0","t1","k1","get1"],1,null,true,7,2]`, got)
	got, errStr = auditCompilerRun(t, `const o = {}; [(log.push('t'), o).rest, ...(log.push('t-rest'), o).more] = [1, 2, 3]; export const r = J([log, o.rest, o.more]);`)
	require.Empty(t, errStr)
	assert.Equal(t, `[["t","t-rest"],1,[2,3]]`, got)
}

// --- 7. template literals --------------------------------------------------------------

func TestAuditCompilerTemplates(t *testing.T) {
	sub300 := "`" + strings.Repeat("${x}", 300) + "`"
	parens200 := strings.Repeat("(", 200) + "1" + strings.Repeat(")", 200)
	tern200 := strings.Repeat("c?", 200) + "1" + strings.Repeat(":0", 200)
	runAuditCompilerCases(t, []auditCompilerCase{
		{"tostring-order", "const a = {toString() { log.push('a'); return 'A' }}, b = {toString() { log.push('b'); return 'B' }}; const s = `${a}${b}`; export const r = J([s, log]);", `["AB",["a","b"]]`, ""},
		{"template-uses-tostring-hint-string", "const o = {toString() { log.push('s'); return 'S' }, valueOf() { log.push('v'); return 'V' }}; const s = `${o}`; export const r = J([s, log]);", `["S",["s"]]`, ""},
		{"plus-uses-default-hint", "const o = {toString() { log.push('s'); return 'S' }, valueOf() { log.push('v'); return 'V' }}; const s = '' + o; export const r = J([s, log]);", `["V",["v"]]`, ""},
		{"bigint-substitution", "export const r = `${1n}`;", "1", ""},
		{"escapes-and-continuation", "export const r = J([`\\u{1F600}`.length, `\\u{1F600}`.codePointAt(0), `a\\\nb`, `\\x41\\u0042`]);", `[2,128512,"ab","AB"]`, ""},
		{"nested-templates", "export const r = `a${`b${1}`}c`;", "ab1c", ""},
		{"300-substitutions", "const x = 'x'; export const r = " + sub300 + ".length;", int64(300), ""},
		{"parens-200-deep", "export const r = " + parens200 + ";", int64(1), ""},
		{"ternary-200-deep", "const c = true; export const r = " + tern200 + ";", int64(1), ""},
		{"empty-and-only-substitution", "const x = 5; export const r = J([``, `${x}`, `${x}${x}`, `${''}`]);", `["","5","55",""]`, ""},
		{"template-null-undefined", "export const r = `${null}${undefined}${true}${[1,2]}${{}}`;", "nullundefinedtrue1,2[object Object]", ""},
		{"folded-string-substitution", "export const r = `a${'b' + 'c'}d${1 + 2}`;", "abcd3", ""},
		{"surrogate-concat-fold", `export const r = J([("\uD83D" + "\uDE00") === "\u{1F600}", ("\uD83D" + "\uDE00").length, ("\uD83D" + "\uDE00").codePointAt(0)]);`, `[true,2,128512]`, ""},
	})
}

// --- 8. optional chaining ----------------------------------------------------------------

func TestAuditCompilerOptionalChaining(t *testing.T) {
	runAuditCompilerCases(t, []auditCompilerCase{
		{"null-short-circuits-chain", `const a = null; export const r = a?.b.c === undefined;`, true, ""},
		{"null-call", `const a = null; export const r = a?.b() === undefined;`, true, ""},
		{"undefined-method-call", `const a = {}; export const r = a.b?.() === undefined;`, true, ""},
		{"computed", `const a = null, k = 'x'; export const r = J([a?.[k], ({x: 1})?.[k]]);`, `[null,1]`, ""},
		{"assign-to-optional-syntax-error", `const a = {}; a?.b = 1;`, nil, "compile:"},
		{"paren-breaks-chain", `const a = null; export const r = tryRun(() => (a?.b).c);`, nil, "TypeError"},
		{"this-of-chained-call", `const a = {b: {m() { return this }}}; export const r = a?.b.m() === a.b;`, true, ""},
		{"optional-call-this", `const o = {f() { return this }}; export const r = o.f?.() === o;`, true, ""},
		{"delete-optional-null", `const a = null; export const r = delete a?.b;`, true, ""},
		{"delete-optional-present", `const a = {b: 1}; const d = delete a?.b; export const r = J([d, 'b' in a]);`, `[true,false]`, ""},
		{"tagged-template-on-chain-syntax-error", "const a = {}; a?.b`x`;", nil, "compile:"},
		{"literal-nullish", `export const r = J([null?.b, undefined?.b]);`, `[null,null]`, ""},
		{"zero-not-nullish", `export const r = (0)?.b === undefined && (0)?.toFixed(1) === '0.0';`, true, ""},
		{"nullish-default", `const x = null; export const r = x?.y ?? 'd';`, "d", ""},
		{"update-on-chain-syntax-error", `const a = {b: 1}; a?.b++;`, nil, "compile:"},
		{"long-chain-middle-null", `const a = {b: null}; export const r = a.b?.c.d.e() === undefined;`, true, ""},
		{"optional-with-args-not-evaluated", `const a = null; a?.f(log.push(1)); export const r = J(log);`, `[]`, ""},
		{"optional-member-of-call-result", `function f() { return null } export const r = f()?.x === undefined;`, true, ""},
		{"optional-in-assignment-rhs-register", `let v = 1; const a = null; v = a?.x; export const r = v === undefined;`, true, ""},
	})
}

// --- 9. spread and holes -------------------------------------------------------------------

func TestAuditCompilerSpread(t *testing.T) {
	big := strings.TrimSuffix(strings.Repeat("1,", 300), ",")
	runAuditCompilerCases(t, []auditCompilerCase{
		{"spread-hole-becomes-undefined", `const a = [...[1, , 3]]; export const r = J([a.length, 1 in a, a[1] === undefined]);`, `[3,true,true]`, ""},
		{"hole-length", `export const r = J([[1, , 3].length, 1 in [1, , 3]]);`, `[3,false]`, ""},
		{"call-spread-hole", `function f(a, b, c) { return [a === undefined, c] } export const r = J(f(...[1, , 3]));`, `[false,3]`, ""},
		{"string-spread-code-points", `export const r = J([...'\u{1F600}a']);`, `["😀","a"]`, ""},
		{"math-max-spread-300", `const arr = [` + big + `]; arr[150] = 7; export const r = Math.max(...arr);`, int64(7), ""},
		{"object-spread-null", `export const r = J({...null, ...undefined});`, `{}`, ""},
		{"object-spread-string", `export const r = J({...'ab'});`, `{"0":"a","1":"b"}`, ""},
		{"object-spread-array", `export const r = J({...[1, 2]});`, `{"0":1,"1":2}`, ""},
		{"spread-duplicate-key-order", `export const r = J([{a: 1, ...{a: 2}, a: 3}.a, {a: 1, ...{a: 2}}.a]);`, `[3,2]`, ""},
		{"computed-after-spread", `const k = 'a'; export const r = J({...{a: 1}, [k]: 2});`, `{"a":2}`, ""},
		{"array-like-not-iterable", `export const r = tryRun(() => [...{length: 1, 0: 'a'}]);`, nil, "TypeError"},
		{"user-iterator-object-not-iterable", `export const r = tryRun(() => [...{next() { return {done: true} }}]);`, nil, "TypeError"},
		{"spread-in-middle", `export const r = J([0, ...[1, 2], 3, ...'ab']);`, `[0,1,2,3,"a","b"]`, ""},
		{"new-spread", `function F(a, b) { this.s = a + b } export const r = new F(...[1, 2]).s;`, int64(3), ""},
		// A computed callee key that is not a local (an upvalue, a module
		// binding, an expression) needs a temporary; the argument block
		// still starts right after the callee and this.
		{"call-spread-captured-key", `const o = {f(a, b) { return a + b }}; function g(k) { return (...args) => o[k](...args) } export const r = g('f')(1, 2);`, int64(3), ""},
		{"call-spread-expr-key", `const o = {f(a, b) { return a + b }}; export const r = o['' + 'f'](...[1, 2], 3);`, int64(3), ""},
		{"call-spread-optional-captured-key", `const o = {f(a) { return a }}; function g(k) { return (...args) => o?.[k](...args) } export const r = g('f')(5);`, int64(5), ""},
		{"call-captured-key-args", `const o = {f(a, b, c) { return J([a, b, c]) }}; function g(k) { return () => o[k](1, 2, 3) } export const r = g('f')();`, `[1,2,3]`, ""},
		{"spread-array-modified-during-spread", `const a = [1, 2]; const o = {toString() { a.push(9); return 'x' }}; export const r = J([...a, o + '']);`, `[1,2,"x"]`, ""},
		{"spread-getter-array-iterator", `export const r = J([...[1, 2].keys(), ...[1, 2].entries()]);`, `[0,1,[0,1],[1,2]]`, ""},
	})
}

// --- 10. misc semantics ----------------------------------------------------------------------

func TestAuditCompilerMiscOperators(t *testing.T) {
	runAuditCompilerCases(t, []auditCompilerCase{
		{"typeof-kinds", `export const r = J([typeof undefined, typeof null, typeof 1, typeof 's', typeof true, typeof {}, typeof [], typeof (() => 1), typeof 1n, typeof undeclaredName]);`,
			`["undefined","object","number","string","boolean","object","object","function","bigint","undefined"]`, ""},
		{"typeof-fused-compare", `let u; export const r = J([typeof u === 'undefined', typeof nope !== 'undefined', typeof 1 == 'number', 'string' === typeof 's', typeof 1n === 'bigint', typeof null === 'object', typeof 1 === 'numbr']);`,
			`[true,false,true,true,true,true,false]`, ""},
		{"instanceof-noncallable-rhs", `export const r = tryRun(() => 1 instanceof {});`, nil, "TypeError"},
		{"instanceof-bound", `function F() {} const B = F.bind(null); export const r = new F() instanceof B;`, true, ""},
		{"in-operator", `export const r = J(['x' in {x: 1}, 1 in [1, 2], 2 in [1, 2], 'length' in []]);`, `[true,true,false,true]`, ""},
		{"in-primitive-rhs", `export const r = tryRun(() => 'a' in 'abc');`, nil, "TypeError"},
		{"delete-returns-true-and-hole", `const o = {x: 1}; const arr = [1, 2]; export const r = J([delete o.x, 'x' in o, delete arr[0], arr.length, 0 in arr]);`, `[true,false,true,2,false]`, ""},
		{"delete-nonconfigurable-strict", `export const r = tryRun(() => { const o = Object.freeze({x: 1}); return delete o.x });`, nil, "TypeError"},
		{"void-and-comma", `let n = 0; export const r = J([void (n = 5), n, (1, 2, 3)]);`, `[null,5,3]`, ""},
		{"logical-values", `export const r = J([0 || 'a', 'b' || 'c', null ?? 'd', 0 ?? 'e', 1 && 2, 0 && 2, '' ?? 'f']);`, `["a","b","d",0,2,0,""]`, ""},
		{"logical-assign-short-circuit-setter", `const o = {}; let calls = 0; Object.defineProperty(o, 'p', {get() { return 1 }, set(v) { calls++ }}); o.p ||= log.push('rhs'); Object.defineProperty(o, 'q', {get() { return 0 }, set(v) { calls++ }}); o.q &&= log.push('rhs2'); o.p ??= log.push('rhs3'); export const r = J([calls, log]);`, `[0,[]]`, ""},
		{"logical-assign-performs-when-needed", `let x = 0; x ||= 5; let y = 1; y &&= 6; let z = null; z ??= 7; export const r = J([x, y, z]);`, `[5,6,7]`, ""},
		{"update-on-string", `let s = '1'; const a = s++; let t = '1'; const b = ++t; export const r = J([a, s, b, t]);`, `[1,2,2,2]`, ""},
		{"unary-numeric", `export const r = J([-'x', +true, 1 / 0, -0 === 0, 1 - -1, 2 ** -1, (-2) ** 2, 2 ** 3 ** 2]);`, `[null,1,null,true,2,0.5,4,512]`, ""},
		{"neg-exp-syntax-error", `export const r = -2 ** 2;`, nil, "compile:"},
		{"modulo", `export const r = J([-5 % 3, 5 % -3, 5.5 % 2, 5 % 0, 0 % 5]);`, `[-2,2,1.5,null,0]`, ""},
		{"shifts", `export const r = J([1 << 31, 1 << 32, -1 >>> 0, 2 ** 32 | 0, ~~3.7, NaN | 0, 1 >> 1, -8 >> 1, -8 >>> 28]);`, `[-2147483648,1,4294967295,0,3,0,0,-4,15]`, ""},
		{"string-arith", `export const r = J(['3' * '4', '3' + 4, [] + {}, [] + [], 1 < '2', 'a' < 'b', '10' < '9', 10 < '9']);`, `[12,"34","[object Object]","",true,true,true,false]`, ""},
		{"comparisons-nullish", `export const r = J([null >= 0, undefined == 0, undefined >= 0, NaN <= NaN, [1, 2] == '1,2', null == 0, null == undefined]);`, `[true,false,false,false,true,false,true]`, ""},
		{"switch-strict-eq", `let hit = 'none'; switch ('1') { case 1: hit = 'num'; break; case '1': hit = 'str'; break; } export const r = hit;`, "str", ""},
		{"switch-fallthrough-default-middle", `function f(v) { const out = []; switch (v) { case 1: out.push(1); default: out.push('d'); case 2: out.push(2); break; case 3: out.push(3); } return out } export const r = J([f(1), f(2), f(3), f(9)]);`, `[[1,"d",2],[2],[3],["d",2]]`, ""},
		{"switch-case-expr-throws", `export const r = tryRun(() => { switch (1) { case (() => { throw new Error('case') })(): return 'x'; } });`, nil, "Error: case"},
		{"switch-case-order-default-middle-tests-after", `function t(v) { log.push(v); return v } switch (2) { case t(1): break; default: log.push('d'); break; case t(2): log.push('two'); break; case t(3): break; } export const r = J(log);`, `[1,2,"two"]`, ""},
		{"do-while-false-once", `let n = 0; do { n++ } while (false); let m = 0; while (0) m++; for (;;) break; export const r = J([n, m]);`, `[1,0]`, ""},
		{"for-in-string", `const ks = []; for (let i in 'ab') ks.push(i); export const r = J(ks);`, `["0","1"]`, ""},
		{"for-in-null", `let n = 0; for (const k in null) n++; for (const k in undefined) n++; export const r = n;`, int64(0), ""},
		{"for-of-string-code-point", `let n = 0; for (const x of '\u{1F600}') n++; export const r = n;`, int64(1), ""},
		{"for-of-array-grows-and-truncates", `const a = [1, 2]; const seen = []; for (const x of a) { seen.push(x); if (x === 1) a.push(3); if (x === 3) a.length = 0 } export const r = J(seen);`, `[1,2,3]`, ""},
		{"for-of-entries-keys", `const out = []; for (const [i, v] of ['a'].entries()) out.push(i, v); for (const k of ['x', 'y'].keys()) out.push(k); export const r = J(out);`, `[0,"a",0,1]`, ""},
		{"comma-in-for", `let a = 0, b = 0; for (let i = 0; i < 3; i++, a++) b += i; export const r = J([a, b]);`, `[3,3]`, ""},
		{"var-hoisting-across-blocks", `function f() { { var v = 1 } return v } export const r = f();`, int64(1), ""},
		{"duplicate-var-and-var-fn", `function f() { var a = 1; var a; function g() { return 1 } var g; return [a, typeof g] } export const r = J(f());`, `[1,"function"]`, ""},
		{"block-fn-strict-scoped", `export const r = tryRun(() => { f(); { function f() {} } });`, nil, "ReferenceError"},
		{"block-fn-hoisted-in-block", `export const r = (() => { { const v = f(); function f() { return 'ok' } return v } })();`, "ok", ""},
		{"exports-live-binding", `export let n = 1; export function inc() { n++ } inc(); export const r = n;`, int64(2), ""},
		{"export-default-expr", `export default 1 + 1; export const r = 1;`, int64(1), ""},
		{"export-as-default", `const a = 1; export { a as default }; export const r = a;`, int64(1), ""},
		{"duplicate-export-syntax-error", `const a = 1; export { a }; export { a as a };`, nil, "compile:"},
		{"export-const-pattern", `const obj = {a: 1, b: 2}; export const {a, b} = obj; export const r = a + b;`, int64(3), ""},
		{"undeclared-assign-strict", `export const r = tryRun(() => { undeclaredZ = 1 });`, nil, "ReferenceError"},
		{"getter-in-literal", `const o = { get x() { return 1 } }; export const r = o.x;`, int64(1), ""},
		{"class-generator", `class A { *g() { yield 1; } } export const r = new A().g().next().value;`, int64(1), ""},
		{"generator", `function* g() { yield* [1, 2]; } export const r = [...g()].length;`, int64(2), ""},
		{"async", `async function f() { return 1; } export const r = typeof f();`, "object", ""},
		{"async-generator", `async function* f() { yield 1; } export const r = Object.prototype.toString.call(f());`, "[object AsyncGenerator]", ""},
		{"await-ident-module", `const await = 1;`, nil, "compile:"},
		{"yield-ident-strict", `const yield = 1;`, nil, "compile:"},
		{"with-unsupported", `with ({}) {}`, nil, "compile:"},
		{"new-function-no-compiler", `export const r = tryRun(() => new Function('return 1'));`, nil, "EvalError"},
		{"dynamic-import-needs-graph", `import('x');`, nil, "uses import() or import.meta must be linked"},
		{"import-meta-needs-graph", `import.meta;`, nil, "uses import() or import.meta must be linked"},
		{"tagged-template", "function f(s, v) { return s.raw[0] + v } export const r = f`x${1}`;", "x1", ""},
		{"labelled-function", `l: function f() {}`, nil, "compile:"},
		{"legacy-octal", `const o = 0777;`, nil, "compile:"},
		{"octal-escape", `const s = "\08";`, nil, "compile:"},
		{"html-comment", "<!-- c\nexport const r = 1;", nil, "compile:"},
		{"debugger-noop", `debugger; export const r = 1;`, int64(1), ""},
		{"new-target", `function f() { return new.target } export const r = J([f() === undefined, new f() === f]);`, `[true,true]`, ""},
		{"super", `const p = { x: 7 }; const o = { __proto__: p, m() { return super.x } }; export const r = o.m();`, int64(7), ""},
		{"private-name", `class A { #p = 1; static g(a) { return a.#p } } export const r = A.g(new A());`, int64(1), ""},
		{"numeric-separators", `export const r = J([1_000, 0x_1 === undefined]);`, nil, "compile:"},
		{"numeric-separator-ok", `export const r = J([1_000, 1_0.5_5, 0xF_F, 1e1_0]);`, `[1000,10.55,255,10000000000]`, ""},
		{"numeric-separator-double", `const n = 1__0;`, nil, "compile:"},
		{"numeric-separator-trailing", `const n = 1_;`, nil, "compile:"},
		{"bigint-basics", `export const r = J([typeof 10n, 10n === 10n, 10n === 10, String(10n), !1n, 0n ? 't' : 'f', 10n == 10]);`, `["bigint",true,false,"10",false,"f",true]`, ""},
		{"bigint-arith", `export const r = tryRun(() => String(1n + 1n));`, "2", ""},
		{"bigint-neg", `export const r = tryRun(() => String(-1n));`, "-1", ""},
		{"bigint-compare", `export const r = tryRun(() => J([1n < 2n, 2n > 1n, 1n < 2]));`, `[true,true,true]`, ""},
		{"bigint-json", `export const r = tryRun(() => JSON.stringify(1n));`, nil, "TypeError"},
		{"bigint-inc", `export const r = tryRun(() => { let b = 1n; b++; return String(b) });`, "2", ""},
		{"direct-eval-no-compiler", `export const r = tryRun(() => eval('1'));`, nil, "EvalError"},
		{"indirect-eval-no-compiler", `const e = eval; export const r = tryRun(() => e('1'));`, nil, "EvalError"},
		{"undefined-shadowed", `export const r = (() => { const undefined = 5; return undefined })();`, int64(5), ""},
		{"NaN-Infinity-fold", `export const r = J([NaN !== NaN, Infinity > 1e308, -Infinity < -1e308, typeof NaN, !NaN, 1 / Infinity]);`, `[true,true,true,"number",true,0]`, ""},
		{"const-fold-div-mod-zero", `export const r = J([1 / 0 === Infinity, -1 / 0 === -Infinity, 0 / 0 !== 0 / 0, 5 % 0 !== 5 % 0, 2 ** 0.5 > 1.41]);`, `[true,true,true,true,true]`, ""},
		{"neg-zero-fold", `export const r = J([Object.is(-0, 0) ? 1 : 1 / (-0) === -Infinity, 1 / -0 === -Infinity, 1 / (0 * -1) === -Infinity]);`, `[true,true,true]`, ""},
	})
}

// TestAuditCompilerRegisterAliasingUpdate checks that operand aliasing of
// register locals respects update expressions in the other operand.
func TestAuditCompilerRegisterAliasing(t *testing.T) {
	runAuditCompilerCases(t, []auditCompilerCase{
		{"x-plus-x-postinc", `export const r = (() => { let x = 1; return x + x++ })();`, int64(2), ""},
		{"x-plus-assign-x", `export const r = (() => { let x = 1; return x + (x = 2) })();`, int64(3), ""},
		{"compound-assign-with-update-rhs", `export const r = (() => { let x = 1; x += x++; return x })();`, int64(2), ""},
		{"call-args-order-with-update", `function f(a, b, c) { return [a, b, c] } export const r = (() => { let x = 1; return J(f(x, x++, x)) })();`, `[1,1,2]`, ""},
		{"array-lit-with-update", `export const r = (() => { let x = 1; return J([x, x++, x]) })();`, `[1,1,2]`, ""},
		{"member-assign-obj-reassigned-in-rhs", `export const r = (() => { let o = {}; const first = o; o.a = (o = {b: 1}, 5); return J([first, o]) })();`, `[{"a":5},{"b":1}]`, ""},
		{"elem-assign-key-updated-in-rhs", `export const r = (() => { let i = 0; const a = []; a[i] = i++; return J([a, i]) })();`, `[[0],1]`, ""},
		{"compare-with-update", `export const r = (() => { let x = 1; return x < x++ })();`, false, ""},
		{"template-with-update", "export const r = (() => { let x = 1; return `${x}${x++}${x}` })();", "112", ""},
		{"assign-into-top-register-call", `function f(a, b) { return a + b } export const r = (() => { let a = 1; let b = f(a, 2); return J([a, b]) })();`, `[1,3]`, ""},
		{"nested-calls-share-block", `function f(a, b) { return a * 10 + b } function g(x) { return x + 1 } export const r = f(g(1), g(2));`, int64(23), ""},
		{"var-init-mentions-itself", `export const r = (() => { let x = 1; { let x = (x, 2) } return x })();`, nil, "ReferenceError"},
		{"switch-disc-copied", `export const r = (() => { let x = 1; switch (x) { case x++: return 'a'; case 1: return 'b'; default: return 'c' + x } })();`, "a", ""},
	})
}

// --- 12. handler table and env unwinding ------------------------------------------------------

func TestAuditCompilerHandlerEnvUnwind(t *testing.T) {
	runAuditCompilerCases(t, []auditCompilerCase{
		// Throws at env depth 1, 2 and 3 inside a try whose own depth is 0;
		// the catch must run in the try statement's environment and the
		// captured values must be intact.
		{"throw-at-nested-env-depths", `
const seen = [];
function probe(depth) {
  let outerCap = 'o'; const fo = () => outerCap;
  try {
    let a = 'a' + depth; const fa = () => a;
    if (depth === 1) throw fa;
    {
      let b = 'b' + depth; const fb = () => b;
      if (depth === 2) throw fb;
      {
        let c = 'c' + depth; const fc = () => c;
        if (depth === 3) throw fc;
      }
    }
  } catch (f) {
    seen.push(f() + fo());
  }
  outerCap = 'z';
  return fo();
}
export const r = J([probe(1), probe(2), probe(3), seen]);`, `["z","z","z",["a1o","b2o","c3o"]]`, ""},
		// Same, but the try itself sits inside two captured blocks so
		// StackDepth is non-zero, and the closure created before the try is
		// read after the catch.
		{"try-inside-captured-blocks", `
let out;
{
  let p = 'p'; const fp = () => p;
  {
    let q = 'q'; const fq = () => q;
    try {
      let x = 'x'; const fx = () => x;
      {
        let y = 'y'; const fy = () => y;
        throw fy;
      }
    } catch (f) {
      out = f() + fp() + fq();
    }
    q = 'Q';
    out += fq();
  }
  out += fp();
}
export const r = out;`, "ypqQp", ""},
		// A throw inside a catch body that itself pushed an env must reach the
		// outer handler with the environments unwound correctly.
		{"throw-in-catch-with-env-reaches-outer", `
let out;
{
  let k = 'k'; const fk = () => k;
  try {
    try { throw 1 } catch (e) { let m = 'm'; const fm = () => m; throw fm }
  } catch (f) {
    out = f() + fk();
  }
  k = 'K';
  out += fk();
}
export const r = out;`, "mkK", ""},
		// Finally reached by throw from a nested env, then a return from
		// inside the finally with its own env.
		{"finally-via-throw-nested-envs", `
function f() {
  let g = 'g'; const fg = () => g;
  try {
    { let h = 'h'; const fh = () => h; throw fh }
  } finally {
    let i = 'i'; const fi = () => i;
    log.push(fg() + fi());
  }
}
export const r = J([tryRun(() => f()), log]);`, `["() => h",["gi"]]`, ""},
		// Loop with captured per-iteration binding, throw on the second
		// iteration, catch outside the loop.
		{"throw-from-for-let-body-env", `
const fs = []; let c;
try {
  for (let i = 0; i < 3; i++) { fs.push(() => i); if (i === 1) throw 'boom' }
} catch (e) { c = e }
export const r = J([c, fs.map(f => f())]);`, `["boom",[0,1]]`, ""},
		// for-of with captured binding inside try/finally; break routes through
		// finally and the loop env is popped exactly once.
		{"break-for-of-env-through-finally", `
const fs = []; let after;
{
  let z = 'z'; const fz = () => z;
  for (const x of [1, 2, 3]) {
    const fx = () => x;
    try { fs.push(fx); if (x === 2) break } finally { log.push(x) }
  }
  after = fz();
}
export const r = J([fs.map(f => f()), log, after]);`, `[[1,2],[1,2],"z"]`, ""},
	})
}

// TestAuditCompilerThrowFromCatchNotSameHandler checks the handler-row
// invariant directly on the compiled table: no HandlerCatch row may cover
// its own handler pc.
func TestAuditCompilerHandlerRowsExcludeOwnCatch(t *testing.T) {
	src := `
export function f(n) {
  try { if (n) throw 1 } catch (e) { try { throw 2 } catch (e2) { throw 3 } finally { n++ } }
  finally { n-- }
  return n;
}`
	m, err := syntax.ParseModule("audit.js", src, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	var check func(fn *bytecode.Function)
	check = func(fn *bytecode.Function) {
		for i, h := range fn.Handlers {
			if h.Handler >= h.Start && h.Handler < h.End {
				t.Errorf("handler row %d of %q covers its own handler pc: %+v", i, fn.Name, h)
			}
		}
		for _, c := range fn.Children {
			check(c)
		}
	}
	check(code)
}

// --- 11. compiler limits and robustness --------------------------------------------------------

// auditCompilerCompile parses and compiles a module and returns the error.
func auditCompilerCompile(src string) error {
	m, err := syntax.ParseModule("audit.js", src, syntax.Options{})
	if err != nil {
		return err
	}
	_, err = compiler.CompileModule(m)
	return err
}

// TestAuditCompilerChainRegisterPressure: binary() computes a non-leaf left
// operand into a fresh temporary before recursing (operand -> alloc ->
// expr), so a left-associative chain `s+s+...+s` keeps one live register per
// term and any chain of about 250+ terms fails with the 256-register
// SyntaxError. The same holds for `a.b.b.b...` member chains and nested
// array literals. Clean error, never a miscompile, but a legitimate program
// (long string-building expressions) is rejected.
func TestAuditCompilerChainRegisterPressure(t *testing.T) {
	chain := strings.TrimSuffix(strings.Repeat("s+", 500), "+")
	got, errStr := auditCompilerRun(t, "const s = 'ab'; export const r = ("+chain+").length;")
	require.Empty(t, errStr)
	assert.Equal(t, int64(1000), got)
	got, errStr = auditCompilerRun(t, "const a = {}; a.b = a; a.c = 7; export const r = a"+strings.Repeat(".b", 500)+".c;")
	require.Empty(t, errStr)
	assert.Equal(t, int64(7), got)
	got, errStr = auditCompilerRun(t, "const a = {}; a.b = a; a.c = 7; export const r = a"+strings.Repeat("?.b", 300)+".c;")
	require.Empty(t, errStr)
	assert.Equal(t, int64(7), got)
	// The chain still evaluates left to right with every side effect, and
	// mixed operators, immediates and typeof fusions keep their meaning.
	for _, c := range []struct {
		src  string
		want any
	}{
		{`let a = 0; export const r = (a = 1) + a + (a = 2) + a + (a = 3) + a;`, int64(12)},
		{`let n = 0; const f = () => ++n; export const r = "" + f() + f() + f() + n;`, "1233"},
		{`const a = 10, b = 3, c = 4, d = 5, e = 2; export const r = a - b + c * d - e;`, int64(25)},
		{`const x = 5, y = 7; export const r = x + 1 + y - 2 + 100;`, int64(111)},
		{`const x = "s"; export const r = (typeof x === "string") + "" + (typeof x !== "number") + (1 + 2 === 3);`, "truetruetrue"},
		{`const x = 2; export const r = 1 + 2 + x + 3 + 4;`, int64(12)},
		{`const s = "a"; export const r = s + s + s + s + s + s + s + s + s + s + s;`, "aaaaaaaaaaa"},
		{`const a = [1, 2, 3]; export const r = a.length + a[0] + a[a.length - 1] + a.length;`, int64(10)},
		{`const o = {p: {q: {r: {s: 42}}}}; export const r = o.p.q.r.s + o["p"]["q"]["r"].s;`, int64(84)},
		{`const o = {p: {q: null}}; export const r = o.p.q?.r.s.t === undefined;`, true},
		{`let k = 0; const o = {a: {b: {c: 9}}}; export const r = o[(k++, "a")][(k++, "b")][(k++, "c")] + k;`, int64(12)},
		{`const a = 1, b = 2, c = 3; export const r = a < b < c;`, true},
		{`const a = 6, b = 3, c = 2; export const r = a / b / c * 8 % 5;`, int64(3)},
	} {
		got, errStr := auditCompilerRun(t, c.src)
		require.Empty(t, errStr, c.src)
		assert.Equal(t, c.want, got, c.src)
	}
}

// TestAuditCompilerChainThresholds logs the largest chain length of each
// shape that still compiles, and asserts a floor of 200 for all of them.
func TestAuditCompilerChainThresholds(t *testing.T) {
	kinds := []struct {
		name string
		mk   func(n int) string
	}{
		{"plus-chain", func(n int) string {
			return "const s = 1; export const r = " + strings.TrimSuffix(strings.Repeat("s+", n), "+") + ";"
		}},
		{"member-chain", func(n int) string {
			return "const a = {}; a.b = a; export const r = a" + strings.Repeat(".b", n) + " === a;"
		}},
		{"array-nest", func(n int) string {
			return "export const r = " + strings.Repeat("[", n) + strings.Repeat("]", n) + ".length;"
		}},
		{"call-nest", func(n int) string {
			return "function f(x) { return x } export const r = " + strings.Repeat("f(", n) + "1" + strings.Repeat(")", n) + ";"
		}},
		{"paren-nest", func(n int) string {
			return "const s = 1; export const r = " + strings.Repeat("(", n) + "s" + strings.Repeat(")", n) + ";"
		}},
		{"right-assoc-exp-chain", func(n int) string {
			return "const s = 1; export const r = " + strings.TrimSuffix(strings.Repeat("s**", n), "**") + ";"
		}},
	}
	for _, k := range kinds {
		max := 0
		for _, n := range []int{100, 200, 250, 253, 254, 255, 256, 257, 300, 1000} {
			if err := auditCompilerCompile(auditCompilerPrelude + k.mk(n)); err == nil {
				max = n
			} else if !strings.Contains(err.Error(), "SyntaxError") {
				t.Errorf("%s n=%d: non-SyntaxError failure: %v", k.name, n, err)
			}
		}
		t.Logf("%-22s largest compiling length among probes: %d", k.name, max)
		// Nested calls need a fresh call block (callee, this, args) per level
		// and right-associative chains are genuinely nested, so 100 is the
		// floor there; left-associative chains could be compiled in constant
		// registers (see TestAuditCompilerChainRegisterPressure).
		floor := 200
		switch k.name {
		case "call-nest", "right-assoc-exp-chain":
			floor = 100
		case "plus-chain", "member-chain":
			floor = 1000 // one accumulator register
		}
		if max < floor {
			t.Errorf("%s: fails to compile below %d terms (largest ok: %d)", k.name, floor, max)
		}
	}
}

// TestAuditCompilerLimits checks that every documented compiler limit is a
// clean SyntaxError and that programs just below the limit run correctly.
func TestAuditCompilerLimits(t *testing.T) {
	t.Run("constants-65536-clean-error", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("function f() {")
		for i := range 65600 {
			b.WriteString("log.push(\"s" + strconv.Itoa(i) + "\");\n")
		}
		b.WriteString("} export const r = 1;")
		err := auditCompilerCompile(auditCompilerPrelude + b.String())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "SyntaxError: function needs more than 65536 constants")
	})
	t.Run("constants-60000-run", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("function f() {")
		for i := range 60000 {
			b.WriteString("log.push(\"s" + strconv.Itoa(i) + "\");\n")
		}
		b.WriteString("} f(); export const r = J([log.length, log[59999]]);")
		got, errStr := auditCompilerRun(t, b.String())
		require.Empty(t, errStr)
		assert.Equal(t, `[60000,"s59999"]`, got)
	})
	t.Run("registers-300-clean-error", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("function f() { let ")
		for i := range 300 {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString("v" + strconv.Itoa(i) + " = " + strconv.Itoa(i))
		}
		b.WriteString("; return v299 } export const r = f();")
		err := auditCompilerCompile(auditCompilerPrelude + b.String())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "SyntaxError: function needs more than 256 registers")
	})
	t.Run("registers-240-run", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("function f() { let ")
		for i := range 240 {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString("v" + strconv.Itoa(i) + " = " + strconv.Itoa(i))
		}
		b.WriteString("; return v0 + v239 } export const r = f();")
		got, errStr := auditCompilerRun(t, b.String())
		require.Empty(t, errStr)
		assert.Equal(t, int64(239), got)
	})
	// Jump offsets: `n++` on a register local is one instruction, so an if
	// body of N statements needs a forward jump of about N words. Statements
	// are newline-separated to stay clear of the quadratic line-table cost
	// (TestAuditCompilerLineTableQuadratic).
	mkJump := func(n int) string {
		return "function f() { let n = 0; if (log.length === 0) {\n" + strings.Repeat("n++;\n", n) + " } log.push('after'); return n } export const r = J([f(), log]);"
	}
	t.Run("jump-30000-runs-correctly", func(t *testing.T) {
		got, errStr := auditCompilerRun(t, mkJump(30000))
		require.Empty(t, errStr)
		assert.Equal(t, `[30000,["after"]]`, got)
	})
	t.Run("jump-32760-runs-correctly", func(t *testing.T) {
		got, errStr := auditCompilerRun(t, mkJump(32760))
		require.Empty(t, errStr)
		assert.Equal(t, `[32760,["after"]]`, got)
	})
	t.Run("jump-40000-clean-error", func(t *testing.T) {
		err := auditCompilerCompile(auditCompilerPrelude + mkJump(40000))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "SyntaxError: jump offset out of range")
	})
	t.Run("jump-100000-clean-error", func(t *testing.T) {
		err := auditCompilerCompile(auditCompilerPrelude + mkJump(100000))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "SyntaxError: jump offset out of range")
	})
	t.Run("backward-jump-large-loop-body-clean-error", func(t *testing.T) {
		src := "function f() { let n = 0; for (let i = 0; i < 2; i++) { " + strings.Repeat("n++;", 40000) + " } return n } export const r = f();"
		err := auditCompilerCompile(auditCompilerPrelude + src)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "SyntaxError: jump offset out of range")
	})
	// A call block is callee + this + args, so 254 positional arguments fill
	// the 256-register window at module top level; the documented "more than
	// 255 positional arguments" limit is reached only via the register limit.
	t.Run("args-254-run", func(t *testing.T) {
		got, errStr := auditCompilerRun(t, "function f(...a) { return a.length } export const r = f("+strings.TrimSuffix(strings.Repeat("1,", 254), ",")+");")
		require.Empty(t, errStr)
		assert.Equal(t, int64(254), got)
	})
	t.Run("args-255-register-limit-clean-error", func(t *testing.T) {
		err := auditCompilerCompile(auditCompilerPrelude + "function f(...a) { return a.length } export const r = f(" + strings.TrimSuffix(strings.Repeat("1,", 255), ",") + ");")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "SyntaxError: function needs more than 256 registers")
	})
	t.Run("args-256-clean-error", func(t *testing.T) {
		err := auditCompilerCompile(auditCompilerPrelude + "function f(...a) { return a.length } export const r = f(" + strings.TrimSuffix(strings.Repeat("1,", 256), ",") + ");")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "SyntaxError: too many arguments")
	})
	t.Run("args-1000-spread-run", func(t *testing.T) {
		got, errStr := auditCompilerRun(t, "function f(...a) { return a.length } const z = [1]; export const r = f("+strings.TrimSuffix(strings.Repeat("1,", 999), ",")+", ...z);")
		require.Empty(t, errStr)
		assert.Equal(t, int64(1000), got)
	})
	// Every level captures its parameter (the innermost body reads them all),
	// so each arrow owns an Env and the innermost read of a0 is n-1 hops away.
	// The curried function is applied in a loop: a literal g(0)(1)... chain
	// would nest n call blocks and hit the register limit first.
	mkClosures := func(n int) string {
		var b strings.Builder
		b.WriteString("let g = ")
		for i := range n {
			b.WriteString("(a" + strconv.Itoa(i) + " => ")
		}
		b.WriteString("[")
		for i := range n {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString("a" + strconv.Itoa(i))
		}
		b.WriteString("]")
		b.WriteString(strings.Repeat(")", n))
		b.WriteString(";\nfor (let i = 0; i < " + strconv.Itoa(n) + "; i++) g = g(i);\nexport const r = J([g.length, g[0], g[" + strconv.Itoa(n-1) + "]]);")
		return b.String()
	}
	t.Run("closure-nesting-300-clean-error", func(t *testing.T) {
		err := auditCompilerCompile(auditCompilerPrelude + mkClosures(300))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "SyntaxError: closure nesting too deep")
	})
	t.Run("closure-nesting-200-runs", func(t *testing.T) {
		got, errStr := auditCompilerRun(t, mkClosures(200))
		require.Empty(t, errStr)
		assert.Equal(t, `[200,0,199]`, got)
	})
	t.Run("array-literal-70000-elements", func(t *testing.T) {
		// NewArray's capacity hint is a 16-bit operand.
		err := auditCompilerCompile(auditCompilerPrelude + "export const r = [" + strings.Repeat("1,", 70000) + "].length;")
		if err != nil {
			assert.Contains(t, err.Error(), "SyntaxError")
			t.Logf("70000-element array literal: %v", err)
		}
	})
}

// TestAuditCompilerDeepNesting drives parse+compile of deeply nested
// sources in a subprocess (a Go stack overflow is a fatal process crash that
// would otherwise kill the whole test binary) and logs the verdicts. The
// parser is the first recursive stage, so the crash itself is reported from
// syntax/audit_test.go; this test checks that whatever survives parsing
// compiles to a clean result or a clean SyntaxError. The child compiles
// every probe listed in MOEJS_AUDIT_CC_PROBES, printing a PROBE line before
// and a RESULT line after each; when a probe kills it, the parent
// classifies that probe from the crash output and resumes after it.
func TestAuditCompilerDeepNesting(t *testing.T) {
	if list := os.Getenv("MOEJS_AUDIT_CC_PROBES"); list != "" {
		for item := range strings.SplitSeq(list, ",") {
			kind, n, _ := strings.Cut(item, ":")
			depth, _ := strconv.Atoi(n)
			fmt.Printf("PROBE %s %d\n", kind, depth)
			fmt.Printf("RESULT %s %d %s\n", kind, depth, auditCompilerNestedOne(kind, depth))
		}
		return
	}
	t.Parallel()
	type probe struct {
		kind string
		n    int
	}
	var probes []probe
	for _, p := range []struct {
		kind   string
		depths []int
	}{
		{"paren", []int{5000, 50000}},
		{"if", []int{5000, 50000, 100000}},
		{"block", []int{5000, 50000, 100000}},
		{"func", []int{5000, 20000}},
		{"neg", []int{5000, 50000}},
		{"member", []int{100000}},
		{"plus", []int{100000}},
		{"array", []int{5000, 50000}},
		{"object", []int{5000, 50000}},
		{"if-flat-100000", []int{100000}},
	} {
		for _, n := range p.depths {
			probes = append(probes, probe{p.kind, n})
		}
	}
	for done := 0; done < len(probes); {
		items := make([]string, 0, len(probes))
		for _, p := range probes[done:] {
			items = append(items, p.kind+":"+strconv.Itoa(p.n))
		}
		cmd := exec.Command(os.Args[0], "-test.run", "^TestAuditCompilerDeepNesting$", "-test.count=1", "-test.timeout", "300s")
		cmd.Env = append(os.Environ(), "MOEJS_AUDIT_CC_PROBES="+strings.Join(items, ","))
		out, err := cmd.CombinedOutput()
		s := string(out)
		report := func(verdict, detail string) {
			p := probes[done]
			done++
			t.Logf("%-16s depth=%-7d -> %s %s", p.kind, p.n, verdict, strings.TrimSpace(detail))
			if verdict == "PANIC" || verdict == "UNKNOWN" {
				t.Errorf("%s depth=%d: %s %s", p.kind, p.n, verdict, detail)
			}
		}
		for line := range strings.SplitSeq(s, "\n") {
			res, ok := strings.CutPrefix(line, "RESULT ")
			if !ok || done == len(probes) {
				continue
			}
			res, ok = strings.CutPrefix(res, probes[done].kind+" "+strconv.Itoa(probes[done].n)+" ")
			switch {
			case !ok:
				report("UNKNOWN", "out of order: "+line)
			case strings.HasPrefix(res, "panic:"):
				report("PANIC", res)
			case strings.HasPrefix(res, "ok"):
				report("ok", res[len("ok"):])
			case strings.HasPrefix(res, "error"):
				report("SyntaxError", res[len("error"):])
			default:
				report("UNKNOWN", line)
			}
		}
		if done == len(probes) {
			break
		}
		// The child died inside probes[done].
		switch {
		case strings.Contains(s, "fatal error: stack overflow"), strings.Contains(s, "goroutine stack exceeds"):
			report("STACK-OVERFLOW (process crash)", "")
		case strings.Contains(s, "panic:"):
			detail := s[strings.Index(s, "panic:"):]
			if j := strings.IndexByte(detail, '\n'); j >= 0 {
				detail = detail[:j]
			}
			report("PANIC", detail)
		default:
			report("UNKNOWN", fmt.Sprintf("exit=%v out=%.300s", err, s))
		}
	}
}

// auditCompilerNestedOne compiles one probe and describes the outcome; a
// recoverable panic is reported rather than killing the child.
func auditCompilerNestedOne(kind string, n int) (res string) {
	src := auditCompilerNested(kind, n)
	defer func() {
		if p := recover(); p != nil {
			res = fmt.Sprintf("panic: %v", p)
		}
	}()
	start := time.Now()
	err := auditCompilerCompile(src)
	el := time.Since(start).Round(time.Millisecond)
	if err != nil {
		msg := err.Error()
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i]
		}
		return fmt.Sprintf("error %s (%s)", msg, el)
	}
	return fmt.Sprintf("ok (%s)", el)
}

func auditCompilerNested(kind string, n int) string {
	rep := strings.Repeat
	switch kind {
	case "paren":
		return "const x = " + rep("(", n) + "1" + rep(")", n) + ";"
	case "if":
		return rep("if(1)", n) + ";"
	case "block":
		return rep("{", n) + rep("}", n)
	case "func":
		return rep("function f(){", n) + rep("}", n)
	case "neg":
		return "const x = " + rep("- ", n) + "1;"
	case "member":
		return "const a = {}; const x = a" + rep(".b", n) + ";"
	case "plus":
		return "const a = 1; const x = " + strings.TrimSuffix(rep("a+", n), "+") + ";"
	case "array":
		return "const x = " + rep("[", n) + rep("]", n) + ";"
	case "object":
		return "const x = " + rep("{a:", n) + "1" + rep("}", n) + ";"
	case "if-flat-100000":
		return "let n = 0; function f() { if (n === 0) { " + rep("n++;\n", n) + " } }"
	}
	panic("unknown kind " + kind)
}

// TestAuditCompilerLineTableQuadratic: funcState.emitWord
// (compiler/compiler.go:325) calls syntax.File.Position for every emitted
// instruction whose source position changed, and File.Position
// (syntax/source.go:81) computes the column with
// utf8.RuneCountInString(Src[lineStart:pos]) — O(column). On a single-line
// (minified) source that makes compilation O(n^2): measured 2026-09-23,
// 100,000 `n++;` on one line (400 KB) compiled in 13.2 s and 65,600
// statements (1.1 MB) in 35 s, versus well under a second with newlines.
// new-api compiles untrusted plugin source at upload, so this is a CPU DoS
// vector and a real slowdown for minified plugins.
func TestAuditCompilerLineTableQuadratic(t *testing.T) {
	const n = 50000
	// The source has a non-ASCII character up front so the column lookup
	// cannot take the ASCII shortcut (a byte offset) and must use the
	// per-file code point index.
	one := "function f() { let n = 0; const s = \"é\"; " + strings.Repeat("n++;", n) + " return n }"
	multi := "function f() { let n = 0; const s = \"é\";\n" + strings.Repeat("n++;\n", n) + " return n }"
	t0 := time.Now()
	require.NoError(t, auditCompilerCompile(multi))
	dMulti := time.Since(t0)
	t0 = time.Now()
	require.NoError(t, auditCompilerCompile(one))
	dOne := time.Since(t0)
	t.Logf("%d statements: multi-line %v, single-line %v (ratio %.1fx)", n, dMulti, dOne, float64(dOne)/float64(dMulti))
	// Both compile in tens of milliseconds; the quadratic column count took
	// about a second for the single line at this size.
	assert.Less(t, dOne, 5*dMulti+200*time.Millisecond, "single-line compile should not be quadratically slower")
	// The line table of the single-line function carries the right columns.
	m, err := syntax.ParseModule("audit.js", "export "+one, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	f := code.Children[0]
	last := f.LineTable[len(f.LineTable)-1]
	require.Equal(t, int32(1), last.Line)
	// The last instruction is the read of n in `return n`.
	wantCol := utf8.RuneCountInString(one[:strings.LastIndex(one, "return n")+len("return ")]) + 1
	require.Equal(t, int32(wantCol), last.Col, "column of the returned operand")
}

// TestAuditCompilerNotes logs (does not assert) the exact behaviour of the
// informational checklist items so the report can quote them.
func TestAuditCompilerNotes(t *testing.T) {
	for _, c := range []struct{ name, src string }{
		{"direct-eval", `export const r = tryRun(() => eval('1'));`},
		{"indirect-eval", `const e = eval; export const r = tryRun(() => e('1'));`},
		{"new-Function", `export const r = tryRun(() => new Function('return 1'));`},
		{"bigint-unary-minus", `export const r = tryRun(() => -1n);`},
		{"bigint-compare-mixed", `export const r = tryRun(() => J([1n < 2n, 1n < 2, 2 > 1n, 1n == 1]));`},
		{"arguments-error", `function f() { return arguments } export const r = 1;`},
		{"getter-literal-error", `const o = { get x() { return 1 } }; export const r = 1;`},
		{"export-default-value", `export default 40 + 2; export const r = 1;`},
		{"destructure-null-message", `export const r = tryRun(() => { const {a} = null });`},
		{"destructure-undefined-array-message", `export const r = tryRun(() => { const [a] = undefined });`},
		{"array-like-spread-message", `export const r = tryRun(() => [...{length: 1, 0: 'a'}]);`},
		{"delete-frozen-message", `export const r = tryRun(() => { const o = Object.freeze({x: 1}); return delete o.x });`},
		{"in-primitive-message", `export const r = tryRun(() => 'a' in 'abc');`},
		{"new-arrow-message", `const A = () => {}; export const r = tryRun(() => new A());`},
		{"const-update-valueOf-order", `const o = { valueOf() { log.push('v'); return 1 } }; try { o++ } catch (e) { log.push(String(e)) } export const r = J(log);`},
	} {
		got, errStr := auditCompilerRun(t, c.src)
		t.Logf("%-38s value=%#v err=%q", c.name, got, errStr)
	}
	// export default value through GetBindingValue.
	m, err := syntax.ParseModule("audit.js", `export default 40 + 2;`, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	r := NewRealm()
	env, err := r.EvaluateModule(code)
	require.NoError(t, err)
	v, ok := env.GetBindingValue("default")
	t.Logf("export default 40+2 -> GetBindingValue(default) ok=%v value=%v", ok, r.ToGo(v))
}
