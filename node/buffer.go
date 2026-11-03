package node

import (
	"bytes"
	"encoding/binary"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/MoeForge/moejs/engine"
)

// Buffer is a subclass of Uint8Array, as Node's: Buffer.prototype's
// prototype is Uint8Array.prototype, and Buffer's is Uint8Array, so a
// Buffer is a Uint8Array that has the methods below too. Each one has an
// ArrayBuffer of its own: there is no pool, so allocUnsafe returns zeroed
// memory and byteOffset is 0 but for a slice.

// kMaxLength is the largest Buffer: moejs's largest ArrayBuffer.
const kMaxLength = 1<<31 - 1

// inspectMaxBytes is buffer.INSPECT_MAX_BYTES.
const inspectMaxBytes = 50

type bufferClass struct {
	ctor, proto *engine.Object
}

// buffer returns the Buffer class, created on first use.
func (e *env) buffer() *bufferClass {
	if e.bufferCls != nil {
		return e.bufferCls
	}
	r := e.r
	x := e.intrinsics()
	proto := r.NewObjectWithProto(x.uint8ArrayProto)
	// Its hundred methods keep a shape, so that buf.method() keeps its
	// inline cache.
	proto.KeepShape()
	c := &bufferClass{proto: proto}
	e.bufferCls = c
	c.ctor = r.NewNativeConstructor(r.InternGoString("Buffer"), 3, func(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		return e.bufferCall(args, nil)
	}, func(r *engine.Realm, args []engine.Value, newTarget *engine.Object) (engine.Value, error) {
		return e.bufferCall(args, newTarget)
	})
	if x.uint8Array != nil {
		c.ctor.SetPrototypeOf(r, x.uint8Array)
	}
	c.ctor.DefineOwnDataFast(r, e.keys.prototype, engine.ObjectValue(proto), 0)
	proto.DefineOwnDataFast(r, e.keys.constructor, engine.ObjectValue(c.ctor), attrProto)

	static := func(name string, length int, fn engine.NativeFunc) { e.method(c.ctor, name, length, attrMethod, fn) }
	static("from", 3, e.bufferFrom)
	static("of", 0, e.bufferOf)
	static("alloc", 3, e.bufferAlloc)
	static("allocUnsafe", 1, e.bufferAllocUnsafe)
	static("allocUnsafeSlow", 1, e.bufferAllocUnsafe)
	static("isBuffer", 1, e.bufferIsBuffer)
	static("isEncoding", 1, e.bufferIsEncoding)
	static("byteLength", 2, e.bufferByteLength)
	static("compare", 2, e.bufferCompareStatic)
	static("concat", 2, e.bufferConcat)
	e.set(c.ctor, "poolSize", engine.IntValue(65536), attrMethod)

	m := func(name string, length int, fn engine.NativeFunc) { e.method(proto, name, length, attrMethod, fn) }
	m("toString", 3, e.bufferToString)
	m("toLocaleString", 3, e.bufferToString)
	m("write", 4, e.bufferWrite)
	m("slice", 2, e.bufferSlice)
	m("equals", 1, e.bufferEquals)
	m("compare", 5, e.bufferCompare)
	m("indexOf", 3, func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
		return e.bufferIndexOf(this, args, true)
	})
	m("lastIndexOf", 3, func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
		return e.bufferIndexOf(this, args, false)
	})
	m("includes", 3, func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
		v, err := e.bufferIndexOf(this, args, true)
		if err != nil {
			return undefined, err
		}
		return engine.Bool(v.AsNumber() != -1), nil
	})
	m("toJSON", 0, e.bufferToJSON)
	m("fill", 4, e.bufferFill)
	m("copy", 4, e.bufferCopy)
	m("swap16", 0, func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
		return e.bufferSwap(this, 2)
	})
	m("swap32", 0, func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
		return e.bufferSwap(this, 4)
	})
	m("swap64", 0, func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
		return e.bufferSwap(this, 8)
	})
	for _, rw := range intAccessors {
		rw := rw
		read := func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
			return e.bufferRead(this, engine.Arg(args, 0), rw)
		}
		write := func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
			return e.bufferWriteNum(this, engine.Arg(args, 0), engine.Arg(args, 1), rw)
		}
		for _, name := range rw.names {
			m("read"+name, 1, read)
			m("write"+name, 2, write)
		}
	}
	for _, signed := range []bool{false, true} {
		for _, le := range []bool{true, false} {
			signed, le := signed, le
			name := "UInt"
			if signed {
				name = "Int"
			}
			end := "BE"
			if le {
				end = "LE"
			}
			read := func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				return e.bufferReadVar(this, engine.Arg(args, 0), engine.Arg(args, 1), signed, le)
			}
			write := func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				return e.bufferWriteVar(this, args, signed, le)
			}
			m("read"+name+end, 2, read)
			m("write"+name+end, 3, write)
			if !signed {
				m("readUint"+end, 2, read)
				m("writeUint"+end, 3, write)
			}
		}
	}
	if sym := e.inspectSymbol(); sym != nil {
		f := r.NewNativeFunction(r.InternGoString("inspect"), 2, e.bufferInspect)
		proto.DefineOwnDataFast(r, engine.SymbolKey(sym), engine.ObjectValue(f), attrProto)
	}
	return c
}

// inspectSymbol returns Symbol.for('nodejs.util.inspect.custom').
func (e *env) inspectSymbol() *engine.Symbol {
	if e.inspectCustom == nil {
		if f := e.intrinsics().symbolFor; f != nil {
			v, err := e.r.CallObject(f, undefined, []engine.Value{str("nodejs.util.inspect.custom")})
			if err == nil && v.IsSymbol() {
				e.inspectCustom = v.AsSymbol()
			}
		}
	}
	return e.inspectCustom
}

// bufferModule returns the binding of node:buffer.
func (e *env) bufferModule() *engine.Object {
	if e.bufferMod != nil {
		return e.bufferMod
	}
	r := e.r
	o := r.NewObject()
	e.bufferMod = o
	e.set(o, "Buffer", engine.ObjectValue(e.buffer().ctor), attrMethod)
	consts := r.NewObject()
	e.set(consts, "MAX_LENGTH", engine.IntValue(kMaxLength), attrMethod)
	e.set(consts, "MAX_STRING_LENGTH", engine.IntValue(maxStringLength), attrMethod)
	e.set(o, "constants", engine.ObjectValue(consts), attrMethod)
	e.set(o, "kMaxLength", engine.IntValue(kMaxLength), attrMethod)
	e.set(o, "kStringMaxLength", engine.IntValue(maxStringLength), attrMethod)
	e.set(o, "INSPECT_MAX_BYTES", engine.IntValue(inspectMaxBytes), attrMethod)
	e.method(o, "isUtf8", 1, attrMethod, func(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		b, err := e.viewBytes(engine.Arg(args, 0), "input")
		if err != nil {
			return undefined, err
		}
		return engine.Bool(utf8.Valid(b)), nil
	})
	e.method(o, "isAscii", 1, attrMethod, func(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		b, err := e.viewBytes(engine.Arg(args, 0), "input")
		if err != nil {
			return undefined, err
		}
		return engine.Bool(isASCII(b)), nil
	})
	for _, name := range []string{"atob", "btoa"} {
		if v, err := r.Global.GetProp(r, engine.StringKey(r.InternGoString(name))); err == nil {
			e.set(o, name, v, attrMethod)
		}
	}
	return o
}

