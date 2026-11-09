package engine

import (
	"math"
	"sync"
	"unsafe"
)

// ArrayBuffer and SharedArrayBuffer (ECMA-262 §25.1, §25.2). Both classes
// hold an *arrayBuffer payload co-allocated with the object; the class alone
// tells them apart (IsSharedArrayBuffer). A single-realm engine shares no
// memory between agents, so a SharedArrayBuffer is an ArrayBuffer that
// cannot be detached, resized down or transferred, with the spec's distinct
// prototype and receiver checks.
//
// The buffers, DataView, the typed arrays, Atomics and their prototypes are
// late globals (lateGlobal) of one group: installBinary builds them once per
// process for the shared realms and on the first use of any of their names
// in a mutable realm.

func init() {
	for _, name := range [...]*String{AtomArrayBuffer, AtomSharedArrayBuffer, AtomDataView} {
		lateGlobal(StringKey(name), installBinary)
	}
	for _, name := range typedArrayCtorNames {
		lateGlobal(StringKey(name), installBinary)
	}
	lateGlobal(StringKey(AtomAtomics), installBinary)
}

// binaryIntrinsics are the intrinsics of the binary data builtins, behind
// the one extIntrinsics pointer.
type binaryIntrinsics struct {
	ArrayBufferPrototype, ArrayBufferCtor             *Object
	SharedArrayBufferPrototype, SharedArrayBufferCtor *Object
	DataViewPrototype, DataViewCtor                   *Object
	TypedArrayPrototype, TypedArrayCtor               *Object
	// The concrete typed array prototypes and constructors, by elemType.
	typedArrayPrototypes, typedArrayCtors [len(typedArrayCtorNames)]*Object
	Atomics                               *Object
	// text holds TextEncoder and TextDecoder, a late group of their own;
	// nil until the realm defines one of them (textIntr).
	text *textIntrinsics
}

// binaryIntr returns the realm's binary intrinsics, defining the ArrayBuffer
// group if the realm has yet to (ensureLate).
func (r *Realm) binaryIntr() *binaryIntrinsics {
	r.lateAt(lateArrayBuffer)
	return r.binary
}

func (b *binaryIntrinsics) visit(visit func(*Object)) {
	for _, o := range [...]*Object{
		b.ArrayBufferPrototype, b.ArrayBufferCtor,
		b.SharedArrayBufferPrototype, b.SharedArrayBufferCtor,
		b.DataViewPrototype, b.DataViewCtor,
		b.TypedArrayPrototype, b.TypedArrayCtor, b.Atomics,
	} {
		visit(o)
	}
	for k := range b.typedArrayCtors {
		visit(b.typedArrayPrototypes[k])
		visit(b.typedArrayCtors[k])
	}
	if b.text != nil {
		b.text.visit(visit)
	}
}

// maxBufferLength is the implementation limit on the byte length and the
// maximum byte length of a buffer (CreateByteDataBlock throws a RangeError
// beyond it): 2 GiB - 1, which keeps every byte offset an int on every
// platform. A resizable buffer allocates as it grows (resizeBytes), never
// beyond its maximum byte length.
const maxBufferLength = 1<<31 - 1

// arrayBuffer is the payload of an ArrayBuffer or SharedArrayBuffer:
// [[ArrayBufferData]] (whose length is [[ArrayBufferByteLength]]) and
// [[ArrayBufferMaxByteLength]], -1 for a fixed-length buffer. A detached
// buffer has no data; every access checks detached.
type arrayBuffer struct {
	data     []byte
	max      int
	detached bool
}

func (b *arrayBuffer) resizable() bool { return b.max >= 0 }

// bufferObject co-allocates a buffer object with its payload.
type bufferObject struct {
	obj Object
	buf arrayBuffer
}

// newBufferObject creates a buffer of class ClassArrayBuffer or
// ClassSharedArrayBuffer with the given prototype and payload.
func (r *Realm) newBufferObject(proto *Object, class Class, data []byte, maxLen int) *Object {
	r.markPrototype(proto)
	bo := &bufferObject{buf: arrayBuffer{data: data, max: maxLen}}
	r.chargeObject(unsafe.Sizeof(bufferObject{}))
	o := &bo.obj
	o.shape = r.rootShapeFor(proto)
	o.class = class
	o.flags = flagExtensible
	setExtData(o, kindArrayBuffer, &bo.buf)
	return o
}

