package engine

import (
	"fmt"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newAsyncFixture evaluates a module in a fresh realm whose f runs body and
// whose done returns log joined by "|": the call of f drains the jobs its
// body queued, so done sees their effects. C is a promise constructor whose
// resolve returns its argument.
func newAsyncFixture(t *testing.T, body string, shared bool) *moduleFixture {
	t.Helper()
	src := "export const log = [];\nfunction C(ex) { return new Promise(ex); }\nC.resolve = v => v;\nexport function f(...args) {\n" + body + "\n}\nexport function done() { return log.join(\"|\"); }"
	return evalModuleWith(t, src, RealmOptions{SharedIntrinsics: shared})
}

// runAsyncCases runs every case in a mutable and in a shared realm; want is
// the log after the drain, or "!" and the prefix of what f throws.
func runAsyncCases(t *testing.T, cases []protoCase) {
	t.Helper()
	for _, shared := range []bool{false, true} {
		for _, c := range cases {
			t.Run(fmt.Sprintf("%s/shared=%v", c.name, shared), func(t *testing.T) {
				f := newAsyncFixture(t, c.body, shared)
				if _, err := f.callErr("f"); err != nil {
					got := err.Error()
					var exc *Exception
					if asException(err, &exc) {
						got = errorDisplayString(exc.Value)
					}
					assert.Truef(t, len(c.want) > 0 && c.want[0] == '!' && hasPrefix(got, c.want[1:]), "threw %q, want %q", got, c.want)
					return
				}
				assert.Equal(t, c.want, f.call("done"))
			})
		}
	}
}

func TestPromiseSemantics(t *testing.T) {
	runAsyncCases(t, []protoCase{
		{"shape", `log.push(typeof Promise, Promise.length, Promise.name, Promise.prototype.then.length, Promise.prototype.catch.length, Promise.prototype.finally.length,
			Promise.all.length, Promise.withResolvers.length, Promise.try.length, Promise[Symbol.species] === Promise, Promise.prototype.constructor === Promise,
			Object.prototype.toString.call(Promise.resolve()), Promise.prototype[Symbol.toStringTag])`,
			"function|1|Promise|2|1|1|1|0|1|true|true|[object Promise]|Promise"},
		{"descriptors", `const d = Object.getOwnPropertyDescriptor(Promise.prototype, Symbol.toStringTag); const s = Object.getOwnPropertyDescriptor(Promise, Symbol.species);
			log.push(d.value, d.writable, d.enumerable, typeof s.get, String(s.set), s.get.name, Object.getOwnPropertyDescriptor(Promise, "prototype").writable)`,
			"Promise|false|false|function|undefined|get [Symbol.species]|false"},
		{"call without new", `Promise(() => {})`, "!TypeError: Constructor Promise requires 'new'"},
		{"executor not callable", `new Promise(1)`, "!TypeError"},
		{"executor runs synchronously", `new Promise(() => log.push("ex")); log.push("after")`, "ex|after"},
		{"resolving functions", `new Promise((res, rej) => log.push(res.length, res.name === "", rej.length, rej.name === "", res !== rej))`, "1|true|1|true|true"},
		{"then order", `const p = Promise.resolve();
			p.then(() => log.push("a1")).then(() => log.push("a2"));
			p.then(() => log.push("b1")).then(() => log.push("b2"));
			log.push("sync")`, "sync|a1|b1|a2|b2"},
		{"pending reactions in order", `let res; const p = new Promise(r => res = r);
			p.then(v => log.push("1:" + v)); p.then(v => log.push("2:" + v)); res("x"); res("y"); log.push("sync")`, "sync|1:x|2:x"},
		{"native promise adoption takes two jobs", `const p = new Promise(res => res(Promise.resolve("x")));
			p.then(v => log.push("p:" + v));
			Promise.resolve().then(() => log.push("t1")).then(() => log.push("t2")).then(() => log.push("t3"))`, "t1|t2|p:x|t3"},
		{"thenable adoption", `const t = {then(res, rej) { log.push("then:" + (this === t) + ":" + arguments.length); res("v"); res("w"); rej("e"); }};
			Promise.resolve(t).then(v => log.push("got:" + v)); log.push("sync")`, "sync|then:true:2|got:v"},
		{"thenable throws after resolving", `Promise.resolve({then(res) { res(1); throw new Error("late"); }}).then(v => log.push(v), e => log.push("rejected"))`, "1"},
		{"thenable throws", `Promise.resolve({then() { throw new RangeError("t"); }}).catch(e => log.push(e.name))`, "RangeError"},
		{"then getter throws", `Promise.resolve({get then() { throw "g"; }}).catch(e => log.push(e))`, "g"},
		{"then getter read once", `let n = 0; const o = {get then() { n++; return undefined; }}; Promise.resolve(o).then(v => log.push(v === o, n))`, "true|1"},
		{"self resolution", `let res; const p = new Promise(r => res = r); res(p); p.catch(e => log.push(e instanceof TypeError))`, "true"},
		{"executor throws", `new Promise(() => { throw "boom"; }).catch(e => log.push(e))`, "boom"},
		{"executor throws after resolve", `new Promise(res => { res(1); throw "boom"; }).then(v => log.push(v))`, "1"},
		{"handler passes through", `Promise.reject(1).then(5).catch(e => log.push("c" + e)); Promise.resolve(2).catch(8).then(v => log.push("t" + v))`, "c1|t2"},
		{"handler throws", `Promise.resolve().then(() => { throw "h"; }).then(null, e => log.push(e))`, "h"},
		{"handler returns promise", `Promise.resolve().then(() => Promise.resolve("in")).then(v => log.push(v))`, "in"},
		{"then chain cycle", `const p = Promise.resolve().then(() => p); p.catch(e => log.push(e.constructor.name))`, "TypeError"},
		{"then on non-promise", `Promise.prototype.then.call({}, () => {})`, "!TypeError"},
		{"catch is generic", `const o = {then(a, b) { log.push(String(a), typeof b); return "r"; }}; log.push(Promise.prototype.catch.call(o, () => {}))`, "undefined|function|r"},
		{"resolve returns the same promise", `const p = Promise.resolve(1); log.push(Promise.resolve(p) === p, Promise.resolve(Promise.reject(0).catch(() => {})) instanceof Promise)`, "true|true"},
		{"resolve checks constructor", `const p = Promise.resolve(1); p.constructor = Object; log.push(Promise.resolve(p) !== p)`, "true"},
		{"resolve non-object this", `Promise.resolve.call(1, 2)`, "!TypeError"},
		{"reject", `Promise.reject(Promise.resolve(1)).catch(e => log.push(e instanceof Promise))`, "true"},
		{"subclass", `class P extends Promise { constructor(ex) { log.push("ctor"); super(ex); } }
			const p = P.resolve(1); const q = p.then(v => log.push("v" + v));
			log.push(p instanceof P, q instanceof P, P.reject(0) instanceof P)`, "ctor|ctor|ctor|true|true|true|v1"},
		{"species", `class P extends Promise { static get [Symbol.species]() { return Promise; } }
			const p = new P(r => r(1)); log.push(p instanceof P, p.then() instanceof P, p.then() instanceof Promise)`, "true|false|true"},
		{"executor called twice", `function C(ex) { ex(() => {}, () => {}); ex(() => {}, () => {}); } Promise.resolve.call(C, 1)`, "!TypeError"},
		{"executor with undefined first", `function C(ex) { ex(undefined, undefined); ex(() => log.push("res"), () => {}); } Promise.resolve.call(C, 1)`, "res"},
		{"capability not callable", `function C(ex) { ex(1, 2); } Promise.reject.call(C, 1)`, "!TypeError"},
		{"all", `Promise.all([1, Promise.resolve(2), {then(r) { r(3); }}]).then(v => log.push(Array.isArray(v), v.join("+")))`, "true|1+2+3"},
		{"all empty", `Promise.all([]).then(v => log.push(v.length))`, "0"},
		{"all rejects with the first", `Promise.all([Promise.reject("a"), Promise.reject("b"), 1]).catch(e => log.push(e))`, "a"},
		{"all not iterable rejects", `Promise.all(1).catch(e => log.push(e.constructor.name))`, "TypeError"},
		{"all non-constructor this", `Promise.all.call({}, [])`, "!TypeError"},
		{"all resolve not callable rejects", `const C = function (ex) { return new Promise(ex); }; C.resolve = 1; Promise.all.call(C, [1]).catch(e => log.push(e.constructor.name))`, "TypeError"},
		{"all closes the iterator", `const it = {[Symbol.iterator]() { return this; }, next() { return {value: 1, done: false}; }, return() { log.push("return"); return {}; }};
			const C = function (ex) { return new Promise(ex); }; C.resolve = () => { throw "no"; };
			Promise.all.call(C, it).catch(e => log.push(e))`, "return|no"},
		{"all next throws does not close", `const it = {[Symbol.iterator]() { return this; }, next() { throw "n"; }, return() { log.push("return"); return {}; }};
			Promise.all(it).catch(e => log.push(e))`, "n"},
		// C.resolve returns the thenable itself, so the combinator calls its
		// then synchronously.
		{"all element called once", `let f; const t = {then(a) { f = a; }}; Promise.all.call(C, [t]).then(v => log.push(v[0])); f("x"); f("y")`, "x"},
		{"all calls resolve per element", `let n = 0; class P extends Promise { static resolve(v) { n++; return super.resolve(v); } }
			P.all([1, 2, 3]).then(() => log.push(n))`, "3"},
		{"allSettled", `Promise.allSettled([1, Promise.reject(2), {then(a, b) { b(3); }}]).then(v => log.push(JSON.stringify(v)))`,
			`[{"status":"fulfilled","value":1},{"status":"rejected","reason":2},{"status":"rejected","reason":3}]`},
		{"allSettled pair shares called", `let a, b; Promise.allSettled.call(C, [{then(x, y) { a = x; b = y; }}]).then(v => log.push(JSON.stringify(v))); b("r"); a("f")`,
			`[{"status":"rejected","reason":"r"}]`},
		{"element functions return the capability's result", `function T(ex) { ex(() => "resolve-ret", () => "reject-ret"); } T.resolve = v => v;
			let fns = []; const t = {then(f, r) { fns.push(f, r); }};
			Promise.all.call(T, [t]); log.push(fns[0]("x"), fns[0]("x"));
			fns = []; Promise.any.call(T, [t]); log.push(fns[1]("x"));
			fns = []; Promise.allSettled.call(T, [t, t]); log.push(fns[1]("x"), fns[2]("y"));
			function W(ex) { return new Promise(ex); } W.resolve = v => v;
			fns = []; Promise.all.call(W, [t]); log.push(fns[0]("x"))`, "resolve-ret||reject-ret||resolve-ret|"},
		{"any", `Promise.any([Promise.reject(1), 2, Promise.resolve(3)]).then(v => log.push(v))`, "2"},
		{"any all rejected", `Promise.any([Promise.reject(1), Promise.reject(2)]).catch(e => log.push(e instanceof AggregateError, e.errors.join(), Object.prototype.hasOwnProperty.call(e, "message"),
			JSON.stringify(Object.getOwnPropertyDescriptor(e, "errors")).replace(/"value":\[[^\]]*\],/, "")))`,
			`true|1,2|false|{"writable":true,"enumerable":false,"configurable":true}`},
		{"any empty", `Promise.any([]).catch(e => log.push(e.constructor.name, e.errors.length))`, "AggregateError|0"},
		{"race", `Promise.race([new Promise(() => {}), Promise.resolve("b"), Promise.reject("c")]).then(v => log.push(v))`, "b"},
		{"race rejects", `Promise.race([Promise.reject("c"), Promise.resolve("b")]).catch(v => log.push(v))`, "c"},
		{"race empty stays pending", `Promise.race([]).then(() => log.push("no"), () => log.push("no")); log.push("sync")`, "sync"},
		{"race first resolution wins", `let a, b; Promise.race.call(C, [{then(r) { a = r; }}, {then(r) { b = r; }}]).then(v => log.push(v)); b({then(r) { r("slow"); }}); a("fast")`, "slow"},
		{"withResolvers", `const {promise, resolve, reject} = Promise.withResolvers(); promise.then(v => log.push(v)); resolve("w"); reject("x");
			log.push(Object.keys(Promise.withResolvers()).join())`, "promise,resolve,reject|w"},
		{"withResolvers subclass", `class P extends Promise {} log.push(P.withResolvers().promise instanceof P)`, "true"},
		{"try", `Promise.try((a, b) => a + b, 1, 2).then(v => log.push(v)); Promise.try(() => { throw "t"; }).catch(e => log.push(e));
			Promise.try(1).catch(e => log.push(e.constructor.name))`, "3|t|TypeError"},
		{"try runs synchronously", `Promise.try(() => log.push("cb")); log.push("sync")`, "cb|sync"},
		{"try does not wrap", `const p = Promise.resolve(); class P extends Promise {} const q = P.resolve();
			log.push(Promise.try(() => p) === p, P.try(() => q) === q, P.try(() => p) !== p, P.try(() => { throw 1; }) instanceof P)`, "true|true|true|true"},
		{"try calls the callback first", `function C(ex) { log.push("ctor"); return new Promise(ex); } C.resolve = Promise.resolve;
			Promise.try.call(C, () => log.push("cb")); Promise.try.call(C, () => { log.push("cb2"); throw 0; })`, "cb|ctor|cb2|ctor"},
		{"finally passes through", `Promise.resolve(1).finally(() => 9).then(v => log.push("v" + v)); Promise.reject(2).finally(() => 9).catch(e => log.push("e" + e))`, "v1|e2"},
		{"finally args", `Promise.resolve(1).finally(function () { log.push(arguments.length); })`, "0"},
		{"finally overrides", `Promise.resolve(1).finally(() => { throw "f"; }).catch(e => log.push(e)); Promise.resolve(1).finally(() => Promise.reject("r")).catch(e => log.push(e))`, "f|r"},
		{"finally not callable", `Promise.resolve(1).finally(5).then(v => log.push(v))`, "1"},
		{"finally on thenable", `const o = {then(a, b) { log.push(typeof a, typeof b, a === b); }}; Promise.prototype.finally.call(o, 1); Promise.prototype.finally.call(o, () => {})`,
			"number|number|true|function|function|false"},
		{"finally on non-object", `Promise.prototype.finally.call(1)`, "!TypeError"},
		{"finally species", `class P extends Promise {} const p = P.resolve(1).finally(() => {}); log.push(p instanceof P)`, "true"},
		{"queueMicrotask", `log.push(typeof queueMicrotask, queueMicrotask.length);
			Promise.resolve().then(() => log.push("p1"));
			queueMicrotask(() => { log.push("m1"); queueMicrotask(() => log.push("m3")); });
			Promise.resolve().then(() => log.push("p2"));
			queueMicrotask(function () { log.push("m2:" + (this === undefined) + ":" + arguments.length); });
			log.push("sync")`, "function|1|sync|p1|m1|p2|m2:true:0|m3"},
		{"queueMicrotask not callable", `queueMicrotask({})`, "!TypeError: queueMicrotask"},
		{"not cloneable", `structuredClone(Promise.resolve())`, "!TypeError: DataCloneError"},
		{"promise is not a plain object", `const p = Promise.resolve(); log.push(typeof p, p instanceof Promise, Object.getPrototypeOf(p) === Promise.prototype)`, "object|true|true"},
	})
}

// TestPromiseProxies runs the combinators, thenable resolution and the
// constructor protocol through proxies: every observable step reaches a
// trap, in spec order, and a trap that throws where the spec rejects
// (IfAbruptRejectPromise, the resolve functions, a reaction job) rejects.
func TestPromiseProxies(t *testing.T) {
	runAsyncCases(t, []protoCase{
		{"all over a proxied array", `const a = new Proxy([1, Promise.resolve(2)], {get(t, k, r) { log.push(String(k)); return Reflect.get(t, k, r); }});
			Promise.all(a).then(v => log.push(v.join("+")))`, "Symbol(Symbol.iterator)|length|0|length|1|length|1+2"},
		{"iterable trap throws rejects", `const a = new Proxy([], {get() { throw "it"; }});
			for (const k of ["all", "allSettled", "any", "race"]) Promise[k](a).catch(e => log.push(k + ":" + e))`, "all:it|allSettled:it|any:it|race:it"},
		{"iterator next trap throws rejects", `Promise.all({[Symbol.iterator]() { return new Proxy({}, {get(t, k) { throw "next"; }}); }}).catch(e => log.push(e))`, "next"},
		{"proxy thenable element", `const t = new Proxy({}, {get(t, k) { log.push(String(k)); return k === "then" ? r => r("pv") : undefined; }});
			Promise.all([t]).then(v => log.push(v[0]))`, "then|pv"},
		{"element then trap throws rejects", `Promise.all([1, new Proxy({}, {get() { throw "el"; }})]).catch(e => log.push(e))`, "el"},
		{"resolved proxy's then trap throws rejects", `const bad = new Proxy({}, {get() { throw new RangeError("trap"); }});
			new Promise(res => res(bad)).catch(e => log.push(e.name)); Promise.resolve().then(() => bad).catch(e => log.push("h:" + e.name))`, "RangeError|h:RangeError"},
		{"revoked proxy rejects", `const {proxy, revoke} = Proxy.revocable({}, {}); revoke(); Promise.resolve(proxy).catch(e => log.push(e.constructor.name))`, "TypeError"},
		{"proxy of a promise is not a promise", `const p = Promise.resolve(1), q = new Proxy(p, {}); log.push(Promise.resolve(q) !== q);
			try { q.then(); } catch (e) { log.push(e.constructor.name); } Promise.resolve(q).catch(e => log.push("adopt:" + e.constructor.name))`, "true|TypeError|adopt:TypeError"},
		{"proxy constructor", `const P = new Proxy(Promise, {get(t, k, r) { log.push(String(k)); return Reflect.get(t, k, r); }});
			Promise.all.call(P, [1]).then(v => log.push("v" + v[0])); log.push(Promise.withResolvers.call(P).promise instanceof Promise)`, "prototype|resolve|prototype|prototype|true|v1"},
		{"constructor trap throws rejects", `const P = new Proxy(Promise, {get(t, k, r) { if (k === "resolve") throw "res"; return Reflect.get(t, k, r); }});
			Promise.race.call(P, [1]).catch(e => log.push(e))`, "res"},
		{"construct trap throws", `const P = new Proxy(Promise, {construct() { throw "ctor"; }}); try { Promise.all.call(P, []); } catch (e) { log.push(e); }`, "ctor"},
		{"resolve returns a proxy whose then throws", `function D(ex) { return new Promise(ex); } D.resolve = () => new Proxy({}, {get() { throw "inv"; }});
			Promise.all.call(D, [1]).catch(e => log.push(e))`, "inv"},
		{"species through a proxy", `class P extends Promise {} const p = P.resolve(1);
			p.constructor = new Proxy(P, {get(t, k, r) { log.push(String(k)); return Reflect.get(t, k, r); }});
			const q = p.then(v => log.push("v" + v)); log.push(q instanceof P)`, "Symbol(Symbol.species)|prototype|true|v1"},
		{"species trap throws synchronously", `const p = Promise.resolve(); p.constructor = new Proxy(function () {}, {get() { throw "sp"; }});
			try { p.then(); } catch (e) { log.push(e); }`, "sp"},
		{"finally on a proxy", `const q = new Proxy(Promise.resolve(1), {get(t, k, r) { log.push(String(k)); return Reflect.get(t, k, r); }});
			try { Promise.prototype.finally.call(q, () => {}); } catch (e) { log.push(e.constructor.name); }`, "constructor|then|TypeError"},
		{"callable proxies as handlers and jobs", `const h = new Proxy(v => log.push("h" + v), {apply(t, th, a) { log.push("apply"); return Reflect.apply(t, th, a); }});
			Promise.resolve(1).then(h); Promise.resolve({then: new Proxy(r => r("pt"), {})}).then(v => log.push(v));
			Promise.resolve().then(new Proxy(() => {}, {apply() { throw "ap"; }})).catch(e => log.push(e)); queueMicrotask(new Proxy(() => log.push("qm"), {}))`,
			"apply|h1|qm|pt|ap"},
	})
}

func TestPromiseMutableIntrinsics(t *testing.T) {
	runMutableProtoCases(t, []protoCase{
		// then is looked up on the value, so a replaced then adopts through it.
		{"replaced then", `const orig = Promise.prototype.then; let n = 0; Promise.prototype.then = function (a, b) { n++; return orig.call(this, a, b); };
			Promise.all([Promise.resolve(1)]); Promise.prototype.then = orig; return n`, "1"},
		{"late global", `const d = Object.getOwnPropertyDescriptor(globalThis, "Promise"); return [d.writable, d.enumerable, d.configurable, typeof queueMicrotask].join()`, "true,false,true,function"},
		{"deleted global keeps working", `const P = Promise; delete globalThis.Promise; return [typeof globalThis.Promise, P.resolve(1) instanceof P].join()`, "undefined,true"},
	})
}

// reactionLog is a promiseReactor that logs its reactions.
type reactionLog struct {
	log []string
	err error
}

func (x *reactionLog) promiseSettled(r *Realm, v Value, rejected bool) error {
	x.log = append(x.log, fmt.Sprintf("%v:%v", rejected, r.ToGo(v)))
	return x.err
}

// drain runs the queued jobs the way the return of an outermost call does.
func drain(r *Realm) error {
	if !r.jobsPending {
		return nil
	}
	return r.endJob(nil)
}

func TestPromiseInternalContract(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared=%v", shared), func(t *testing.T) {
			r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
			p := r.newPromise()
			require.Equal(t, ClassPromise, p.class)
			require.Same(t, r.promise.proto, p.proto)
			d := p.internal.(*promiseData)
			x := &reactionLog{}
			r.promiseReact(p, x)
			r.promiseReact(p, x)
			assert.True(t, d.handled)
			r.resolvePromise(p, IntValue(1))
			r.rejectPromise(p, IntValue(2)) // settled: ignored
			assert.Empty(t, x.log, "reactions run as jobs")
			require.NoError(t, drain(r))
			assert.Equal(t, []string{"false:1", "false:1"}, x.log)
			assert.Equal(t, PromiseFulfilled, d.state)

			// A settled promise queues the reaction at once.
			x.log = nil
			r.promiseReact(p, x)
			require.NoError(t, drain(r))
			assert.Equal(t, []string{"false:1"}, x.log)

			// Rejection, and adoption of a promise through a job.
			q, pr := r.newPromise(), r.newPromise()
			x.log = nil
			r.promiseReact(q, x)
			r.resolvePromise(q, ObjectValue(pr))
			r.rejectPromise(pr, StringValue(r.InternGoString("no")))
			require.NoError(t, drain(r))
			assert.Equal(t, []string{"true:no"}, x.log)

			// promiseResolve returns a %Promise% instance itself and wraps
			// anything else.
			got, err := r.promiseResolve(ObjectValue(p))
			require.NoError(t, err)
			assert.Same(t, p, got)
			got, err = r.promiseResolve(IntValue(5))
			require.NoError(t, err)
			assert.Equal(t, PromiseFulfilled, got.internal.(*promiseData).state)
			assert.Equal(t, 5, int(got.internal.(*promiseData).result.AsNumber()))
		})
	}
}