// --- creating Buffers ---------------------------------------------------------------

// newBuffer returns a Buffer over data, which it does not copy.
func (e *env) newBuffer(data []byte) (engine.Value, error) {
	if e.intrinsics().uint8Array == nil {
		return undefined, e.r.TypeError("The runtime has no Uint8Array")
	}
	o, err := e.r.NewUint8Array(data, e.buffer().proto)
	if err != nil {
		return undefined, err
	}
	return engine.ObjectValue(o), nil
}

// view returns a Buffer (or the class newTarget's object) over n bytes of
// the ArrayBuffer ab from off.
func (e *env) view(ab *engine.Object, off, n int, newTarget *engine.Object) (engine.Value, error) {
	c := e.buffer()
	u8 := e.intrinsics().uint8Array
	if u8 == nil {
		return undefined, e.r.TypeError("The runtime has no Uint8Array")
	}
	if newTarget == nil {
		newTarget = c.ctor
	}
	return e.r.Construct(engine.ObjectValue(u8), []engine.Value{engine.ObjectValue(ab), engine.IntValue(off), engine.IntValue(n)}, newTarget)
}

// viewOf is view with the offset and length as JavaScript values.
func (e *env) viewOf(ab *engine.Object, off, n engine.Value, newTarget *engine.Object) (engine.Value, error) {
	c := e.buffer()
	u8 := e.intrinsics().uint8Array
	if u8 == nil {
		return undefined, e.r.TypeError("The runtime has no Uint8Array")
	}
	if newTarget == nil {
		newTarget = c.ctor
	}
	return e.r.Construct(engine.ObjectValue(u8), []engine.Value{engine.ObjectValue(ab), off, n}, newTarget)
}

// alloc returns a zeroed Buffer of n bytes.
func (e *env) alloc(n int) (engine.Value, error) {
	if err := e.r.ChargeMemory(n); err != nil {
		return undefined, err
	}
	return e.newBuffer(make([]byte, n))
}

// bufferCall implements Buffer(arg, encodingOrOffset, length), called or
// constructed: a number allocates, anything else is Buffer.from. The
// species constructor of a Buffer's subarray and map comes here too, with
// an ArrayBuffer or a length.
func (e *env) bufferCall(args []engine.Value, newTarget *engine.Object) (engine.Value, error) {
	a := engine.Arg(args, 0)
	if a.IsNumber() {
		n, err := e.sizeArg(a)
		if err != nil {
			return undefined, err
		}
		if err := e.r.ChargeMemory(n); err != nil {
			return undefined, err
		}
		ab, err := e.r.NewArrayBuffer(make([]byte, n))
		if err != nil {
			return undefined, err
		}
		return e.view(ab, 0, n, newTarget)
	}
	return e.from(a, engine.Arg(args, 1), engine.Arg(args, 2), newTarget)
}

// sizeArg validates the size of alloc.
func (e *env) sizeArg(v engine.Value) (int, error) {
	if !v.IsNumber() {
		return 0, e.argTypeError("size", "of type number", v)
	}
	f := v.AsNumber()
	if !(f >= 0 && f <= kMaxLength) {
		return 0, e.outOfRange("size", ">= 0 && <= "+itoa(kMaxLength), v)
	}
	return int(f), nil
}

func (e *env) bufferFrom(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
	return e.from(engine.Arg(args, 0), engine.Arg(args, 1), engine.Arg(args, 2), nil)
}

// from implements Buffer.from(value, encodingOrOffset, length).
func (e *env) from(value, encOrOff, length engine.Value, newTarget *engine.Object) (engine.Value, error) {
	r := e.r
	if value.IsString() {
		return e.fromString(value.AsString(), encOrOff, newTarget)
	}
	if value.IsObject() && !value.AsObject().IsCallable() {
		o := value.AsObject()
		if o.Class() == engine.ClassArrayBuffer || o.Class() == engine.ClassSharedArrayBuffer {
			return e.fromArrayBuffer(o, encOrOff, length, newTarget)
		}
		valueOf, err := o.GetProp(r, engine.StringKey(r.InternGoString("valueOf")))
		if err != nil {
			return undefined, err
		}
		if isFunction(valueOf) {
			v, err := r.Call(valueOf, value, nil)
			if err != nil {
				return undefined, err
			}
			if !v.IsNullish() && !(v.IsObject() && v.AsObject() == o) && (v.IsString() || v.IsObject()) {
				return e.from(v, encOrOff, length, newTarget)
			}
		}
		if b, ok, err := e.fromObject(o, newTarget); err != nil || ok {
			return b, err
		}
		prim, err := o.GetProp(r, engine.SymbolKey(engine.SymToPrimitive))
		if err != nil {
			return undefined, err
		}
		if isFunction(prim) {
			v, err := r.Call(prim, value, []engine.Value{str("string")})
			if err != nil {
				return undefined, err
			}
			if v.IsString() {
				return e.fromString(v.AsString(), encOrOff, newTarget)
			}
		}
	}
	return undefined, e.argTypeError("first argument",
		"of type string or an instance of Buffer, ArrayBuffer, or Array or an Array-like Object", value)
}

// encodingOf reads the encoding argument of a method: a string Node's
// getEncodingOps knows, ERR_UNKNOWN_ENCODING otherwise.
func (e *env) encodingOf(v engine.Value) (encoding, error) {
	s, err := e.r.ToString(v)
	if err != nil {
		return 0, err
	}
	name := s.GoString()
	if enc, ok := parseEncoding(name); ok && name != "" {
		return enc, nil
	}
	return 0, e.throwCoded(engine.KindTypeError, "ERR_UNKNOWN_ENCODING", "Unknown encoding: "+name)
}

func (e *env) fromString(s *engine.String, encV engine.Value, newTarget *engine.Object) (engine.Value, error) {
	enc := encUTF8
	if encV.IsString() && encV.AsString().Len() > 0 {
		var err error
		if enc, err = e.encodingOf(encV); err != nil {
			return undefined, err
		}
	}
	if err := e.r.ChargeMemory(byteLength(enc, s)); err != nil {
		return undefined, err
	}
	data := encode(enc, s)
	ab, err := e.r.NewArrayBuffer(data)
	if err != nil {
		return undefined, err
	}
	return e.view(ab, 0, len(data), newTarget)
}

