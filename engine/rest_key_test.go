package engine

import "testing"

// TestRestComputedKeyOnce: a computed key of an object pattern with a rest
// element is converted once (ToPropertyKey), whatever its target, and the
// rest excludes the key it converted to: a toString runs once and one that
// would give another key the second time cannot exclude the wrong one. In
// const, let and var declarations, assignments, parameters, nested
// patterns, with a default and at the top level, with symbol and number
// keys, in order with the source's getters. The expectations are Node's.
func TestRestComputedKeyOnce(t *testing.T) {
	runScriptCases(t, []scriptCase{
		{"restOnce", []string{`(function () { let n = 0; const k = {toString() { n++; return 'a' }}; const {[k]: x, ...r} = {a: 1, b: 2}; return [n, x, JSON.stringify(r)].join(); })()`}, "1,1,{\"b\":2}"},
		{"restSecondString", []string{`(function () { let n = 0; const k = {toString() { return n++ ? 'b' : 'a' }}; const {[k]: x, ...r} = {a: 1, b: 2}; return [n, x, JSON.stringify(r)].join(); })()`}, "1,1,{\"b\":2}"},
		{"restLetVar", []string{`(function () { let n = 0; const k = {toString() { n++; return 'a' }}; let {[k]: x, ...r} = {a: 1, b: 2}; var {[k]: y, ...q} = {a: 3, c: 4}; return [n, x, JSON.stringify(r), y, JSON.stringify(q)].join(); })()`}, "2,1,{\"b\":2},3,{\"c\":4}"},
		{"restAssign", []string{`(function () { let n = 0, x, r; const k = {toString() { n++; return 'a' }}; ({[k]: x, ...r} = {a: 1, b: 2}); return [n, x, JSON.stringify(r)].join(); })()`}, "1,1,{\"b\":2}"},
		{"restParam", []string{`(function () { let n = 0; const k = {toString() { n++; return 'a' }}; function f({[k]: x, ...r}) { return [x, JSON.stringify(r)]; } return [f({a: 1, b: 2}), n].join(); })()`}, "1,{\"b\":2},1"},
		{"restArrow", []string{`(function () { let n = 0; const k = {toString() { n++; return 'a' }}; const f = ({[k]: x, ...r}) => [x, JSON.stringify(r)]; return [f({a: 1, b: 2}), n].join(); })()`}, "1,{\"b\":2},1"},
		{"restMember", []string{`(function () { let n = 0; const k = {toString() { n++; return 'a' }}; const o = {}; let r; ({[k]: o.x, ...r} = {a: 1, b: 2}); return [n, o.x, JSON.stringify(r)].join(); })()`}, "1,1,{\"b\":2}"},
		{"restNested", []string{`(function () { let n = 0; const k = {toString() { n++; return 'a' }}; const {[k]: {y}, ...r} = {a: {y: 1}, b: 2}; return [n, y, JSON.stringify(r)].join(); })()`}, "1,1,{\"b\":2}"},
		{"restDefault", []string{`(function () { let n = 0; const k = {toString() { n++; return 'z' }}; const {[k]: x = 'dflt', ...r} = {a: 1, b: 2}; return [n, x, JSON.stringify(r)].join(); })()`}, "1,dflt,{\"a\":1,\"b\":2}"},
		{"restKinds", []string{`(function () { const s = Symbol('s'); const {[s]: x, [1]: one, ['b']: b, ...r} = {[s]: 1, 1: 'one', b: 2, c: 3}; return [x, one, b, JSON.stringify(r), Object.getOwnPropertySymbols(r).length].join(); })()`}, "1,one,2,{\"c\":3},0"},
		{"restOrder", []string{`(function () { const log = []; const k = {toString() { log.push('key'); return 'a' }};
			const src = {get a() { log.push('get a'); return 1 }, get b() { log.push('get b'); return 2 }};
			const {[k]: x, ...r} = src; return log.join(); })()`}, "key,get a,get b"},
		{"restThrows", []string{`(function () { const k = {toString() { throw new Error('key') }}; try { const {[k]: x, ...r} = {a: 1}; return 'no'; } catch (e) { return e.message; } })()`}, "key"},
		{"restTopLevel", []string{`let n = 0; const k = {toString() { n++; return 'a' }}; const {[k]: x, ...r} = {a: 1, b: 2}; [n, x, JSON.stringify(r)].join()`}, "1,1,{\"b\":2}"},
	})
}