// shareBuffer returns a new SharedArrayBuffer over the data block of the
// SharedArrayBuffer payload b, as a clone into another agent would get: the
// two objects share b, so each sees the other's writes and grows.
func (r *Realm) shareBuffer(b *arrayBuffer) *Object {
	proto := r.binaryIntr().SharedArrayBufferPrototype
	r.markPrototype(proto)
	o := r.newObject(ClassSharedArrayBuffer, r.rootShapeFor(proto))
	setExtData(o, kindArrayBuffer, b)
	return o
}

// allocateBuffer implements AllocateArrayBuffer and AllocateSharedArrayBuffer
// (maxLen -1 when the buffer is fixed-length): the length checks, the
// prototype from newTarget, then the data block.
func (r *Realm) allocateBuffer(newTarget, ctor, defaultProto *Object, class Class, length, maxLen int64) (*Object, error) {
	if maxLen >= 0 && length > maxLen {
		return nil, r.RangeError("Invalid array buffer length: %d exceeds maxByteLength %d", length, maxLen)
	}
	proto, err := r.GetPrototypeFromConstructor(newTarget, ctor, defaultProto)
	if err != nil {
		return nil, err
	}
	if length > maxBufferLength || maxLen > maxBufferLength || r.overBudget(int(length)) {
		return nil, r.RangeError("Array buffer allocation failed")
	}
	return r.newBufferObject(proto, class, r.newBytes(int(length), int(length)), int(maxLen)), nil
}

// newBytes returns a zeroed data block of length n and capacity c, aligned
// for every element type (TypedArrayElements). That alignment is Go runtime
// behaviour, not a language guarantee: a block of 16 bytes or more gets a
// size class, and those are multiples of 8, while the tiny allocator packs
// the noscan blocks under 16 bytes at any offset, except that mallocgcTiny
// 8-aligns a request whose size is a multiple of 8. So a capacity under 16
// is rounded up to a multiple of 8: 8 bytes stay in the tiny allocator,
// 8-aligned, and 9 to 15 become 16 and leave it. TestBufferDataAligned
// checks this.
func newBytes(n, c int) []byte {
	if c < 16 {
		c = (c + 7) &^ 7
	}
	return make([]byte, n, c)
}

// newBytes is newBytes charging the block to r's memory limit.
func (r *Realm) newBytes(n, c int) []byte {
	r.charge(c)
	return newBytes(n, c)
}

// maxByteLengthOption implements GetArrayBufferMaxByteLengthOption: -1 when
// options has no maxByteLength.
func (r *Realm) maxByteLengthOption(options Value) (int64, error) {
	if !options.IsObject() {
		return -1, nil
	}
	v, err := options.AsObject().GetProp(r, StringKey(binaryNames().maxByteLength))
	if err != nil || v.IsUndefined() {
		return -1, err
	}
	return r.ToIndex(v)
}

// thisBuffer implements RequireInternalSlot(this, [[ArrayBufferData]]) with
// the IsSharedArrayBuffer check of class's methods.
func thisBuffer(r *Realm, this Value, class Class, method string) (*arrayBuffer, error) {
	if this.IsObject() {
		if o := this.AsObject(); o.class == class {
			if b, ok := extDataOk(o, kindArrayBuffer); ok {
				return b, nil
			}
		}
	}
	return nil, r.TypeError("Method %s called on incompatible receiver %s", method, r.DisplayString(this))
}

// DetachArrayBuffer implements DetachArrayBuffer(v) for a host: v must be
// an ArrayBuffer (not a SharedArrayBuffer). Its data is released and every
// view over it becomes out of bounds.
func (r *Realm) DetachArrayBuffer(v Value) error {
	if v.IsObject() && v.AsObject().class == ClassArrayBuffer {
		if b, ok := extDataOk(v.AsObject(), kindArrayBuffer); ok {
			b.data, b.detached = nil, true
			return nil
		}
	}
	return r.TypeError("DetachArrayBuffer: %s is not an ArrayBuffer", r.DisplayString(v))
}