func (e *env) fromArrayBuffer(ab *engine.Object, offV, lenV engine.Value, newTarget *engine.Object) (engine.Value, error) {
	r := e.r
	off := 0.0
	if !offV.IsUndefined() {
		f, err := r.ToNumber(offV)
		if err != nil {
			return undefined, err
		}
		if f == f {
			off = f
		}
	}
	data, _ := ab.BufferData()
	max := float64(len(data)) - off
	if max < 0 {
		return undefined, e.throwCoded(engine.KindRangeError, "ERR_BUFFER_OUT_OF_BOUNDS", `"offset" is outside of buffer bounds`)
	}
	n := max
	if !lenV.IsUndefined() {
		f, err := r.ToNumber(lenV)
		if err != nil {
			return undefined, err
		}
		if f > 0 {
			if f > max {
				return undefined, e.throwCoded(engine.KindRangeError, "ERR_BUFFER_OUT_OF_BOUNDS", `"length" is outside of buffer bounds`)
			}
			n = f
		} else {
			n = 0
		}
	}
	// The Uint8Array constructor checks the offset and length, a
	// negative or fractional offset included, as it does in Node.js.
	return e.viewOf(ab, engine.NumberValue(off), engine.NumberValue(n), newTarget)
}

// fromObject implements Node's fromObject: an array-like, or the JSON of
// a Buffer ({ type: 'Buffer', data: [...] }).
func (e *env) fromObject(o *engine.Object, newTarget *engine.Object) (engine.Value, bool, error) {
	r := e.r
	if o.Class() == engine.ClassTypedArray {
		return e.fromArrayLike(o, newTarget)
	}
	lv, err := o.GetProp(r, e.keys.length)
	if err != nil {
		return undefined, false, err
	}
	bv, err := o.GetProp(r, e.keys.buffer)
	if err != nil {
		return undefined, false, err
	}
	isAB := bv.IsObject() && (bv.AsObject().Class() == engine.ClassArrayBuffer || bv.AsObject().Class() == engine.ClassSharedArrayBuffer)
	if !lv.IsUndefined() || isAB {
		if !lv.IsNumber() {
			b, err := e.allocFor(0, newTarget)
			return b, true, err
		}
		return e.fromArrayLike(o, newTarget)
	}
	tv, err := o.GetProp(r, e.keys.typ)
	if err != nil {
		return undefined, false, err
	}
	if tv.IsString() && tv.AsString().GoString() == "Buffer" {
		dv, err := o.GetProp(r, e.keys.data)
		if err != nil {
			return undefined, false, err
		}
		if ok, err := r.IsArray(dv); err != nil || !ok {
			return undefined, false, err
		}
		return e.fromArrayLike(dv.AsObject(), newTarget)
	}
	return undefined, false, nil
}

// allocFor allocates n zero bytes for the class newTarget.
func (e *env) allocFor(n int, newTarget *engine.Object) (engine.Value, error) {
	if err := e.r.ChargeMemory(n); err != nil {
		return undefined, err
	}
	ab, err := e.r.NewArrayBuffer(make([]byte, n))
	if err != nil {
		return undefined, err
	}
	return e.view(ab, 0, n, newTarget)
}

// fromArrayLike copies an array-like's elements, each converted as a
// Uint8Array element is.
func (e *env) fromArrayLike(o *engine.Object, newTarget *engine.Object) (engine.Value, bool, error) {
	r := e.r
	if o.Class() == engine.ClassTypedArray {
		if elems, ok := o.TypedArrayElements(); ok {
			if b, ok := elems.([]uint8); ok {
				v, err := e.copyOf(b, newTarget)
				return v, true, err
			}
		}
	}
	n, err := r.LengthOfArrayLike(o)
	if err != nil {
		return undefined, true, err
	}
	if n <= 0 {
		v, err := e.allocFor(0, newTarget)
		return v, true, err
	}
	if n > kMaxLength {
		return undefined, true, r.RangeError("Array buffer allocation failed")
	}
	if err := r.ChargeMemory(int(n)); err != nil {
		return undefined, true, err
	}
	data := make([]byte, n)
	for i := int64(0); i < n; i++ {
		if i&4095 == 4095 {
			if err := r.CheckInterrupt(); err != nil {
				return undefined, true, err
			}
		}
		v, err := o.GetProp(r, indexKey(r, i))
		if err != nil {
			return undefined, true, err
		}
		b, err := toUint8(r, v)
		if err != nil {
			return undefined, true, err
		}
		data[i] = b
	}
	ab, err := r.NewArrayBuffer(data)
	if err != nil {
		return undefined, true, err
	}
	v, err := e.view(ab, 0, len(data), newTarget)
	return v, true, err
}

// copyOf returns a Buffer holding a copy of b.
func (e *env) copyOf(b []byte, newTarget *engine.Object) (engine.Value, error) {
	if err := e.r.ChargeMemory(len(b)); err != nil {
		return undefined, err
	}
	data := append(make([]byte, 0, len(b)), b...)
	ab, err := e.r.NewArrayBuffer(data)
	if err != nil {
		return undefined, err
	}
	return e.view(ab, 0, len(data), newTarget)
}

func indexKey(r *engine.Realm, i int64) engine.PropertyKey {
	return r.KeyFromGoString(itoa(i))
}

// toUint8 converts v as a Uint8Array element store does.
func toUint8(r *engine.Realm, v engine.Value) (byte, error) {
	if v.IsNumber() {
		return f2u8(v.AsNumber()), nil
	}
	f, err := r.ToNumber(v)
	if err != nil {
		return 0, err
	}
	return f2u8(f), nil
}

func f2u8(f float64) byte {
	if f != f || math.IsInf(f, 0) {
		return 0
	}
	return byte(int64(math.Mod(math.Trunc(f), 256)))
}

func (e *env) bufferOf(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
	data := make([]byte, len(args))
	for i, v := range args {
		b, err := toUint8(r, v)
		if err != nil {
			return undefined, err
		}
		data[i] = b
	}
	if err := r.ChargeMemory(len(data)); err != nil {
		return undefined, err
	}
	return e.newBuffer(data)
}

func (e *env) bufferAlloc(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
	n, err := e.sizeArg(engine.Arg(args, 0))
	if err != nil {
		return undefined, err
	}
	b, err := e.alloc(n)
	if err != nil {
		return undefined, err
	}
	fill := engine.Arg(args, 1)
	if !fill.IsUndefined() && !(fill.IsNumber() && fill.AsNumber() == 0) && n > 0 {
		return e.fill(b, fill, engine.IntValue(0), engine.IntValue(n), engine.Arg(args, 2), true)
	}
	return b, nil
}

func (e *env) bufferAllocUnsafe(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
	n, err := e.sizeArg(engine.Arg(args, 0))
	if err != nil {
		return undefined, err
	}
	return e.alloc(n)
}

func (e *env) bufferIsBuffer(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
	v := engine.Arg(args, 0)
	if !v.IsObject() {
		return engine.Bool(false), nil
	}
	ok, err := r.InstanceOf(v, engine.ObjectValue(e.buffer().ctor))
	return engine.Bool(ok), err
}

func (e *env) bufferIsEncoding(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
	v := engine.Arg(args, 0)
	if !v.IsString() || v.AsString().Len() == 0 {
		return engine.Bool(false), nil
	}
	_, ok := parseEncoding(v.AsString().GoString())
	return engine.Bool(ok), nil
}

