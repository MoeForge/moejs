//go:build moejsregcheck

package engine

// The checked build's payload accessors (value_as.go): a value that holds
// a payload of another kind panics instead of being read as this one. The
// engine takes the kind of some operands from the compiler (a class's
// prototype, a generator, a private name); bytecode no compiler made, as
// FuzzBytecode's, can hand it another, which the fast build reads as the
// wrong memory. A value with no payload (a number, undefined) still gives
// nil, which some callers compare with an object.

func (v Value) importTarget() *Value {
	checkPayload(v, tagImport, "importTarget")
	return (*Value)(v.ptr)
}

func (v Value) asPrivate() *PrivateName {
	checkPayload(v, tagPrivate, "asPrivate")
	return (*PrivateName)(v.ptr)
}

func (v Value) asAsyncFrame() *asyncFrame {
	checkPayload(v, tagAsync, "asAsyncFrame")
	return (*asyncFrame)(v.ptr)
}

// AsString returns the *String payload. Only valid when IsString.
func (v Value) AsString() *String { checkPayload(v, tagString, "AsString"); return (*String)(v.ptr) }

// AsObject returns the *Object payload. Only valid when IsObject.
func (v Value) AsObject() *Object { checkPayload(v, tagObject, "AsObject"); return (*Object)(v.ptr) }

// AsSymbol returns the *Symbol payload. Only valid when IsSymbol.
func (v Value) AsSymbol() *Symbol { checkPayload(v, tagSymbol, "AsSymbol"); return (*Symbol)(v.ptr) }

// AsBigInt returns the *BigInt payload. Only valid when IsBigInt.
func (v Value) AsBigInt() *BigInt { checkPayload(v, tagBigInt, "AsBigInt"); return (*BigInt)(v.ptr) }

func (v Value) asAccessor() *Accessor {
	checkPayload(v, tagAccessor, "asAccessor")
	return (*Accessor)(v.ptr)
}

// checkPayload panics when v holds a payload of another kind than tag.
func checkPayload(v Value, tag uint64, what string) {
	if v.ptr != nil && v.bits != tag {
		panic("engine: payload: " + what + " of a value of another kind")
	}
}