// NewArrayBuffer returns a fixed-length ArrayBuffer over data, which it
// does not copy: JavaScript reads and writes data itself, so the host must
// not modify data while JavaScript may read it. Nothing is written past
// len(data): a transfer to a longer buffer moves the bytes. A nil data is
// an empty buffer; one longer than 2 GiB - 1 bytes is a RangeError.
func (r *Realm) NewArrayBuffer(data []byte) (*Object, error) {
	if len(data) > maxBufferLength {
		return nil, r.RangeError("Array buffer allocation failed")
	}
	if data == nil {
		data = []byte{}
	}
	b := r.binaryIntr()
	return r.newBufferObject(b.ArrayBufferPrototype, ClassArrayBuffer, data[:len(data):len(data)], -1), nil
}

// BufferData returns the data of an ArrayBuffer or SharedArrayBuffer, or
// the bytes of its buffer a typed array or DataView views; ok is false for
// any other object. The slice is the buffer's data, not a copy, and is
// empty for a detached buffer and a view out of its buffer's bounds. It is
// valid until JavaScript next runs, which may detach or resize the buffer.
func (o *Object) BufferData() (data []byte, ok bool) {
	switch x := o.payload().(type) {
	case *arrayBuffer:
		return x.data[:len(x.data):len(x.data)], true
	case *typedArray:
		return x.viewed(), true
	case *dataView:
		return x.bytes(), true
	}
	return nil, false
}

// IsDetachedBuffer reports whether o is a detached ArrayBuffer.
func (o *Object) IsDetachedBuffer() bool {
	b, ok := extDataOk(o, kindArrayBuffer)
	return ok && b.detached
}

// resizeBytes returns data resized to n bytes for a buffer whose length
// can reach maxLen (-1 for a fixed-length buffer); the bytes it gains are
// zero. The backing array is kept only while the buffer still uses most of
// it: a resizable buffer down to a quarter of its capacity, which it can
// grow back into, and a fixed-length one within an eighth, since it never
// uses the rest. Otherwise the bytes move to an array of their own, so a
// small buffer never pins a large block. A resizable buffer growing past
// its capacity at least doubles it, up to maxLen, so growing one in small
// steps copies each byte a constant number of times.
func (r *Realm) resizeBytes(data []byte, n, maxLen int) ([]byte, error) {
	c := cap(data)
	if n <= c && (maxLen >= 0 && n >= c/4 || n >= c-c/8) {
		old := len(data)
		data = data[:n]
		if n > old {
			clear(data[old:])
		}
		return data, nil
	}
	size := n
	if maxLen >= 0 && n > c {
		size = min(max(n, 2*c), maxLen)
	}
	moved := r.newBytes(n, size)
	if err := r.copyBytes(moved, data); err != nil {
		return nil, err
	}
	return moved, nil
}

// copyBytesChunk is how many bytes copyBytes moves between interrupt
// checks: 4096 work units of 256 bytes.
const copyBytesChunk = 1 << 20

// copyBytes copies src into dst (CopyDataBlockBytes), honouring the
// interrupt flag on large lengths.
func (r *Realm) copyBytes(dst, src []byte) error {
	for len(src) > copyBytesChunk && len(dst) > copyBytesChunk {
		copy(dst, src[:copyBytesChunk])
		dst, src = dst[copyBytesChunk:], src[copyBytesChunk:]
		if err := r.CheckInterrupt(); err != nil {
			return err
		}
	}
	copy(dst, src)
	return nil
}

// relativeRange implements the start/end clamping of the slice methods
// over a length len.
func (r *Realm) relativeRange(start, end Value, length int) (int, int, error) {
	rs, err := r.ToIntegerOrInfinity(start)
	if err != nil {
		return 0, 0, err
	}
	re := float64(length)
	if !end.IsUndefined() {
		if re, err = r.ToIntegerOrInfinity(end); err != nil {
			return 0, 0, err
		}
	}
	clamp := func(f float64) int {
		if f < 0 {
			return int(math.Max(float64(length)+f, 0))
		}
		return int(math.Min(f, float64(length)))
	}
	return clamp(rs), clamp(re), nil
}

// --- install ----------------------------------------------------------------------

// binaryDefs holds the builtin tables of the binary group.
type binaryDefs struct {
	arrayBufferGetters, sharedArrayBufferGetters, dataViewGetters []getterDef
	arrayBufferStatics, arrayBufferMethods                        []builtinDef
	sharedArrayBufferMethods, dataViewMethods                     []builtinDef
	arrayBufferCall, sharedArrayBufferCall, dataViewCall          NativeFunc
	typedArrayGetters                                             []getterDef
	typedArrayStatics, typedArrayMethods                          []builtinDef
	uint8ArrayStatics, uint8ArrayMethods                          []builtinDef
	typedArrayCalls                                               [len(typedArrayCtorNames)]NativeFunc
	typedArrayConstructs                                          [len(typedArrayCtorNames)]NativeCtor
	atomics                                                       *keyTable
}