func (e *env) bufferByteLength(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
	v := engine.Arg(args, 0)
	if !v.IsString() {
		if v.IsObject() {
			switch v.AsObject().Class() {
			case engine.ClassTypedArray, engine.ClassDataView, engine.ClassArrayBuffer, engine.ClassSharedArrayBuffer:
				b, _ := v.AsObject().BufferData()
				return engine.IntValue(len(b)), nil
			}
		}
		return undefined, e.argTypeError("string", "of type string or an instance of Buffer or ArrayBuffer", v)
	}
	s := v.AsString()
	if s.Len() == 0 {
		return engine.IntValue(0), nil
	}
	enc := encUTF8
	if ev := engine.Arg(args, 1); engine.ToBoolean(ev) {
		es, err := r.ToString(ev)
		if err != nil {
			return undefined, err
		}
		if x, ok := parseEncoding(es.GoString()); ok {
			enc = x
		}
	}
	return engine.IntValue(byteLength(enc, s)), nil
}

func (e *env) bufferCompareStatic(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
	a, err := e.u8Arg(engine.Arg(args, 0), "buf1")
	if err != nil {
		return undefined, err
	}
	b, err := e.u8Arg(engine.Arg(args, 1), "buf2")
	if err != nil {
		return undefined, err
	}
	return engine.IntValue(bytes.Compare(a, b)), nil
}

func (e *env) bufferConcat(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
	list := engine.Arg(args, 0)
	if ok, err := r.IsArray(list); err != nil {
		return undefined, err
	} else if !ok {
		return undefined, e.argTypeError("list", "an instance of Array", list)
	}
	lo := list.AsObject()
	n, err := r.LengthOfArrayLike(lo)
	if err != nil {
		return undefined, err
	}
	if n == 0 {
		return e.newBuffer(nil)
	}
	// As Node.js 24's: each item is checked, and nothing is kept for it,
	// so a list of 2**32 - 1 holes fails at its first.
	item := func(i int64) ([]byte, error) {
		if i&1023 == 1023 {
			if err := r.CheckInterrupt(); err != nil {
				return nil, err
			}
		}
		it, err := lo.GetProp(r, indexKey(r, i))
		if err != nil {
			return nil, err
		}
		return e.u8Arg(it, "list["+itoa(i)+"]")
	}
	var total int64
	lv := engine.Arg(args, 1)
	if lv.IsUndefined() {
		for i := int64(0); i < n; i++ {
			b, err := item(i)
			if err != nil {
				return undefined, err
			}
			total += int64(len(b))
		}
	} else {
		l, err := e.offsetArg(lv, "length", 0, maxSafe)
		if err != nil {
			return undefined, err
		}
		for i := int64(0); i < n; i++ {
			if _, err := item(i); err != nil {
				return undefined, err
			}
		}
		total = int64(l)
	}
	if total > kMaxLength {
		return undefined, r.RangeError("Array buffer allocation failed")
	}
	if err := r.ChargeMemory(int(total)); err != nil {
		return undefined, err
	}
	data := make([]byte, total)
	pos := 0
	for i := int64(0); i < n; i++ {
		b, err := item(i)
		if err != nil {
			return undefined, err
		}
		if lv.IsUndefined() && pos+len(b) > len(data) {
			// A getter gave a longer item the second time.
			return undefined, r.RangeError("offset is out of bounds")
		}
		pos += copy(data[pos:], b)
	}
	return e.newBuffer(data)
}

// maxSafe is Number.MAX_SAFE_INTEGER, the bound Node.js checks offsets
// against (its kMaxLength on 64-bit systems).
const maxSafe = 1<<53 - 1

// prefixFor returns the start of s that encodes to at least n bytes in
// enc, where that is cheap to tell, so that write and fill encode no more
// than they use; s itself otherwise.
func prefixFor(enc encoding, s *engine.String, n int) *engine.String {
	var units int
	switch enc {
	case encLatin1, encASCII:
		units = n
	case encUTF16LE:
		units = n/2 + 1
	case encHex:
		units = 2*n + 2
	case encUTF8:
		units = n + 1 // each unit is at least one byte
	default:
		return s
	}
	if units < s.Len() {
		return s.Substring(0, units)
	}
	return s
}

// --- receivers and arguments --------------------------------------------------------

// thisBytes returns the bytes of the receiver, a Uint8Array.
func (e *env) thisBytes(this engine.Value) ([]byte, error) {
	if this.IsObject() && this.AsObject().Class() == engine.ClassTypedArray {
		b, _ := this.AsObject().BufferData()
		return b, nil
	}
	return nil, e.r.TypeError("argument must be a buffer")
}

// u8Arg returns the bytes of a Buffer or Uint8Array argument.
func (e *env) u8Arg(v engine.Value, name string) ([]byte, error) {
	if v.IsObject() && v.AsObject().Class() == engine.ClassTypedArray && e.isUint8Array(v.AsObject()) {
		b, _ := v.AsObject().BufferData()
		return b, nil
	}
	return nil, e.argTypeError(name, "an instance of Buffer or Uint8Array", v)
}

// viewBytes returns the bytes of an ArrayBuffer or a typed array.
func (e *env) viewBytes(v engine.Value, name string) ([]byte, error) {
	if v.IsObject() {
		switch v.AsObject().Class() {
		case engine.ClassTypedArray, engine.ClassArrayBuffer, engine.ClassSharedArrayBuffer:
			b, _ := v.AsObject().BufferData()
			return b, nil
		}
	}
	return nil, e.argTypeError(name, "an instance of ArrayBuffer, Buffer, or TypedArray", v)
}

// offsetArg implements Node's validateOffset: an integer number in
// [min, max].
func (e *env) offsetArg(v engine.Value, name string, min, max int) (int, error) {
	if !v.IsNumber() {
		return 0, e.argTypeError(name, "of type number", v)
	}
	f := v.AsNumber()
	if f != math.Trunc(f) || math.IsInf(f, 0) {
		return 0, e.outOfRange(name, "an integer", v)
	}
	if f < float64(min) || f > float64(max) {
		return 0, e.outOfRange(name, ">= "+itoa(int64(min))+" && <= "+itoa(int64(max)), v)
	}
	return int(f), nil
}

// --- methods ------------------------------------------------------------------------

