package engine

import "testing"

// TestEntriesEnumerableAtTurn: Object.entries and Object.values take the
// own string keys first, enumerable or not, and check each property at
// its turn: one a getter made enumerable before its turn is in, one it
// hid or deleted is not, one it added is not. Shape objects (the shape
// walk, while the shape holds and after a getter changed it), arrays,
// dictionaries and exotic objects. The expectations are Node's.
func TestEntriesEnumerableAtTurn(t *testing.T) {
	runScriptCases(t, []scriptCase{
		{"entriesTurn", []string{`const s = {a: 1, get b() { Object.defineProperty(s, 'c', {enumerable: true}); return 2 }};
			Object.defineProperty(s, 'c', {value: 3, enumerable: false, writable: true, configurable: true});
			s.d = 4;
			JSON.stringify(Object.entries(s))`}, "[[\"a\",1],[\"b\",2],[\"c\",3],[\"d\",4]]"},
		{"valuesTurn", []string{`const s = {a: 1, get b() { Object.defineProperty(s, 'c', {enumerable: true}); return 2 }};
			Object.defineProperty(s, 'c', {value: 3, enumerable: false, writable: true, configurable: true});
			s.d = 4;
			JSON.stringify(Object.values(s))`}, "[1,2,3,4]"},
		{"hiddenAtTurn", []string{`const s = {a: 1, get b() { Object.defineProperty(s, 'c', {enumerable: false}); return 2 }, c: 3, d: 4};
			JSON.stringify(Object.entries(s))`}, "[[\"a\",1],[\"b\",2],[\"d\",4]]"},
		{"deletedAdded", []string{`const s = {a: 1, get b() { delete s.c; s.e = 5; return 2 }, c: 3, d: 4};
			JSON.stringify(Object.entries(s))`}, "[[\"a\",1],[\"b\",2],[\"d\",4]]"},
		{"stillHidden", []string{`const s = {a: 1, get b() { s.e = 5; return 2 }};
			Object.defineProperty(s, 'c', {value: 3, enumerable: false}); s.d = 4;
			JSON.stringify(Object.entries(s))`}, "[[\"a\",1],[\"b\",2],[\"d\",4]]"},
		{"symbolsAndHidden", []string{`JSON.stringify(Object.entries(Object.defineProperty({a: 1, [Symbol('s')]: 2}, 'h', {value: 3})))`}, "[[\"a\",1]]"},
		{"arrayTurn", []string{`const a = [1, 2];
			Object.defineProperty(a, 'x', {value: 'x', enumerable: false, configurable: true, writable: true});
			Object.defineProperty(a, 0, {get() { Object.defineProperty(a, 'x', {enumerable: true}); return 'g' }, enumerable: true, configurable: true});
			JSON.stringify([Object.entries(a), Object.values(a)])`}, "[[[\"0\",\"g\"],[\"1\",2],[\"x\",\"x\"]],[\"g\",2,\"x\"]]"},
		{"dictTurn", []string{`const d = {};
			for (let i = 0; i < 100; i++) d['k' + i] = i;
			for (let i = 2; i < 100; i++) delete d['k' + i];
			Object.defineProperty(d, 'g', {get() { Object.defineProperty(d, 'h', {enumerable: true}); delete d.z; return 'g' }, enumerable: true, configurable: true});
			Object.defineProperty(d, 'h', {value: 'h', enumerable: false, configurable: true, writable: true});
			d.z = 'z';
			JSON.stringify([Object.entries(d), Object.keys(d)])`}, "[[[\"k0\",0],[\"k1\",1],[\"g\",\"g\"],[\"h\",\"h\"]],[\"k0\",\"k1\",\"g\",\"h\"]]"},
		{"exotic", []string{`JSON.stringify([Object.entries(new String('ab')), Object.values(new Uint8Array([1, 2])), Object.entries(Object.assign(function f() {}, {k: 1})), Object.entries([5, , 6])])`}, "[[[\"0\",\"a\"],[\"1\",\"b\"]],[1,2],[[\"k\",1]],[[\"0\",5],[\"2\",6]]]"},
	})
}