// binaryTables builds the tables with their names (binaryNames) on the
// first installBinary of the process. The getters' "get x" names are made
// by installGetters: a static atom may not be registered after init.
var binaryTables = sync.OnceValue(func() *binaryDefs {
	a := binaryNames()
	d := &binaryDefs{
		arrayBufferGetters: []getterDef{
			{name: a.byteLength, get: arrayBufferByteLength},
			{name: a.maxByteLength, get: arrayBufferMaxByteLength},
			{name: a.resizable, get: arrayBufferResizable},
			{name: a.detached, get: arrayBufferDetached},
		},
		sharedArrayBufferGetters: []getterDef{
			{name: a.byteLength, get: sharedArrayBufferByteLength},
			{name: a.growable, get: sharedArrayBufferGrowable},
			{name: a.maxByteLength, get: sharedArrayBufferMaxByteLength},
		},
		dataViewGetters: []getterDef{
			{name: a.buffer, get: dataViewBuffer},
			{name: a.byteLength, get: dataViewByteLength},
			{name: a.byteOffset, get: dataViewByteOffset},
		},
		arrayBufferStatics: []builtinDef{{a.isView, arrayBufferIsView, 1}},
		arrayBufferMethods: []builtinDef{
			{a.resize, arrayBufferResize, 1},
			{AtomSlice, arrayBufferSlice, 2},
			{atomTransfer, arrayBufferTransfer, 0},
			{a.transferToFixedLength, arrayBufferTransferToFixedLength, 0},
		},
		sharedArrayBufferMethods: []builtinDef{
			{a.grow, sharedArrayBufferGrow, 1},
			{AtomSlice, sharedArrayBufferSlice, 2},
		},
		dataViewMethods:       dataViewMethods(a),
		arrayBufferCall:       requireNew("ArrayBuffer"),
		sharedArrayBufferCall: requireNew("SharedArrayBuffer"),
		dataViewCall:          requireNew("DataView"),
		typedArrayGetters: []getterDef{
			{name: a.buffer, get: typedArrayBuffer},
			{name: a.byteLength, get: typedArrayByteLength},
			{name: a.byteOffset, get: typedArrayByteOffset},
			{name: AtomLength, get: typedArrayLength},
		},
		typedArrayStatics: []builtinDef{{AtomFrom, typedArrayFrom, 1}, {AtomOf, typedArrayOf, 0}},
		typedArrayMethods: typedArrayMethods(a),
		uint8ArrayStatics: uint8ArrayStatics(a),
		uint8ArrayMethods: uint8ArrayMethods(a),
		atomics:           atomicsTable(a),
	}
	for k, name := range typedArrayCtorNames {
		d.typedArrayCalls[k] = requireNew(name.GoString())
		d.typedArrayConstructs[k] = typedArrayConstructor(elemType(k))
	}
	return d
})