func (e *env) bufferToString(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
	if _, err := e.thisBytes(this); err != nil {
		return undefined, err
	}
	b, _ := this.AsObject().BufferData()
	if len(args) == 0 {
		return e.decodeValue(encUTF8, b)
	}
	n := len(b)
	start, end := 0, n
	if sv := engine.Arg(args, 1); !sv.IsUndefined() {
		f, err := r.ToNumber(sv)
		if err != nil {
			return undefined, err
		}
		switch {
		case f <= 0:
		case f >= float64(n):
			return engine.StringValue(engine.EmptyString()), nil
		case f == f:
			start = int(f)
		}
	}
	if ev := engine.Arg(args, 2); !ev.IsUndefined() {
		f, err := r.ToNumber(ev)
		if err != nil {
			return undefined, err
		}
		if !(f > float64(n)) {
			// Node: MathTrunc(end) || 0, NaN included.
			if !(f > 0) {
				f = 0
			}
			end = int(math.Trunc(f))
		}
	}
	if end <= start {
		return engine.StringValue(engine.EmptyString()), nil
	}
	enc := encUTF8
	if ev := engine.Arg(args, 0); !ev.IsUndefined() {
		var err error
		if enc, err = e.encodingOf(ev); err != nil {
			return undefined, err
		}
	}
	// The conversions may have run JavaScript.
	b, _ = this.AsObject().BufferData()
	end = min(end, len(b))
	if end <= start {
		return engine.StringValue(engine.EmptyString()), nil
	}
	return e.decodeValue(enc, b[start:end])
}

// decodeValue returns the string of the bytes b of a buffer, charged.
func (e *env) decodeValue(enc encoding, b []byte) (engine.Value, error) {
	if len(b) == 0 {
		return engine.StringValue(engine.EmptyString()), nil
	}
	if err := e.r.ChargeMemory(encodedSize(enc, b)); err != nil {
		return undefined, err
	}
	return engine.StringValue(decode(enc, b, false)), nil
}

func (e *env) bufferWrite(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
	b, err := e.thisBytes(this)
	if err != nil {
		return undefined, err
	}
	sv := engine.Arg(args, 0)
	if !sv.IsString() {
		return undefined, e.throwCoded(engine.KindTypeError, "ERR_INVALID_ARG_TYPE", "argument must be a string")
	}
	offV, lenV, encV := engine.Arg(args, 1), engine.Arg(args, 2), engine.Arg(args, 3)
	n := len(b)
	off, length := 0, n
	switch {
	case offV.IsUndefined():
	case lenV.IsUndefined() && offV.IsString():
		encV = offV
	default:
		if off, err = e.offsetArg(offV, "offset", 0, n); err != nil {
			return undefined, err
		}
		remaining := n - off
		switch {
		case lenV.IsUndefined():
			length = remaining
		case lenV.IsString():
			encV, length = lenV, remaining
		default:
			if length, err = e.offsetArg(lenV, "length", 0, n); err != nil {
				return undefined, err
			}
			length = min(length, remaining)
		}
	}
	enc := encUTF8
	if engine.ToBoolean(encV) {
		if enc, err = e.encodingOf(encV); err != nil {
			return undefined, err
		}
	}
	b, _ = this.AsObject().BufferData()
	if off > len(b) {
		return engine.IntValue(0), nil
	}
	dst := b[off:min(off+length, len(b))]
	s := sv.AsString()
	if enc == encUTF8 {
		return engine.IntValue(writeUTF8(dst, s)), nil
	}
	data := encode(enc, prefixFor(enc, s, len(dst)))
	if enc == encUTF16LE {
		data = data[:min(len(data), len(dst))&^1]
	}
	return engine.IntValue(copy(dst, data)), nil
}

// writeUTF8 writes the UTF-8 of s into dst, only whole characters, and
// returns how many bytes it wrote.
func writeUTF8(dst []byte, s *engine.String) int {
	if a, ok := s.ASCII(); ok {
		return copy(dst, a)
	}
	n, pos := s.Len(), 0
	var buf [4]byte
	for i := 0; i < n; i++ {
		c := rune(s.At(i))
		if c >= 0xd800 && c <= 0xdfff {
			if c < 0xdc00 && i+1 < n {
				if d := rune(s.At(i + 1)); d >= 0xdc00 && d <= 0xdfff {
					c = (c-0xd800)<<10 + (d - 0xdc00) + 0x10000
					i++
				} else {
					c = utf8.RuneError
				}
			} else {
				c = utf8.RuneError
			}
		}
		k := utf8.EncodeRune(buf[:], c)
		if pos+k > len(dst) {
			break
		}
		copy(dst[pos:], buf[:k])
		pos += k
	}
	return pos
}

// adjustOffset is Node's adjustOffset of slice.
func adjustOffset(f float64, length int) int {
	f = math.Trunc(f)
	switch {
	case f == 0 || f != f:
		return 0
	case f < 0:
		f += float64(length)
		if f > 0 {
			return int(f)
		}
		return 0
	case f < float64(length):
		return int(f)
	}
	return length
}

func (e *env) bufferSlice(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
	b, err := e.thisBytes(this)
	if err != nil {
		return undefined, err
	}
	n := len(b)
	sf, err := r.ToNumber(engine.Arg(args, 0))
	if err != nil {
		return undefined, err
	}
	start, end := adjustOffset(sf, n), n
	if ev := engine.Arg(args, 1); !ev.IsUndefined() {
		ef, err := r.ToNumber(ev)
		if err != nil {
			return undefined, err
		}
		end = adjustOffset(ef, n)
	}
	o := this.AsObject()
	abv, err := o.GetProp(r, e.keys.buffer)
	if err != nil {
		return undefined, err
	}
	offv, err := o.GetProp(r, e.keys.byteOffset)
	if err != nil {
		return undefined, err
	}
	if !abv.IsObject() || !offv.IsNumber() {
		return undefined, r.TypeError("argument must be a buffer")
	}
	return e.view(abv.AsObject(), int(offv.AsNumber())+start, max(end-start, 0), nil)
}

func (e *env) bufferEquals(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
	a, err := e.u8Arg(this, "this")
	if err != nil {
		return undefined, err
	}
	b, err := e.u8Arg(engine.Arg(args, 0), "otherBuffer")
	if err != nil {
		return undefined, err
	}
	return engine.Bool(bytes.Equal(a, b)), nil
}

func (e *env) bufferCompare(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
	src, err := e.thisBytes(this)
	if err != nil {
		return undefined, err
	}
	target, err := e.u8Arg(engine.Arg(args, 0), "target")
	if err != nil {
		return undefined, err
	}
	if len(args) == 1 {
		return engine.IntValue(bytes.Compare(src, target)), nil
	}
	arg := func(i int, name string, max, def int) (int, error) {
		v := engine.Arg(args, i)
		if v.IsUndefined() {
			return def, nil
		}
		return e.offsetArg(v, name, 0, max)
	}
	ts, err := arg(1, "targetStart", maxSafe, 0)
	if err != nil {
		return undefined, err
	}
	te, err := arg(2, "targetEnd", len(target), len(target))
	if err != nil {
		return undefined, err
	}
	ss, err := arg(3, "sourceStart", maxSafe, 0)
	if err != nil {
		return undefined, err
	}
	se, err := arg(4, "sourceEnd", len(src), len(src))
	if err != nil {
		return undefined, err
	}
	if ss >= se {
		if ts >= te {
			return engine.IntValue(0), nil
		}
		return engine.IntValue(-1), nil
	}
	if ts >= te {
		return engine.IntValue(1), nil
	}
	ss, ts = min(ss, se), min(ts, te)
	return engine.IntValue(bytes.Compare(src[ss:se], target[ts:te])), nil
}