func TestPromiseReactAllocs(t *testing.T) {
	r := NewRealm()
	p := r.newPromise()
	x := &reactionLog{}
	r.promiseReact(p, x) // the first reaction creates the job state
	allocs := testing.AllocsPerRun(100, func() { r.promiseReact(p, x) })
	assert.Equal(t, 1.0, allocs, "the reaction record only")
}

func TestPromiseReactorError(t *testing.T) {
	r := NewRealm()
	p := r.newPromise()
	x := &reactionLog{err: r.TypeError("reactor")}
	r.promiseReact(p, x)
	r.resolvePromise(p, Undefined())
	err := drain(r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reactor")
}

func TestPromiseRejectionTracker(t *testing.T) {
	r := NewRealm()
	var ops []string
	r.SetPromiseRejectionTracker(func(p *Object, op PromiseRejectionOperation) {
		_, v, _ := p.PromiseResult()
		ops = append(ops, fmt.Sprintf("%d:%v", op, r.ToGo(v)))
	})
	p := r.newPromise()
	r.rejectPromise(p, IntValue(1))   // no handler yet
	r.promiseReact(p, &reactionLog{}) // handled from now on
	r.promiseReact(p, &reactionLog{})
	q := r.newPromise()
	r.promiseReact(q, &reactionLog{})
	r.rejectPromise(q, IntValue(2)) // handled before it was rejected
	require.NoError(t, drain(r))
	assert.Equal(t, []string{"0:1", "1:1"}, ops)
	r.SetPromiseRejectionTracker(nil)
	r.rejectPromise(r.newPromise(), IntValue(3))
	assert.Len(t, ops, 2)
	NewRealm().SetPromiseRejectionTracker(nil) // no job state to clear
}

func TestPromiseHostAPI(t *testing.T) {
	assert.LessOrEqual(t, unsafe.Sizeof(promiseObject{}), uintptr(128), "a promise stays in the 128-byte size class")
	r := NewRealm()
	p, resolve, reject := r.NewPromiseWithResolvers()
	state, v, ok := p.PromiseResult()
	assert.True(t, ok)
	assert.Equal(t, PromisePending, state)
	assert.True(t, v.IsUndefined())
	_, _, ok = r.NewObject().PromiseResult()
	assert.False(t, ok)

	// The reactions run in the order they were added, then resolve adopts a
	// thenable and a later reject does nothing.
	var order []string
	for i := range 3 {
		r.promiseReact(p, orderReactor{&order, i})
	}
	q := r.newPromise()
	_, err := r.CallObject(resolve, Undefined(), []Value{ObjectValue(q)})
	require.NoError(t, err)
	_, err = r.CallObject(reject, Undefined(), []Value{IntValue(0)})
	require.NoError(t, err)
	r.resolvePromise(q, IntValue(7))
	require.NoError(t, drain(r))
	state, v, _ = p.PromiseResult()
	assert.Equal(t, PromiseFulfilled, state)
	assert.Equal(t, IntValue(7), v)
	assert.Equal(t, []string{"0:7", "1:7", "2:7"}, order)
}

// orderReactor appends its index and the result to a shared log.
type orderReactor struct {
	log *[]string
	i   int
}

func (x orderReactor) promiseSettled(r *Realm, v Value, rejected bool) error {
	*x.log = append(*x.log, fmt.Sprintf("%d:%v", x.i, r.ToGo(v)))
	return nil
}

func TestPromiseLateInstall(t *testing.T) {
	// Code that needs %Promise% installs it in a mutable realm whose
	// program never named Promise.
	r := NewRealm()
	require.Nil(t, r.promise)
	p := r.newPromise()
	require.NotNil(t, r.promise)
	v, err := r.Global.GetProp(r, StringKey(AtomPromise))
	require.NoError(t, err)
	assert.Same(t, r.promise.ctor, v.AsObject())
	assert.Same(t, r.promise.proto, p.proto)
}

// TestPromiseRootShape: the promises a realm makes take the root shape
// rootShapeFor gives their prototype, in a mutable realm (one whose
// Promise is defined after boot) and in shared realms, so an object made
// with the same prototype shares their shapes.
func TestPromiseRootShape(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for range 2 {
			r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
			pi := r.promiseIntr()
			want := r.rootShapeFor(pi.proto)
			assert.Same(t, want, r.newPromise().shape, "shared=%v", shared)
			assert.Same(t, want, r.allocPromise(pi.proto).shape, "shared=%v", shared)
			assert.Same(t, want, r.NewObjectWithProto(pi.proto).shape, "shared=%v", shared)
		}
	}
}