// installBinary is the late installer of the binary group: ArrayBuffer,
// SharedArrayBuffer, DataView, the typed arrays and Atomics. It builds them
// all and binds them in coldGlobalKeys order.
func installBinary(r *Realm) {
	b, d := &binaryIntrinsics{}, binaryTables()
	r.binary = b

	b.ArrayBufferPrototype = r.newIntrinsic(ClassObject, r.ObjectPrototype, 10)
	b.ArrayBufferCtor = r.newConstructor(AtomArrayBuffer, 1, d.arrayBufferCall, arrayBufferConstruct, b.ArrayBufferPrototype)
	b.ArrayBufferCtor.ReserveSlots(r, 2)
	r.installBuiltins(b.ArrayBufferCtor, d.arrayBufferStatics)
	r.installSpeciesGetter(b.ArrayBufferCtor)
	r.installGetters(b.ArrayBufferPrototype, d.arrayBufferGetters)
	r.installBuiltins(b.ArrayBufferPrototype, d.arrayBufferMethods)
	r.installToStringTag(b.ArrayBufferPrototype, AtomArrayBuffer)

	b.SharedArrayBufferPrototype = r.newIntrinsic(ClassObject, r.ObjectPrototype, 7)
	b.SharedArrayBufferCtor = r.newConstructor(AtomSharedArrayBuffer, 1, d.sharedArrayBufferCall, sharedArrayBufferConstruct, b.SharedArrayBufferPrototype)
	b.SharedArrayBufferCtor.ReserveSlots(r, 1)
	r.installSpeciesGetter(b.SharedArrayBufferCtor)
	r.installGetters(b.SharedArrayBufferPrototype, d.sharedArrayBufferGetters)
	r.installBuiltins(b.SharedArrayBufferPrototype, d.sharedArrayBufferMethods)
	r.installToStringTag(b.SharedArrayBufferPrototype, AtomSharedArrayBuffer)

	installDataView(r, b, d)
	installTypedArrays(r, b, d)

	r.bindGlobal(AtomArrayBuffer, ObjectValue(b.ArrayBufferCtor))
	r.bindGlobal(AtomSharedArrayBuffer, ObjectValue(b.SharedArrayBufferCtor))
	r.bindGlobal(AtomDataView, ObjectValue(b.DataViewCtor))
	for k, name := range typedArrayCtorNames {
		r.bindGlobal(name, ObjectValue(b.typedArrayCtors[k]))
	}
	r.bindGlobal(AtomAtomics, ObjectValue(b.Atomics))
}

// --- ArrayBuffer --------------------------------------------------------------------

// arrayBufferConstruct implements new ArrayBuffer(length, options).
func arrayBufferConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	length, err := r.ToIndex(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	maxLen, err := r.maxByteLengthOption(Arg(args, 1))
	if err != nil {
		return Undefined(), err
	}
	b := r.binaryIntr()
	o, err := r.allocateBuffer(newTarget, b.ArrayBufferCtor, b.ArrayBufferPrototype, ClassArrayBuffer, length, maxLen)
	if err != nil {
		return Undefined(), err
	}
	return ObjectValue(o), nil
}

// arrayBufferIsView implements ArrayBuffer.isView(arg).
func arrayBufferIsView(r *Realm, this Value, args []Value) (Value, error) {
	v := Arg(args, 0)
	return Bool(v.IsObject() && (v.AsObject().class == ClassDataView || v.AsObject().class == ClassTypedArray)), nil
}

func arrayBufferByteLength(r *Realm, this Value, args []Value) (Value, error) {
	b, err := thisBuffer(r, this, ClassArrayBuffer, "ArrayBuffer.prototype.byteLength")
	if err != nil {
		return Undefined(), err
	}
	return IntValue(len(b.data)), nil
}

func arrayBufferMaxByteLength(r *Realm, this Value, args []Value) (Value, error) {
	b, err := thisBuffer(r, this, ClassArrayBuffer, "ArrayBuffer.prototype.maxByteLength")
	if err != nil {
		return Undefined(), err
	}
	if b.resizable() && !b.detached {
		return IntValue(b.max), nil
	}
	return IntValue(len(b.data)), nil
}

func arrayBufferResizable(r *Realm, this Value, args []Value) (Value, error) {
	b, err := thisBuffer(r, this, ClassArrayBuffer, "ArrayBuffer.prototype.resizable")
	if err != nil {
		return Undefined(), err
	}
	return Bool(b.resizable()), nil
}

func arrayBufferDetached(r *Realm, this Value, args []Value) (Value, error) {
	b, err := thisBuffer(r, this, ClassArrayBuffer, "ArrayBuffer.prototype.detached")
	if err != nil {
		return Undefined(), err
	}
	return Bool(b.detached), nil
}