// bufferIndexOf implements indexOf (forward) and lastIndexOf.
func (e *env) bufferIndexOf(this engine.Value, args []engine.Value, forward bool) (engine.Value, error) {
	r := e.r
	if _, err := e.thisBytes(this); err != nil {
		return undefined, err
	}
	val, offV, encV := engine.Arg(args, 0), engine.Arg(args, 1), engine.Arg(args, 2)
	if offV.IsString() {
		encV, offV = offV, undefined
	}
	off, err := r.ToNumber(offV)
	if err != nil {
		return undefined, err
	}
	off = math.Max(math.Min(off, 0x7fffffff), -0x80000000)
	b, _ := this.AsObject().BufferData()
	if off != off {
		if forward {
			off = 0
		} else {
			off = float64(len(b))
		}
	}
	offset := int64(math.Trunc(off))
	if val.IsNumber() {
		f := val.AsNumber()
		needle := byte(0)
		if f == f && !math.IsInf(f, 0) {
			needle = byte(uint32(int64(math.Mod(math.Trunc(f), 4294967296))))
		}
		o := indexOfOffset(len(b), offset, 1, forward)
		if o < 0 || len(b) == 0 {
			return engine.IntValue(-1), nil
		}
		if forward {
			i := bytes.IndexByte(b[o:], needle)
			if i < 0 {
				return engine.IntValue(-1), nil
			}
			return engine.IntValue(int(o) + i), nil
		}
		return engine.IntValue(bytes.LastIndexByte(b[:o+1], needle)), nil
	}
	enc := encUTF8
	if !encV.IsUndefined() {
		if val.IsString() {
			if enc, err = e.encodingOf(encV); err != nil {
				return undefined, err
			}
		}
	}
	var needle []byte
	switch {
	case val.IsString():
		b, _ = this.AsObject().BufferData()
		if s := val.AsString(); s.Len() > 0 && byteLength(enc, s) > len(b) && enc != encBase64 && enc != encBase64URL {
			// It cannot be in the buffer: no need to encode it.
			return engine.IntValue(-1), nil
		}
		needle = encode(enc, val.AsString())
	case val.IsObject() && val.AsObject().Class() == engine.ClassTypedArray && e.isUint8Array(val.AsObject()):
		nb, _ := val.AsObject().BufferData()
		needle = nb
	default:
		return undefined, e.argTypeError("value", "one of type number or string or an instance of Buffer or Uint8Array", val)
	}
	b, _ = this.AsObject().BufferData()
	o := indexOfOffset(len(b), offset, int64(len(needle)), forward)
	if len(needle) == 0 {
		return engine.IntValue(int(o)), nil
	}
	if len(b) == 0 || o < 0 || (forward && int(o)+len(needle) > len(b)) || len(needle) > len(b) {
		return engine.IntValue(-1), nil
	}
	if forward {
		i := bytes.Index(b[o:], needle)
		if i < 0 {
			return engine.IntValue(-1), nil
		}
		return engine.IntValue(int(o) + i), nil
	}
	end := min(int(o)+len(needle), len(b))
	return engine.IntValue(bytes.LastIndex(b[:end], needle)), nil
}

// indexOfOffset is Node's IndexOfOffset: where a search starts, or -1
// for none.
func indexOfOffset(length int, offset, needle int64, forward bool) int64 {
	l := int64(length)
	if offset < 0 {
		switch {
		case offset+l >= 0:
			return l + offset
		case forward || needle == 0:
			return 0
		}
		return -1
	}
	switch {
	case offset+needle <= l:
		return offset
	case needle == 0:
		return l
	case forward:
		return -1
	}
	return l - 1
}

func (e *env) bufferToJSON(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
	if !this.IsObject() || this.AsObject().Class() != engine.ClassTypedArray {
		return undefined, r.TypeError("Method get TypedArray.prototype.length called on incompatible receiver %s", r.DisplayString(this))
	}
	o := this.AsObject()
	var items []engine.Value
	if e.isUint8Array(o) {
		b, _ := o.BufferData()
		// The array takes 16 bytes an element: charged before it is made.
		if err := r.ChargeMemory(16 * len(b)); err != nil {
			return undefined, err
		}
		items = make([]engine.Value, len(b))
		for i, c := range b {
			items[i] = engine.IntValue(int(c))
		}
	} else {
		// Another typed array: its elements, as Node's this[i] reads them.
		n, err := r.LengthOfArrayLike(o)
		if err != nil {
			return undefined, err
		}
		if err := r.ChargeMemory(16 * int(n)); err != nil {
			return undefined, err
		}
		items = make([]engine.Value, n)
		for i := range items {
			if items[i], err = o.GetProp(r, indexKey(r, int64(i))); err != nil {
				return undefined, err
			}
		}
	}
	res := r.NewObject()
	res.DefineOwnDataFast(r, e.keys.typ, str("Buffer"), attrMethod)
	res.DefineOwnDataFast(r, e.keys.data, engine.ObjectValue(r.NewArrayFromSlice(items)), attrMethod)
	return engine.ObjectValue(res), nil
}

func (e *env) bufferFill(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
	if _, err := e.thisBytes(this); err != nil {
		return undefined, err
	}
	return e.fill(this, engine.Arg(args, 0), engine.Arg(args, 1), engine.Arg(args, 2), engine.Arg(args, 3), false)
}

// fill implements Node's _fill.
func (e *env) fill(buf, value, offV, endV, encV engine.Value, fromAlloc bool) (engine.Value, error) {
	r := e.r
	b, _ := buf.AsObject().BufferData()
	n := len(b)
	enc := encUTF8
	if value.IsString() {
		switch {
		case offV.IsUndefined() || offV.IsString():
			encV, offV, endV = offV, undefined, undefined
		case endV.IsString():
			encV, endV = endV, undefined
		}
		if !encV.IsNullish() && !(encV.IsString() && encV.AsString().Len() == 0) {
			if !encV.IsString() {
				return undefined, e.argTypeError("encoding", "of type string", encV)
			}
			var ok bool
			if enc, ok = parseEncoding(encV.AsString().GoString()); !ok {
				return undefined, e.throwCoded(engine.KindTypeError, "ERR_UNKNOWN_ENCODING", "Unknown encoding: "+encV.AsString().GoString())
			}
		}
	}
	start, end := 0, n
	if !offV.IsUndefined() {
		var err error
		if start, err = e.offsetArg(offV, "offset", 0, kMaxLength); err != nil {
			return undefined, err
		}
		if !endV.IsUndefined() {
			if end, err = e.offsetArg(endV, "end", 0, n); err != nil {
				return undefined, err
			}
		}
		if start >= end {
			return buf, nil
		}
		if start > n {
			return undefined, e.throwCoded(engine.KindRangeError, "ERR_BUFFER_OUT_OF_BOUNDS", "Attempt to access memory outside buffer bounds")
		}
	}
	var pattern []byte
	switch {
	case value.IsString():
		s := value.AsString()
		if s.Len() == 0 {
			pattern = []byte{0}
		} else if pattern = encode(enc, prefixFor(enc, s, end-start)); len(pattern) == 0 {
			return undefined, e.argValueError("value", value, "is invalid")
		}
	case value.IsObject() && value.AsObject().Class() == engine.ClassTypedArray:
		vb, _ := value.AsObject().BufferData()
		if len(vb) == 0 {
			return undefined, e.argValueError("value", value, "is invalid")
		}
		pattern = append([]byte(nil), vb...)
	default:
		f, err := r.ToNumber(value)
		if err != nil {
			return undefined, err
		}
		pattern = []byte{f2u8(f)}
	}
	// The conversions may have run JavaScript that shrank or detached
	// the buffer.
	b, _ = buf.AsObject().BufferData()
	end = min(end, len(b))
	if start >= end {
		return buf, nil
	}
	dst := b[start:end]
	for i := 0; i < len(dst); i += len(pattern) {
		copy(dst[i:], pattern)
	}
	return buf, nil
}

