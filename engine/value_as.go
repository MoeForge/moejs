//go:build !moejsregcheck

package engine

// The payload accessors. Each reads v.ptr as the kind its name says,
// whatever v holds: the caller checks the kind first, or knows it (the
// compiler gives some operands a kind, a class's prototype or a private
// name). value_as_regcheck.go is the checked build's.

// importTarget returns the slot an import reference points to. Only valid
// when isImportRef.
func (v Value) importTarget() *Value { return (*Value)(v.ptr) }

// asPrivate returns the *PrivateName payload. Only valid when isPrivate.
func (v Value) asPrivate() *PrivateName { return (*PrivateName)(v.ptr) }

// asAsyncFrame returns the *asyncFrame payload. Only valid when
// isAsyncFrame.
func (v Value) asAsyncFrame() *asyncFrame { return (*asyncFrame)(v.ptr) }

// AsString returns the *String payload. Only valid when IsString.
func (v Value) AsString() *String { return (*String)(v.ptr) }

// AsObject returns the *Object payload. Only valid when IsObject.
func (v Value) AsObject() *Object { return (*Object)(v.ptr) }

// AsSymbol returns the *Symbol payload. Only valid when IsSymbol.
func (v Value) AsSymbol() *Symbol { return (*Symbol)(v.ptr) }

// AsBigInt returns the *BigInt payload. Only valid when IsBigInt.
func (v Value) AsBigInt() *BigInt { return (*BigInt)(v.ptr) }

func (v Value) asAccessor() *Accessor { return (*Accessor)(v.ptr) }
