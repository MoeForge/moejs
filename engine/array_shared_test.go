package engine

import (
	"strings"
	"testing"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sharedConstsJS runs every way to write an array on two arrays of the
// same literal of constants, whose elements are shared (bitSharedElems),
// and compares each with an array of that literal made before and one
// made after; then the reads that leave the elements shared.
const sharedConstsJS = `var out = [];
function lit() { return [1, 2, 3, 4, 5, 6, 7, 8, 9, 'ten', null, undefined, -0, 1.5]; }
function strictLit() { 'use strict'; return [1, 2, 3, 4, 5, 6, 7, 8, 9, 10]; }
function show(x) {
	var s = [];
	for (var i = 0; i < x.length; i++) s.push(!(i in x) ? 'hole' : Object.is(x[i], -0) ? '-0' : typeof x[i] === 'string' ? '"' + x[i] + '"' : String(x[i]));
	return s.join(',') + ' #' + x.length;
}
function err(f) { try { return String(f()); } catch (e) { return e.constructor.name; } }
var muts = [
	['set', function (a) { a[0] = 'x'; }],
	['setLast', function (a) { a[a.length - 1] = 'x'; }],
	['setPast', function (a) { a[a.length] = 'x'; }],
	['setFar', function (a) { a[40] = 'x'; }],
	['inc', function (a) { a[1]++; }],
	['compound', function (a) { a[2] += 10; }],
	['push', function (a) { a.push('x', 'y'); }],
	['pop', function (a) { a.pop(); }],
	['shift', function (a) { a.shift(); }],
	['unshift', function (a) { a.unshift('x'); }],
	['spliceDel', function (a) { a.splice(1, 2); }],
	['spliceIns', function (a) { a.splice(1, 0, 'x', 'y'); }],
	['spliceRep', function (a) { a.splice(3, 1, 'x'); }],
	['reverse', function (a) { a.reverse(); }],
	['sort', function (a) { a.sort(); }],
	['sortFn', function (a) { a.sort(function (x, y) { return String(y) < String(x) ? -1 : 1; }); }],
	['fill', function (a) { a.fill(0, 2, 5); }],
	['fillAll', function (a) { a.fill('f'); }],
	['copyWithin', function (a) { a.copyWithin(0, 5, 8); }],
	['truncate', function (a) { a.length = 3; }],
	['extend', function (a) { a.length = 20; a[18] = 'x'; }],
	['zero', function (a) { a.length = 0; a.push('x'); }],
	['delete', function (a) { delete a[4]; }],
	['defineIndex', function (a) { Object.defineProperty(a, 5, {value: 'd', writable: false}); }],
	['defineGetter', function (a) { Object.defineProperty(a, 6, {get: function () { return 'g'; }}); }],
	['defineLength', function (a) { Object.defineProperty(a, 'length', {value: 4}); }],
	['freeze', function (a) { Object.freeze(a); a[0] = 'x'; }],
	['seal', function (a) { Object.seal(a); a[0] = 'x'; delete a[1]; }],
	['preventExtensions', function (a) { Object.preventExtensions(a); a[0] = 'x'; a[50] = 'y'; }],
	['setProto', function (a) { Object.setPrototypeOf(a, {0: 'p'}); a[0] = 'x'; }],
	['reflectSet', function (a) { Reflect.set(a, 3, 'x'); }],
	['reflectDefine', function (a) { Reflect.defineProperty(a, 3, {value: 'x'}); }],
	['reflectDelete', function (a) { Reflect.deleteProperty(a, 3); }],
	['assign', function (a) { Object.assign(a, {0: 'x', 2: 'y'}); }],
	['forInWrite', function (a) { for (var k in a) a[k] = k; }],
	['forOfWrite', function (a) { var i = 0; for (var v of a) a[i++] = typeof v; }],
	['named', function (a) { a.foo = 1; a[0] = 'x'; }],
	['manyNamed', function (a) { for (var i = 0; i < 70; i++) a['k' + i] = i; a[1] = 'x'; }],
	['apply', function (a) { Array.prototype.push.apply(a, a); }],
	['concatThenWrite', function (a) { var c = a.concat(a); c[0] = 'x'; a[1] = 'y'; }],
	['sliceThenWrite', function (a) { var c = a.slice(); c[0] = 'x'; }],
	['spreadThenWrite', function (a) { var c = [...a]; c[0] = 'x'; }],
	['mapIndex', function (a) { a.map(function (v, i, arr) { arr[i] = 'm'; }); }],
	['forEachIndex', function (a) { a.forEach(function (v, i, arr) { if (i === 0) arr[13] = 'e'; }); }],
	['typed', function (a) { var t = new Float64Array(a); t[0] = 7; }],
	['keys', function (a) { Object.keys(a); a[0] = 'x'; }],
	['json', function (a) { JSON.stringify(a); a[0] = 'x'; }],
	['join', function (a) { a.join('-'); a[0] = 'x'; }],
	['includes', function (a) { a.includes(3); a.indexOf(5); a[0] = 'x'; }],
];
for (var m = 0; m < muts.length; m++) {
	var name = muts[m][0], f = muts[m][1];
	var before = lit(), a = lit(), b = lit();
	f(a);
	f(b);
	var r1 = show(before), r2 = show(a), r3 = show(b), r4 = show(lit());
	out.push(name + ': ' + r2 + ' | ' + (r3 === r2 ? 'same' : r3) + ' | ' + r1 + ' | ' + (r4 === r1 ? 'same' : 'CHANGED ' + r4));
}
// Strict writes to a frozen literal throw; the next literal is unchanged.
var fz = Object.freeze(strictLit());
out.push('strict: ' + err(function () { 'use strict'; fz[0] = 9; }) + ' ' + err(function () { 'use strict'; fz.push(9); }) + ' ' + show(strictLit()));
// Reads that leave the constants shared see the same values.
var r = lit();
out.push('reads: ' + r.indexOf(5) + ' ' + r.includes('ten') + ' ' + r.lastIndexOf(null) + ' ' + r.join('/') + ' ' + JSON.stringify(r) +
	' ' + r.slice(8, 11).join() + ' ' + r.concat([0]).length + ' ' + [...r].length + ' ' + Math.max.apply(null, r.slice(0, 9)) +
	' ' + r.toReversed()[0] + ' ' + r.with(0, 'w')[0] + ' ' + r.toSorted()[0] + ' ' + r.toSpliced(0, 1)[0] + ' ' + r.at(-1) + ' ' + r[9]);
var it = 0; for (var v of lit()) it++;
var sum = 0; for (var i = 0; i < r.length; i++) if (typeof r[i] === 'number') sum += r[i];
out.push('iter: ' + it + ' ' + sum + ' ' + show(lit()));
out.join('\n');
`

// TestSharedConstsNode runs sharedConstsJS: the expected results are
// Node 24's.
func TestSharedConstsNode(t *testing.T) {
	assert.Equal(t, `set: "x",2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
setLast: 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,"x" #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
setPast: 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5,"x" #15 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
setFar: 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5,hole,hole,hole,hole,hole,hole,hole,hole,hole,hole,hole,hole,hole,hole,hole,hole,hole,hole,hole,hole,hole,hole,hole,hole,hole,hole,"x" #41 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
inc: 1,3,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
compound: 1,2,13,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
push: 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5,"x","y" #16 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
pop: 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0 #13 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
shift: 2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #13 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
unshift: "x",1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #15 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
spliceDel: 1,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #12 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
spliceIns: 1,"x","y",2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #16 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
spliceRep: 1,2,3,"x",5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
reverse: 1.5,-0,undefined,null,"ten",9,8,7,6,5,4,3,2,1 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
sort: -0,1,1.5,2,3,4,5,6,7,8,9,null,"ten",undefined #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
sortFn: "ten",null,9,8,7,6,5,4,3,2,1.5,1,-0,undefined #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
fill: 1,2,0,0,0,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
fillAll: "f","f","f","f","f","f","f","f","f","f","f","f","f","f" #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
copyWithin: 6,7,8,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
truncate: 1,2,3 #3 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
extend: 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5,hole,hole,hole,hole,"x",hole #20 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
zero: "x" #1 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
delete: 1,2,3,4,hole,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
defineIndex: 1,2,3,4,5,"d",7,8,9,"ten",null,undefined,-0,1.5 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
defineGetter: 1,2,3,4,5,6,"g",8,9,"ten",null,undefined,-0,1.5 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
defineLength: 1,2,3,4 #4 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
freeze: 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
seal: "x",2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
preventExtensions: "x",2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
setProto: "x",2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
reflectSet: 1,2,3,"x",5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
reflectDefine: 1,2,3,"x",5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
reflectDelete: 1,2,3,hole,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
assign: "x",2,"y",4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
forInWrite: "0","1","2","3","4","5","6","7","8","9","10","11","12","13" #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
forOfWrite: "number","number","number","number","number","number","number","number","number","string","object","undefined","number","number" #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
named: "x",2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
manyNamed: 1,"x",3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
apply: 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5,1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #28 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
concatThenWrite: 1,"y",3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
sliceThenWrite: 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
spreadThenWrite: 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
mapIndex: "m","m","m","m","m","m","m","m","m","m","m","m","m","m" #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
forEachIndex: 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,"e" #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
typed: 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
keys: "x",2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
json: "x",2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
join: "x",2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
includes: "x",2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same | 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14 | same
strict: TypeError TypeError 1,2,3,4,5,6,7,8,9,10 #10
reads: 4 true 10 1/2/3/4/5/6/7/8/9/ten///0/1.5 [1,2,3,4,5,6,7,8,9,"ten",null,null,0,1.5] 9,ten, 15 14 9 1.5 w 0 2 1.5 ten
iter: 14 46.5 1,2,3,4,5,6,7,8,9,"ten",null,undefined,-0,1.5 #14`, runScripts(t, sharedConstsJS))
}

// sharedConstsModule compiles src as a module in a fresh realm and
// returns the realm and its exports.
func sharedConstsModule(t *testing.T, src string) (*Realm, *ModuleEnv) {
	t.Helper()
	m, err := syntax.ParseModule("shared.js", src, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	r := NewRealm()
	env, err := r.EvaluateModule(code)
	require.NoError(t, err)
	return r, env
}

// TestSharedConstsHost writes arrays of a literal of constants through the
// Go API, the lazy properties of a host among them, after which a write
// from JavaScript takes the interpreter's fast path again, and freezes
// them: each gets a copy of its own, and the literal's other arrays and its
// next one keep its values.
func TestSharedConstsHost(t *testing.T) {
	r, env := sharedConstsModule(t, `export function lit() { return [1, 2, 3, 4, 5, 6, 7, 8, 9, 10]; }
		export function set(a, i, v) { a[i] = v; }`)
	lit, _ := env.GetBindingValue("lit")
	set, _ := env.GetBindingValue("set")
	fresh := func() *Object {
		v, err := r.Call(lit, Undefined(), nil)
		require.NoError(t, err)
		return v.AsObject()
	}
	show := func(o *Object) string {
		var s []string
		for i := range o.ArrayLength() {
			v, err := o.GetIndex(r, i)
			require.NoError(t, err)
			s = append(s, v.String())
		}
		return strings.Join(s, ",")
	}
	const want = "1,2,3,4,5,6,7,8,9,10"
	lazy := StringKey(r.InternGoString("lazy"))
	for i, write := range []func(o *Object){
		func(o *Object) { _, _ = o.Set(r, IndexKey(0), IntValue(0), ObjectValue(o)) },
		func(o *Object) { o.Push(r, IntValue(11)) },
		func(o *Object) { o.Elements()[1] = IntValue(0) },
		func(o *Object) { _, _ = o.DefineOwnProperty(r, IndexKey(2), DataDescriptor(IntValue(0), attrDefault)) },
		func(o *Object) { o.Delete(r, IndexKey(3)) },
		func(o *Object) { o.Freeze(r) },
		func(o *Object) { o.PreventExtensions(r) },
		func(o *Object) {
			// The host's lazy property, defined, leaves an ordinary array
			// whose elements run's SetElem writes in place.
			o.DefineLazyProperty(r, lazy, func(*Realm) Value { return IntValue(1) })
			_, _ = o.GetProp(r, lazy)
			require.Zero(t, o.flags&flagHasLazy)
			for range 3 {
				_, err := r.Call(set, Undefined(), []Value{ObjectValue(o), IntValue(4), IntValue(0)})
				require.NoError(t, err)
			}
		},
	} {
		o, other := fresh(), fresh()
		require.NotZero(t, o.bits&bitSharedElems, "case %d", i)
		write(o)
		assert.Zero(t, o.bits&bitSharedElems, "case %d", i)
		assert.Equal(t, want, show(other), "case %d", i)
		assert.Equal(t, want, show(fresh()), "case %d", i)
	}
	// Arrays only read keep sharing the constants: the host's reads walk
	// them as any dense array.
	o := fresh()
	_, err := r.Call(set, Undefined(), []Value{ObjectValue(fresh()), IntValue(0), IntValue(0)})
	require.NoError(t, err)
	assert.Equal(t, want, show(o))
	wantGo := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	var ints, into []int
	complete, err := r.Unmarshal(ObjectValue(o), &ints)
	require.NoError(t, err)
	assert.True(t, complete)
	assert.Equal(t, wantGo, ints)
	assert.True(t, r.ToGoInto(ObjectValue(o), &into))
	assert.Equal(t, wantGo, into)
	assert.Equal(t, []any{int64(1), int64(2), int64(3), int64(4), int64(5), int64(6), int64(7), int64(8), int64(9), int64(10)}, r.ToGo(ObjectValue(o)))
	assert.NotZero(t, o.bits&bitSharedElems)
}

// TestSharedConstsAllocs checks the allocations of array literals of
// constants: one for an array of its own up to sharedConstsMin elements,
// one, the header, for an array sharing its constants, and one more, its
// copy, when it is written.
func TestSharedConstsAllocs(t *testing.T) {
	skipInRegcheck(t)
	r, env := sharedConstsModule(t, `export function k3() { return [1, "b", null]; }
		export function k8() { return [1, 2, 3, 4, 5, 6, 7, 8]; }
		export function k12() { return [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, "eleven", undefined]; }
		export function w12() { const a = [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, "eleven", undefined]; a[11] = 0; return a; }
		export function r12() { const a = [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, "eleven", undefined]; return a[3] + a.length + a.indexOf(10) + a.join().length; }`)
	for _, c := range []struct {
		fn     string
		allocs float64
	}{{"k3", 1}, {"k8", 1}, {"k12", 1}, {"w12", 2}, {"r12", 2}} { // r12: and the joined string
		f, _ := env.GetBindingValue(c.fn)
		_, err := r.Call(f, Undefined(), nil)
		require.NoError(t, err)
		assert.Equal(t, c.allocs, testing.AllocsPerRun(20, func() { _, _ = r.Call(f, Undefined(), nil) }), c.fn)
	}
}

// TestSharedConstsDynamic checks that only code that is not dynamic shares
// a literal's constants: eval and Function code, a function inside it too,
// copies them, so its arrays keep no constant slab of a tree the realm may
// drop.
func TestSharedConstsDynamic(t *testing.T) {
	SetCompiler(compiler.Hook{})
	t.Cleanup(func() { SetCompiler(nil) })
	r, env := sharedConstsModule(t, `const src = "[1, 2, 3, 4, 5, 6, 7, 8, 9, 'ten']";
		export function script() { return [1, 2, 3, 4, 5, 6, 7, 8, 9, 'ten']; }
		export function direct() { return eval(src); }
		export function indirect() { return (0, eval)(src); }
		export const fn = new Function("return " + src);
		export const inner = (0, eval)("(function () { return () => " + src + "; })()");`)
	want := []any{int64(1), int64(2), int64(3), int64(4), int64(5), int64(6), int64(7), int64(8), int64(9), "ten"}
	for _, c := range []struct {
		fn     string
		shared bool
	}{{"script", true}, {"direct", false}, {"indirect", false}, {"fn", false}, {"inner", false}} {
		f, _ := env.GetBindingValue(c.fn)
		for range 2 {
			v, err := r.Call(f, Undefined(), nil)
			require.NoError(t, err)
			assert.Equal(t, c.shared, v.AsObject().bits&bitSharedElems != 0, c.fn)
			assert.Equal(t, want, r.ToGo(v), c.fn)
		}
	}
}

// TestSharedConstsCharge checks that an array sharing the constants of its
// literal is charged, and measured live, as one that owns its elements,
// so the copy a write gives it needs no charge; and that a program keeping
// such arrays meets its memory limit.
func TestSharedConstsCharge(t *testing.T) {
	r := NewRealm()
	r.SetMemoryLimit(1 << 30)
	runScriptsIn(t, r, `var x = 1, last;
		function fk() { return [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, "eleven", null]; }
		function fw() { var a = fk(); a[0] = 0; return a; }
		function fv() { return [x, x, x, x, x, x, x, x, x, x, x, x]; }`)
	charge := func(fn string) int64 {
		before := r.Stats().AllocatedBytes
		runScriptsIn(t, r, "for (var i = 0; i < 100; i++) last = "+fn+"();")
		return r.Stats().AllocatedBytes - before
	}
	charge("fk")
	charge("fw")
	charge("fv")
	ck, cw, cv := charge("fk"), charge("fw"), charge("fv")
	require.Positive(t, ck)
	assert.Equal(t, cv, ck)
	assert.Equal(t, ck, cw)

	live := func(fn string) int64 {
		r := NewRealm()
		runScriptsIn(t, r, `function fk() { return [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, "eleven", null]; }
			function fw() { var a = fk(); a[0] = 0; return a; }
			var keep = []; for (var i = 0; i < 1000; i++) keep.push(`+fn+`());`)
		return r.MeasureLive(nil)
	}
	assert.Equal(t, live("fw"), live("fk"))

	r = NewRealmWith(RealmOptions{MemoryLimit: 1 << 20})
	_, err := runScriptValue(t, r, `const a = []; for (;;) a.push([1, 2, 3, 4, 5, 6, 7, 8, 9, 10, "eleven", null])`)
	assert.ErrorIs(t, err, ErrMemoryLimit)
}