func (e *env) bufferCopy(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
	if !this.IsObject() || (this.AsObject().Class() != engine.ClassTypedArray && this.AsObject().Class() != engine.ClassDataView) {
		return undefined, e.argTypeError("source", "an instance of Buffer or Uint8Array", this)
	}
	tv := engine.Arg(args, 0)
	if !tv.IsObject() || (tv.AsObject().Class() != engine.ClassTypedArray && tv.AsObject().Class() != engine.ClassDataView) {
		return undefined, e.argTypeError("target", "an instance of Buffer or Uint8Array", tv)
	}
	toInt := func(v engine.Value, def int64) (int64, error) {
		f, err := r.ToNumber(v)
		if err != nil {
			return 0, err
		}
		if f != f || f < -9007199254740991 || f > 9007199254740991 {
			return def, nil
		}
		return int64(math.Floor(f)), nil
	}
	src, _ := this.AsObject().BufferData()
	var ts, ss int64
	se := int64(len(src))
	var err error
	if v := engine.Arg(args, 1); !v.IsUndefined() {
		if ts, err = toInt(v, 0); err != nil {
			return undefined, err
		}
		if ts < 0 {
			return undefined, e.outOfRange("targetStart", ">= 0", v)
		}
	}
	if v := engine.Arg(args, 2); !v.IsUndefined() {
		if ss, err = toInt(v, 0); err != nil {
			return undefined, err
		}
		if ss < 0 || ss > int64(len(src)) {
			return undefined, e.outOfRange("sourceStart", ">= 0 && <= "+itoa(int64(len(src))), v)
		}
	}
	if v := engine.Arg(args, 3); !v.IsUndefined() {
		if se, err = toInt(v, 0); err != nil {
			return undefined, err
		}
		if se < 0 {
			return undefined, e.outOfRange("sourceEnd", ">= 0", v)
		}
	}
	src, _ = this.AsObject().BufferData()
	dst, _ := tv.AsObject().BufferData()
	if ts >= int64(len(dst)) || ss >= se {
		return engine.IntValue(0), nil
	}
	if se-ss > int64(len(dst))-ts {
		se = ss + int64(len(dst)) - ts
	}
	nb := min(se-ss, int64(len(src))-ss)
	if nb <= 0 {
		return engine.IntValue(0), nil
	}
	copy(dst[ts:ts+nb], src[ss:ss+nb])
	return engine.IntValue(int(nb)), nil
}

func (e *env) bufferSwap(this engine.Value, size int) (engine.Value, error) {
	b, err := e.thisBytes(this)
	if err != nil {
		return undefined, err
	}
	if len(b)%size != 0 {
		return undefined, e.throwCoded(engine.KindRangeError, "ERR_INVALID_BUFFER_SIZE",
			"Buffer size must be a multiple of "+itoa(int64(size*8))+"-bits")
	}
	for i := 0; i < len(b); i += size {
		for j, k := i, i+size-1; j < k; j, k = j+1, k-1 {
			b[j], b[k] = b[k], b[j]
		}
	}
	return this, nil
}

func (e *env) bufferInspect(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
	b, err := e.thisBytes(this)
	if err != nil {
		return undefined, err
	}
	n := min(len(b), inspectMaxBytes)
	var sb strings.Builder
	const digits = "0123456789abcdef"
	for i, c := range b[:n] {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteByte(digits[c>>4])
		sb.WriteByte(digits[c&15])
	}
	if rest := len(b) - inspectMaxBytes; rest > 0 {
		sb.WriteString(" ... " + itoa(int64(rest)) + " more byte")
		if rest > 1 {
			sb.WriteByte('s')
		}
	}
	name := "Buffer"
	if c, err := this.AsObject().GetProp(r, e.keys.constructor); err == nil && c.IsObject() {
		if nv, err := c.AsObject().GetProp(r, e.keys.name); err == nil && nv.IsString() {
			name = nv.AsString().GoString()
		}
	}
	return str("<" + name + " " + sb.String() + ">"), nil
}

// --- numbers ------------------------------------------------------------------------

// numAccessor is a pair of read and write methods of a fixed size.
type numAccessor struct {
	names  []string // the method names after read and write
	size   int
	signed bool
	le     bool
	float  bool
}

var intAccessors = []numAccessor{
	{names: []string{"UInt8", "Uint8"}, size: 1},
	{names: []string{"Int8"}, size: 1, signed: true},
	{names: []string{"UInt16LE", "Uint16LE"}, size: 2, le: true},
	{names: []string{"UInt16BE", "Uint16BE"}, size: 2},
	{names: []string{"Int16LE"}, size: 2, signed: true, le: true},
	{names: []string{"Int16BE"}, size: 2, signed: true},
	{names: []string{"UInt32LE", "Uint32LE"}, size: 4, le: true},
	{names: []string{"UInt32BE", "Uint32BE"}, size: 4},
	{names: []string{"Int32LE"}, size: 4, signed: true, le: true},
	{names: []string{"Int32BE"}, size: 4, signed: true},
	{names: []string{"FloatLE"}, size: 4, le: true, float: true},
	{names: []string{"FloatBE"}, size: 4, float: true},
	{names: []string{"DoubleLE"}, size: 8, le: true, float: true},
	{names: []string{"DoubleBE"}, size: 8, float: true},
}

// readOffset validates the offset of a read or write of size bytes in a
// buffer of n: Node's validateNumber and boundsError.
func (e *env) readOffset(v engine.Value, size, n int) (int, error) {
	if v.IsUndefined() {
		v = engine.IntValue(0)
	}
	if !v.IsNumber() {
		return 0, e.argTypeError("offset", "of type number", v)
	}
	f := v.AsNumber()
	if f >= 0 && f == math.Trunc(f) && f+float64(size) <= float64(n) {
		return int(f), nil
	}
	if f != math.Floor(f) {
		return 0, e.outOfRange("offset", "an integer", v)
	}
	if n-size < 0 {
		return 0, e.throwCoded(engine.KindRangeError, "ERR_BUFFER_OUT_OF_BOUNDS", "Attempt to access memory outside buffer bounds")
	}
	return 0, e.outOfRange("offset", ">= 0 and <= "+itoa(int64(n-size)), v)
}

