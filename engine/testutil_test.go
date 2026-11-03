package engine

import "github.com/MoeForge/moejs/bytecode"

// bytecodeStubFunction is a minimal compiled-function template (an empty
// body) used by tests of Call/Construct dispatch and IC allocation.
var bytecodeStubFunction = bytecode.Function{
	Name: "stub", Length: 2, Strict: true, Kind: bytecode.KindNormal,
	Code: []uint32{bytecode.EncodeABC(bytecode.RetUndef, 0, 0, 0)},
}

// testClosure creates a closure over code, treated as its own template
// tree, with no environment, as the interpreter creates one.
func testClosure(r *Realm, code *bytecode.Function) (*Object, *FunctionData) {
	m, err := r.metaFor(code)
	if err != nil {
		panic(err)
	}
	return r.newClosure(code, m, nil, Undefined())
}

// concatRealm backs concat; the string-representation tests have no realm of
// their own and never approach the length limit.
var concatRealm = NewRealm()

// concat is Realm.Concat for tests of the string representation.
func concat(a, b *String) *String {
	s, err := concatRealm.Concat(a, b)
	if err != nil {
		panic(err)
	}
	return s
}