// TestSettlePromise: a promise of NewHostPromise settles once, as through
// its resolving functions: resolve adopts a thenable, later calls do
// nothing, a call from outside a call runs the jobs and one inside a call
// leaves them to the outermost return, and the rejection tracker sees a
// rejection with no handler. Other promises, another realm's included, are
// refused.
func TestSettlePromise(t *testing.T) {
	for _, shared := range []bool{false, true} {
		r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
		p := r.NewHostPromise()
		var order []string
		for i := range 2 {
			r.promiseReact(p, orderReactor{&order, i})
		}
		q := r.newPromise()
		require.NoError(t, r.SettlePromise(p, ObjectValue(q), false))
		require.NoError(t, r.SettlePromise(p, IntValue(0), true))
		require.NoError(t, r.SettlePromise(p, IntValue(1), false))
		state, _, _ := p.PromiseResult()
		assert.Equal(t, PromisePending, state, "adopting the thenable")
		r.HoldJobs()
		r.resolvePromise(q, IntValue(7))
		require.NoError(t, r.ReleaseJobs(nil))
		state, v, _ := p.PromiseResult()
		assert.Equal(t, PromiseFulfilled, state)
		assert.Equal(t, IntValue(7), v)
		assert.Equal(t, []string{"0:7", "1:7"}, order)

		// From inside a call the jobs wait for the outermost return.
		p = r.NewHostPromise()
		order = order[:0]
		r.promiseReact(p, orderReactor{&order, 0})
		r.HoldJobs()
		require.NoError(t, r.SettlePromise(p, IntValue(1), false))
		assert.Empty(t, order)
		require.NoError(t, r.ReleaseJobs(nil))
		assert.Equal(t, []string{"0:1"}, order)

		var ops []PromiseRejectionOperation
		r.SetPromiseRejectionTracker(func(_ *Object, op PromiseRejectionOperation) { ops = append(ops, op) })
		p = r.NewHostPromise()
		require.NoError(t, r.SettlePromise(p, IntValue(2), true))
		state, v, _ = p.PromiseResult()
		assert.Equal(t, PromiseRejected, state)
		assert.Equal(t, IntValue(2), v)
		assert.Equal(t, []PromiseRejectionOperation{PromiseRejectionReject}, ops)
		r.SetPromiseRejectionTracker(nil)

		// A queueMicrotask callback's exception comes back as a call's would.
		r.enqueue(job{fn: r.NewNativeFunction(AtomEmpty, 0, func(r *Realm, this Value, args []Value) (Value, error) {
			return Undefined(), r.Throw(StringValue(FromGoString("cb")))
		})})
		err := r.SettlePromise(r.NewHostPromise(), IntValue(3), false)
		var exc *Exception
		require.True(t, asException(err, &exc), "%v", err)
		assert.Equal(t, "cb", exc.Value.String())
		f, _, _ := r.NewPromiseWithResolvers()
		assert.Error(t, r.SettlePromise(f, IntValue(1), false), "a promise with resolving functions")
		assert.Error(t, r.SettlePromise(r.NewObject(), IntValue(1), false), "an object")
		other := NewRealmWith(RealmOptions{SharedIntrinsics: shared}).NewHostPromise()
		assert.Error(t, r.SettlePromise(other, IntValue(1), false), "a promise of another realm")
		state, _, _ = other.PromiseResult()
		assert.Equal(t, PromisePending, state, "a promise of another realm")
	}
}