func (e *env) bufferRead(this engine.Value, offV engine.Value, a numAccessor) (engine.Value, error) {
	b, err := e.thisBytes(this)
	if err != nil {
		return undefined, err
	}
	off, err := e.readOffset(offV, a.size, len(b))
	if err != nil {
		return undefined, err
	}
	p := b[off : off+a.size]
	var u uint64
	switch a.size {
	case 1:
		u = uint64(p[0])
	case 2:
		if a.le {
			u = uint64(binary.LittleEndian.Uint16(p))
		} else {
			u = uint64(binary.BigEndian.Uint16(p))
		}
	case 4:
		if a.le {
			u = uint64(binary.LittleEndian.Uint32(p))
		} else {
			u = uint64(binary.BigEndian.Uint32(p))
		}
	case 8:
		if a.le {
			u = binary.LittleEndian.Uint64(p)
		} else {
			u = binary.BigEndian.Uint64(p)
		}
	}
	switch {
	case a.float && a.size == 4:
		return engine.NumberValue(float64(math.Float32frombits(uint32(u)))), nil
	case a.float:
		return engine.NumberValue(math.Float64frombits(u)), nil
	case a.signed:
		shift := 64 - 8*a.size
		return engine.Int64Value(int64(u<<shift) >> shift), nil
	}
	return engine.Int64Value(int64(u)), nil
}

func (e *env) bufferWriteNum(this, valV, offV engine.Value, a numAccessor) (engine.Value, error) {
	r := e.r
	if _, err := e.thisBytes(this); err != nil {
		return undefined, err
	}
	v, err := r.ToNumber(valV)
	if err != nil {
		return undefined, err
	}
	b, _ := this.AsObject().BufferData()
	if !a.float {
		lo, hi := 0.0, math.Ldexp(1, 8*a.size)-1
		if a.signed {
			lo, hi = -math.Ldexp(1, 8*a.size-1), math.Ldexp(1, 8*a.size-1)-1
		}
		if a.size == 1 {
			if !offV.IsUndefined() && !offV.IsNumber() {
				return undefined, e.argTypeError("offset", "of type number", offV)
			}
		}
		if v > hi || v < lo {
			var rng string
			switch {
			case a.size > 4 && !a.signed:
				rng = ">= 0 and < 2 ** " + itoa(int64(8*a.size))
			case a.size > 4:
				rng = ">= -(2 ** " + itoa(int64(8*a.size-1)) + ") and < 2 ** " + itoa(int64(8*a.size-1))
			default:
				rng = ">= " + numberString(lo) + " and <= " + numberString(hi)
			}
			return undefined, e.outOfRange("value", rng, valV)
		}
	}
	off, err := e.readOffset(offV, a.size, len(b))
	if err != nil {
		return undefined, err
	}
	p := b[off : off+a.size]
	var u uint64
	switch {
	case a.float && a.size == 4:
		u = uint64(math.Float32bits(float32(v)))
	case a.float:
		u = math.Float64bits(v)
	default:
		if v == v && !math.IsInf(v, 0) {
			u = uint64(int64(math.Trunc(v)))
		}
	}
	switch a.size {
	case 1:
		p[0] = byte(u)
	case 2:
		if a.le {
			binary.LittleEndian.PutUint16(p, uint16(u))
		} else {
			binary.BigEndian.PutUint16(p, uint16(u))
		}
	case 4:
		if a.le {
			binary.LittleEndian.PutUint32(p, uint32(u))
		} else {
			binary.BigEndian.PutUint32(p, uint32(u))
		}
	case 8:
		if a.le {
			binary.LittleEndian.PutUint64(p, u)
		} else {
			binary.BigEndian.PutUint64(p, u)
		}
	}
	return engine.IntValue(off + a.size), nil
}

// byteLengthArg validates the byteLength of readUIntLE and friends.
func (e *env) byteLengthArg(v engine.Value) (int, error) {
	if !v.IsNumber() {
		return 0, e.argTypeError("byteLength", "of type number", v)
	}
	f := v.AsNumber()
	if f != math.Trunc(f) {
		return 0, e.outOfRange("byteLength", "an integer", v)
	}
	if f < 1 || f > 6 {
		return 0, e.outOfRange("byteLength", ">= 1 and <= 6", v)
	}
	return int(f), nil
}

func (e *env) bufferReadVar(this, offV, lenV engine.Value, signed, le bool) (engine.Value, error) {
	b, err := e.thisBytes(this)
	if err != nil {
		return undefined, err
	}
	if offV.IsUndefined() {
		return undefined, e.argTypeError("offset", "of type number", offV)
	}
	n, err := e.byteLengthArg(lenV)
	if err != nil {
		return undefined, err
	}
	off, err := e.readOffset(offV, n, len(b))
	if err != nil {
		return undefined, err
	}
	var u uint64
	for i := 0; i < n; i++ {
		c := b[off+i]
		if le {
			u |= uint64(c) << (8 * i)
		} else {
			u = u<<8 | uint64(c)
		}
	}
	if signed {
		shift := 64 - 8*n
		return engine.Int64Value(int64(u<<shift) >> shift), nil
	}
	return engine.Int64Value(int64(u)), nil
}

func (e *env) bufferWriteVar(this engine.Value, args []engine.Value, signed, le bool) (engine.Value, error) {
	r := e.r
	if _, err := e.thisBytes(this); err != nil {
		return undefined, err
	}
	valV, offV, lenV := engine.Arg(args, 0), engine.Arg(args, 1), engine.Arg(args, 2)
	n, err := e.byteLengthArg(lenV)
	if err != nil {
		return undefined, err
	}
	v, err := r.ToNumber(valV)
	if err != nil {
		return undefined, err
	}
	lo, hi := 0.0, math.Ldexp(1, 8*n)-1
	if signed {
		lo, hi = -math.Ldexp(1, 8*n-1), math.Ldexp(1, 8*n-1)-1
	}
	if v > hi || v < lo {
		var rng string
		switch {
		case n > 4 && !signed:
			rng = ">= 0 and < 2 ** " + itoa(int64(8*n))
		case n > 4:
			rng = ">= -(2 ** " + itoa(int64(8*n-1)) + ") and < 2 ** " + itoa(int64(8*n-1))
		default:
			rng = ">= " + numberString(lo) + " and <= " + numberString(hi)
		}
		return undefined, e.outOfRange("value", rng, valV)
	}
	b, _ := this.AsObject().BufferData()
	if offV.IsUndefined() {
		return undefined, e.argTypeError("offset", "of type number", offV)
	}
	off, err := e.readOffset(offV, n, len(b))
	if err != nil {
		return undefined, err
	}
	var u uint64
	if v == v {
		u = uint64(int64(math.Trunc(v)))
	}
	for i := 0; i < n; i++ {
		if le {
			b[off+i] = byte(u >> (8 * i))
		} else {
			b[off+n-1-i] = byte(u >> (8 * i))
		}
	}
	return engine.IntValue(off + n), nil
}