// arrayBufferResize implements ArrayBuffer.prototype.resize(newLength).
func arrayBufferResize(r *Realm, this Value, args []Value) (Value, error) {
	b, err := thisBuffer(r, this, ClassArrayBuffer, "ArrayBuffer.prototype.resize")
	if err == nil && !b.resizable() {
		err = r.TypeError("Method ArrayBuffer.prototype.resize called on a fixed-length ArrayBuffer")
	}
	if err != nil {
		return Undefined(), err
	}
	n, err := r.ToIndex(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	if b.detached {
		return Undefined(), r.TypeError("Cannot resize a detached ArrayBuffer")
	}
	if n > int64(b.max) {
		return Undefined(), r.RangeError("ArrayBuffer.prototype.resize: invalid length %d", n)
	}
	data, err := r.resizeBytes(b.data, int(n), b.max)
	if err != nil {
		return Undefined(), err
	}
	b.data = data
	return Undefined(), nil
}

// arrayBufferSlice implements ArrayBuffer.prototype.slice(start, end).
func arrayBufferSlice(r *Realm, this Value, args []Value) (Value, error) {
	const method = "ArrayBuffer.prototype.slice"
	b, err := thisBuffer(r, this, ClassArrayBuffer, method)
	if err != nil {
		return Undefined(), err
	}
	if b.detached {
		return Undefined(), r.TypeError("Cannot perform %s on a detached ArrayBuffer", method)
	}
	first, final, err := r.relativeRange(Arg(args, 0), Arg(args, 1), len(b.data))
	if err != nil {
		return Undefined(), err
	}
	newLen := max(final-first, 0)
	nb, nbObj, err := r.bufferSpeciesCreate(this.AsObject(), r.binaryIntr().ArrayBufferCtor, ClassArrayBuffer, method, newLen)
	if err != nil {
		return Undefined(), err
	}
	if nb.detached {
		return Undefined(), r.TypeError("%s: the species constructor returned a detached ArrayBuffer", method)
	}
	if nbObj == this.AsObject() {
		return Undefined(), r.TypeError("%s: the species constructor returned the same ArrayBuffer", method)
	}
	if b.detached {
		return Undefined(), r.TypeError("Cannot perform %s on a detached ArrayBuffer", method)
	}
	if cur := len(b.data); first < cur {
		if err := r.copyBytes(nb.data[:min(newLen, cur-first)], b.data[first:]); err != nil {
			return Undefined(), err
		}
	}
	return ObjectValue(nbObj), nil
}

// bufferSpeciesCreate implements the species steps of the slice methods:
// Construct(SpeciesConstructor(o, defaultCtor), «newLen») must return a
// buffer of class with at least newLen bytes.
func (r *Realm) bufferSpeciesCreate(o, defaultCtor *Object, class Class, method string, newLen int) (*arrayBuffer, *Object, error) {
	c, err := r.speciesConstructor(o, defaultCtor)
	if err != nil {
		return nil, nil, err
	}
	argv := r.pushArgs(1)
	argv[0] = IntValue(newLen)
	v, err := r.Construct(ObjectValue(c), argv, nil)
	r.popArgs(1)
	if err != nil {
		return nil, nil, err
	}
	nb, err := thisBuffer(r, v, class, method+" species result")
	if err != nil {
		return nil, nil, err
	}
	if len(nb.data) < newLen {
		return nil, nil, r.TypeError("%s: the species constructor returned a buffer smaller than %d bytes", method, newLen)
	}
	return nb, v.AsObject(), nil
}

func arrayBufferTransfer(r *Realm, this Value, args []Value) (Value, error) {
	return r.arrayBufferCopyAndDetach(this, Arg(args, 0), true, "ArrayBuffer.prototype.transfer")
}

func arrayBufferTransferToFixedLength(r *Realm, this Value, args []Value) (Value, error) {
	return r.arrayBufferCopyAndDetach(this, Arg(args, 0), false, "ArrayBuffer.prototype.transferToFixedLength")
}

// arrayBufferCopyAndDetach implements ArrayBufferCopyAndDetach. The new
// buffer takes over the old one's backing array (the zero-copy move the
// spec allows) when resizeBytes would keep it for the new length; the
// source is detached only once the new buffer exists.
func (r *Realm) arrayBufferCopyAndDetach(this, newLength Value, preserveResizability bool, method string) (Value, error) {
	b, err := thisBuffer(r, this, ClassArrayBuffer, method)
	if err != nil {
		return Undefined(), err
	}
	n := int64(len(b.data))
	if !newLength.IsUndefined() {
		if n, err = r.ToIndex(newLength); err != nil {
			return Undefined(), err
		}
	}
	if b.detached {
		return Undefined(), r.TypeError("Cannot perform %s on a detached ArrayBuffer", method)
	}
	maxLen := int64(-1)
	if preserveResizability && b.resizable() {
		maxLen = int64(b.max)
	}
	if maxLen >= 0 && n > maxLen {
		return Undefined(), r.RangeError("Invalid array buffer length: %d exceeds maxByteLength %d", n, maxLen)
	}
	if n > maxBufferLength {
		return Undefined(), r.RangeError("Array buffer allocation failed")
	}
	data, err := r.resizeBytes(b.data, int(n), int(maxLen))
	if err != nil {
		return Undefined(), err
	}
	b.data, b.detached = nil, true
	return ObjectValue(r.newBufferObject(r.binaryIntr().ArrayBufferPrototype, ClassArrayBuffer, data, int(maxLen))), nil
}

// --- SharedArrayBuffer ----------------------------------------------------------------

// sharedArrayBufferConstruct implements new SharedArrayBuffer(length, options).
func sharedArrayBufferConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	length, err := r.ToIndex(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	maxLen, err := r.maxByteLengthOption(Arg(args, 1))
	if err != nil {
		return Undefined(), err
	}
	b := r.binaryIntr()
	o, err := r.allocateBuffer(newTarget, b.SharedArrayBufferCtor, b.SharedArrayBufferPrototype, ClassSharedArrayBuffer, length, maxLen)
	if err != nil {
		return Undefined(), err
	}
	return ObjectValue(o), nil
}

func sharedArrayBufferByteLength(r *Realm, this Value, args []Value) (Value, error) {
	b, err := thisBuffer(r, this, ClassSharedArrayBuffer, "SharedArrayBuffer.prototype.byteLength")
	if err != nil {
		return Undefined(), err
	}
	return IntValue(len(b.data)), nil
}

func sharedArrayBufferGrowable(r *Realm, this Value, args []Value) (Value, error) {
	b, err := thisBuffer(r, this, ClassSharedArrayBuffer, "SharedArrayBuffer.prototype.growable")
	if err != nil {
		return Undefined(), err
	}
	return Bool(b.resizable()), nil
}

func sharedArrayBufferMaxByteLength(r *Realm, this Value, args []Value) (Value, error) {
	b, err := thisBuffer(r, this, ClassSharedArrayBuffer, "SharedArrayBuffer.prototype.maxByteLength")
	if err != nil {
		return Undefined(), err
	}
	if b.resizable() {
		return IntValue(b.max), nil
	}
	return IntValue(len(b.data)), nil
}

// sharedArrayBufferGrow implements SharedArrayBuffer.prototype.grow(newLength).
func sharedArrayBufferGrow(r *Realm, this Value, args []Value) (Value, error) {
	b, err := thisBuffer(r, this, ClassSharedArrayBuffer, "SharedArrayBuffer.prototype.grow")
	if err == nil && !b.resizable() {
		err = r.TypeError("Method SharedArrayBuffer.prototype.grow called on a fixed-length SharedArrayBuffer")
	}
	if err != nil {
		return Undefined(), err
	}
	n, err := r.ToIndex(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	if n < int64(len(b.data)) || n > int64(b.max) {
		return Undefined(), r.RangeError("SharedArrayBuffer.prototype.grow: invalid length %d", n)
	}
	data, err := r.resizeBytes(b.data, int(n), b.max)
	if err != nil {
		return Undefined(), err
	}
	b.data = data
	return Undefined(), nil
}

// sharedArrayBufferSlice implements SharedArrayBuffer.prototype.slice(start, end).
func sharedArrayBufferSlice(r *Realm, this Value, args []Value) (Value, error) {
	const method = "SharedArrayBuffer.prototype.slice"
	b, err := thisBuffer(r, this, ClassSharedArrayBuffer, method)
	if err != nil {
		return Undefined(), err
	}
	first, final, err := r.relativeRange(Arg(args, 0), Arg(args, 1), len(b.data))
	if err != nil {
		return Undefined(), err
	}
	newLen := max(final-first, 0)
	nb, nbObj, err := r.bufferSpeciesCreate(this.AsObject(), r.binaryIntr().SharedArrayBufferCtor, ClassSharedArrayBuffer, method, newLen)
	if err != nil {
		return Undefined(), err
	}
	if nb == b {
		return Undefined(), r.TypeError("%s: the species constructor returned the same SharedArrayBuffer", method)
	}
	// A SharedArrayBuffer never shrinks, so [first, first+newLen) is still
	// within b.
	if err := r.copyBytes(nb.data[:newLen], b.data[first:]); err != nil {
		return Undefined(), err
	}
	return ObjectValue(nbObj), nil
}
