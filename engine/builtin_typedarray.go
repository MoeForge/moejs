package engine

import (
	"encoding/binary"
	"math"
	"reflect"
	"unsafe"
)

// Typed arrays (ECMA-262 §23.2): %TypedArray%, its prototype and the twelve
// concrete constructors, one per element type but in elemType order. A typed
// array is an integer-indexed exotic object: every canonical numeric string
// key (typedArrayKey) is answered by its buffer in all the internal methods
// (the class-routing arms in object.go and proxy.go call the hooks below)
// and never reaches its shape, so it shares the root shapes of ordinary
// objects and no property inline cache ever holds an indexed key of one
// (fillGetIC). The elements are stored little-endian, the byte order the
// spec lets an implementation choose.
//
// The typed arrays belong to the binary group (installBinary): the same
// late installer defines the buffers, DataView, the typed arrays and
// Atomics on the first use of any of their names.

// typedArray is the payload of a typed array: its view of the buffer, whose
// length is [[ByteLength]] (-1 for a length-tracking array: [[ArrayLength]]
// auto), and its element type.
type typedArray struct {
	view dataView
	kind elemType
}

// typedArrayObject co-allocates a typed array object with its payload.
type typedArrayObject struct {
	obj Object
	ta  typedArray
}

// elemShift is log2 of each elemType's size.
var elemShift = [...]uint8{0, 0, 0, 1, 1, 2, 2, 1, 2, 3, 3, 3}

// length implements IsTypedArrayOutOfBounds and TypedArrayLength: the
// element count, or -1 when the array is out of bounds (its buffer
// detached, or resized to end before the array does).
func (ta *typedArray) length() int {
	n := ta.view.byteLength()
	if n < 0 {
		return -1
	}
	return n >> elemShift[ta.kind]
}

// fixedLength implements IsTypedArrayFixedLength.
func (ta *typedArray) fixedLength() bool {
	return ta.view.length >= 0 && (!ta.view.data.resizable() || ta.view.buf.class == ClassSharedArrayBuffer)
}

// index implements CanonicalNumericIndexString and IsValidIntegerIndex for a
// typedArrayKey key: the element index and whether it is valid. Every
// valid index is an array index: an array holds fewer than 2^31 elements.
func (ta *typedArray) index(key PropertyKey) (int, bool) {
	if !key.IsIndex() {
		return 0, false
	}
	i := int(key.Index())
	return i, i < ta.length()
}

// bytes returns the bytes of the n elements from i on; they must be in
// bounds.
func (ta *typedArray) bytes(i, n int) []byte {
	s := elemShift[ta.kind]
	off := ta.view.offset + i<<s
	return ta.view.data.data[off : off+n<<s]
}

// viewed returns the bytes of all the elements, nil when the array is out
// of bounds.
func (ta *typedArray) viewed() []byte {
	n := ta.length()
	if n < 0 {
		return nil
	}
	b := ta.bytes(0, n)
	return b[:len(b):len(b)]
}

// raw returns the bits of element i, which must be in bounds.
func (ta *typedArray) raw(i int) uint64 {
	s := elemShift[ta.kind]
	b := ta.view.data.data[ta.view.offset+i<<s:]
	switch s {
	case 0:
		return uint64(b[0])
	case 1:
		return uint64(binary.LittleEndian.Uint16(b))
	case 2:
		return uint64(binary.LittleEndian.Uint32(b))
	}
	return binary.LittleEndian.Uint64(b)
}

// setRaw stores the bits u in element i, which must be in bounds.
func (ta *typedArray) setRaw(i int, u uint64) {
	s := elemShift[ta.kind]
	b := ta.view.data.data[ta.view.offset+i<<s:]
	switch s {
	case 0:
		b[0] = byte(u)
	case 1:
		binary.LittleEndian.PutUint16(b, uint16(u))
	case 2:
		binary.LittleEndian.PutUint32(b, uint32(u))
	default:
		binary.LittleEndian.PutUint64(b, u)
	}
}

// at returns element i, which must be in bounds.
func (ta *typedArray) at(i int) Value { return rawToValue(ta.kind, ta.raw(i)) }

// rawToFloat is rawToValue for a type t that does not hold BigInts, as a
// float64.
func rawToFloat(t elemType, u uint64) float64 {
	switch t {
	case elemInt8:
		return float64(int8(u))
	case elemUint8, elemUint8Clamped:
		return float64(uint8(u))
	case elemInt16:
		return float64(int16(u))
	case elemUint16:
		return float64(uint16(u))
	case elemInt32:
		return float64(int32(u))
	case elemUint32:
		return float64(uint32(u))
	case elemFloat16:
		return float16ToFloat64(uint16(u))
	case elemFloat32:
		return float64(math.Float32frombits(uint32(u)))
	}
	return math.Float64frombits(u)
}

// typedArrayKey reports whether a typed array answers key itself: key is a
// canonical numeric string (CanonicalNumericIndexString is not undefined).
// Array indices always are; a string key is an atom that knows
// (String.numeric).
func typedArrayKey(key PropertyKey) bool {
	return key.IsIndex() || key.IsString() && key.String().numeric
}

// typedArrayOwnCell implements [[GetOwnProperty]] of the typed array o for
// an array index key: a valid index is a writable, enumerable,
// configurable data property. A numeric string key is never a valid index,
// and getOwnCell finds none in the shape: DefineOwnProperty and Set send
// every typedArrayKey key to typedArrayDefine and typedArraySet.
func typedArrayOwnCell(o *Object, key PropertyKey) (propCell, bool) {
	ta := o.internal.(*typedArray)
	if i, ok := ta.index(key); ok {
		return propCell{value: ta.at(i), attrs: attrDefault}, true
	}
	return propCell{}, false
}

// typedArrayGetNumber implements [[Get]] of the typed array o for the
// Number key f (TypedArrayGetElement): every Number is a canonical numeric
// string. An index inFixed of a Number type is read in place.
func typedArrayGetNumber(o *Object, f float64) Value {
	ta := o.internal.(*typedArray)
	i := int(f)
	if float64(i) != f || i < 0 {
		return Undefined()
	}
	if ta.inFixed(i) {
		b := ta.view.data.data[ta.view.offset+i<<elemShift[ta.kind]:]
		var x float64
		switch ta.kind {
		case elemFloat64:
			x = math.Float64frombits(binary.LittleEndian.Uint64(b))
		case elemFloat32:
			x = float64(math.Float32frombits(binary.LittleEndian.Uint32(b)))
		case elemInt32:
			return IntValue(int(int32(binary.LittleEndian.Uint32(b))))
		case elemUint32:
			return IntValue(int(binary.LittleEndian.Uint32(b)))
		case elemInt16:
			return IntValue(int(int16(binary.LittleEndian.Uint16(b))))
		case elemUint16:
			return IntValue(int(binary.LittleEndian.Uint16(b)))
		case elemInt8:
			return IntValue(int(int8(b[0])))
		case elemUint8, elemUint8Clamped:
			return IntValue(int(b[0]))
		default:
			return ta.at(i) // Float16 and the BigInt types
		}
		return NumberValue(x)
	}
	if i < ta.length() {
		return ta.at(i)
	}
	return Undefined()
}

// inFixed reports whether i, i >= 0, indexes an element of a fixed-length
// array whose buffer is not detached and still holds the whole array: then
// the array's length is its own, with no TypedArrayLength to compute, and
// the element types read and write their bits in place.
func (ta *typedArray) inFixed(i int) bool {
	v := &ta.view
	return v.length >= 0 && i < v.length>>elemShift[ta.kind] && !v.data.detached && v.offset+v.length <= len(v.data.data)
}

// float64At reads the element at the Number key f of a Float64Array when f
// is an index that inFixed accepts: the in-place read of typedArrayGetNumber,
// which the interpreter loop makes without a call.
func (ta *typedArray) float64At(f float64) (float64, bool) {
	v, i := &ta.view, int(f)
	if ta.kind != elemFloat64 || float64(i) != f || v.length < 0 || uint(i) >= uint(v.length>>3) || v.data.detached || v.offset+v.length > len(v.data.data) {
		return 0, false
	}
	return math.Float64frombits(binary.LittleEndian.Uint64(v.data.data[v.offset+i<<3:])), true
}

// setFloat64 stores the Number x at the Number key f of a Float64Array when
// f is an index that inFixed accepts: the in-place write of
// typedArraySetNumber.
func (ta *typedArray) setFloat64(f, x float64) bool {
	v, i := &ta.view, int(f)
	if ta.kind != elemFloat64 || float64(i) != f || v.length < 0 || uint(i) >= uint(v.length>>3) || v.data.detached || v.offset+v.length > len(v.data.data) {
		return false
	}
	binary.LittleEndian.PutUint64(v.data.data[v.offset+i<<3:], math.Float64bits(x))
	return true
}

// typedArraySetNumber implements [[Set]] of the typed array o for the Number
// key f with o as the receiver (TypedArraySetElement): the value is
// converted even when f is not a valid index. A Number written to an index
// inFixed of a Number type is stored in place.
func (r *Realm) typedArraySetNumber(o *Object, f float64, v Value) error {
	ta := o.internal.(*typedArray)
	i := int(f)
	index := float64(i) == f && i >= 0
	var u uint64
	if index && v.IsNumber() && ta.inFixed(i) && !ta.kind.isBigInt() {
		// A Number converts without running code, so the index stays valid.
		if x := v.AsNumber(); ta.kind == elemFloat64 {
			u = math.Float64bits(x)
		} else {
			u = numberToRaw(ta.kind, x)
		}
	} else {
		var err error
		if u, err = r.toRaw(ta.kind, v); err != nil {
			return err
		}
		if !index || i >= ta.length() {
			return nil
		}
	}
	ta.setRaw(i, u)
	return nil
}

// typedArraySetElement implements TypedArraySetElement(ta, i, v): convert,
// then store if i is still a valid index.
func (r *Realm) typedArraySetElement(ta *typedArray, i int, v Value) error {
	u, err := r.toRaw(ta.kind, v)
	if err != nil {
		return err
	}
	if i < ta.length() {
		ta.setRaw(i, u)
	}
	return nil
}

// typedArraySet implements [[Set]] of the typed array obj for a
// typedArrayKey key.
func (r *Realm) typedArraySet(obj *Object, key PropertyKey, v Value, receiver Value) (bool, error) {
	ta := obj.internal.(*typedArray)
	if receiver.IsObject() && receiver.AsObject() == obj {
		u, err := r.toRaw(ta.kind, v)
		if err != nil {
			return false, err
		}
		if i, ok := ta.index(key); ok {
			ta.setRaw(i, u)
		}
		return true, nil
	}
	if _, ok := ta.index(key); !ok {
		return true, nil
	}
	// OrdinarySetWithOwnDescriptor with the element's writable data
	// descriptor.
	if !receiver.IsObject() {
		return false, nil
	}
	return r.receiverSet(receiver.AsObject(), key, v)
}

// typedArrayDefine implements [[DefineOwnProperty]] of the typed array o for
// a typedArrayKey key.
func (r *Realm) typedArrayDefine(o *Object, key PropertyKey, desc PropertyDescriptor) (bool, error) {
	ta := o.internal.(*typedArray)
	if _, ok := ta.index(key); !ok {
		return false, nil
	}
	if desc.HasConfigurable() && !desc.Configurable() || desc.HasEnumerable() && !desc.Enumerable() ||
		desc.IsAccessorDescriptor() || desc.HasWritable() && !desc.Writable() {
		return false, nil
	}
	if desc.HasValue() {
		u, err := r.toRaw(ta.kind, desc.Value)
		if err != nil {
			return false, err
		}
		if i, ok := ta.index(key); ok {
			ta.setRaw(i, u)
		}
	}
	return true, nil
}

// maxTypedArrayKeys bounds the key lists of a typed array: past it
// [[OwnPropertyKeys]] throws a RangeError rather than build a list of up to
// 2^31 keys.
const maxTypedArrayKeys = 1 << 24

// typedArrayKeysLimit checks the typed array o against maxTypedArrayKeys.
func (r *Realm) typedArrayKeysLimit(o *Object) error {
	if o.internal.(*typedArray).length() > maxTypedArrayKeys {
		return r.RangeError("Too many properties to enumerate")
	}
	return nil
}

// typedArraySetIntegrity implements SetIntegrityLevel(o, sealed or frozen)
// for Object.seal and Object.freeze of the typed array o, which throw
// unless o has no elements: an element cannot become non-configurable.
func (r *Realm) typedArraySetIntegrity(o *Object, frozen bool) error {
	ok, err := r.preventExtensions(o)
	if err != nil {
		return err
	}
	if !ok {
		return r.TypeError("Cannot prevent extensions on a length-tracking typed array or one over a resizable buffer")
	}
	if o.internal.(*typedArray).length() > 0 {
		if frozen {
			return r.TypeError("Cannot freeze array buffer views with elements")
		}
		return r.TypeError("Cannot seal array buffer views with elements")
	}
	_, err = r.setIntegrityLevel(o, frozen)
	return err
}

// --- install ----------------------------------------------------------------------

// installTypedArrays builds %TypedArray%, the concrete constructors, their
// prototypes and Atomics (installBinary).
func installTypedArrays(r *Realm, b *binaryIntrinsics, d *binaryDefs) {
	a := binaryNames()
	// The getters, constructor, the methods, toString, @@iterator and
	// @@toStringTag.
	props := len(d.typedArrayGetters) + 1 + len(d.typedArrayMethods) + 3
	p := r.newIntrinsic(ClassObject, r.ObjectPrototype, props)
	c := r.newConstructor(a.typedArray, 0, typedArrayAbstract, typedArrayAbstractConstruct, p)
	b.TypedArrayPrototype, b.TypedArrayCtor = p, c
	c.ReserveSlots(r, len(d.typedArrayStatics)+1)
	r.installBuiltins(c, d.typedArrayStatics)
	r.installSpeciesGetter(c)
	r.installGetters(p, d.typedArrayGetters)
	r.installBuiltins(p, d.typedArrayMethods)
	r.installOrReplace(p, StringKey(AtomToString), propCell{value: r.arrayToStringFn(), attrs: attrHidden})
	r.installOrReplace(p, iteratorKey, propCell{value: bootstrapOwnValue(p, StringKey(AtomValues)), attrs: attrHidden})
	tag := r.NewNativeFunction(a.getToStringTag, 0, typedArrayToStringTag)
	r.installOrReplace(p, SymbolKey(SymToStringTag), propCell{
		value: accessorValue(&Accessor{Get: tag}),
		attrs: attrConfigurable | attrAccessor,
	})

	// Each constructor inherits from %TypedArray%, each prototype from
	// %TypedArray.prototype%.
	r.markPrototype(c)
	root := r.rootShapeFor(c)
	bpe := StringKey(a.bytesPerElement)
	for k := range elemType(len(typedArrayCtorNames)) {
		// Uint8Array has the base64 and hex members too.
		var statics, methods []builtinDef
		if k == elemUint8 {
			statics, methods = d.uint8ArrayStatics, d.uint8ArrayMethods
		}
		size := propCell{value: IntValue(elemSize[k]), attrs: 0}
		kp := r.newIntrinsic(ClassObject, p, 2+len(methods))
		kc := r.newConstructor(typedArrayCtorNames[k], 3, d.typedArrayCalls[k], d.typedArrayConstructs[k], kp)
		kc.proto = c
		kc.shape = kc.shape.rebase(r, root)
		kc.ReserveSlots(r, 1+len(statics))
		r.installOrReplace(kc, bpe, size)
		r.installOrReplace(kp, bpe, size)
		r.installBuiltins(kc, statics)
		r.installBuiltins(kp, methods)
		b.typedArrayPrototypes[k], b.typedArrayCtors[k] = kp, kc
	}

	b.Atomics = r.newIntrinsic(ClassObject, r.ObjectPrototype, 0)
	b.Atomics.flags &^= flagIsPrototype
	r.installKeys(b.Atomics, d.atomics)
}

// arrayToStringFn returns %Array.prototype.toString%, the initial value of
// %TypedArray%.prototype.toString too. A mutable realm installs the typed
// arrays late, possibly after the program replaced Array.prototype.toString:
// then the typed arrays get a function of their own.
func (r *Realm) arrayToStringFn() Value {
	v := bootstrapOwnValue(r.ArrayPrototype, StringKey(AtomToString))
	if v.IsObject() {
		if fd, ok := v.AsObject().internal.(*FunctionData); ok && fd.kind == FuncNative &&
			reflect.ValueOf(fd.native).Pointer() == reflect.ValueOf(arrayProtoToString).Pointer() {
			return v
		}
	}
	return ObjectValue(r.NewNativeFunction(AtomToString, 0, arrayProtoToString))
}

// typedArrayKind returns the element type of c if it is one of the realm's
// typed array constructors.
func (b *binaryIntrinsics) typedArrayKind(c *Object) (elemType, bool) {
	for k, kc := range b.typedArrayCtors {
		if kc == c {
			return elemType(k), true
		}
	}
	return 0, false
}

// typedArrayAbstract and typedArrayAbstractConstruct implement %TypedArray%
// itself, which always throws.
func typedArrayAbstract(r *Realm, this Value, args []Value) (Value, error) {
	return Undefined(), r.TypeError("Abstract class TypedArray not directly constructable")
}

func typedArrayAbstractConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	return typedArrayAbstract(r, Undefined(), args)
}

// typedArrayToStringTag implements get %TypedArray%.prototype[@@toStringTag].
func typedArrayToStringTag(r *Realm, this Value, args []Value) (Value, error) {
	if this.IsObject() && this.AsObject().class == ClassTypedArray {
		return StringValue(typedArrayCtorNames[this.AsObject().internal.(*typedArray).kind]), nil
	}
	return Undefined(), nil
}

// --- creation ---------------------------------------------------------------------

// newTypedArrayObject creates a typed array of type kind over byteLength
// bytes (-1: length-tracking) of buf from offset.
func (r *Realm) newTypedArrayObject(proto *Object, kind elemType, buf *Object, offset, byteLength int) *Object {
	r.markPrototype(proto)
	to := &typedArrayObject{ta: typedArray{
		view: dataView{buf: buf, data: buf.internal.(*arrayBuffer), offset: offset, length: byteLength},
		kind: kind,
	}}
	r.chargeObject(unsafe.Sizeof(typedArrayObject{}))
	o := &to.obj
	o.shape = r.rootShapeFor(proto)
	o.proto = proto
	o.class = ClassTypedArray
	o.flags = flagExtensible
	o.internal = &to.ta
	return o
}

// checkTypedArrayLength implements the RangeError of AllocateTypedArrayBuffer:
// n elements of type kind must fit a buffer.
func (r *Realm) checkTypedArrayLength(kind elemType, n int64) error {
	if n > maxBufferLength>>elemShift[kind] {
		return r.RangeError("Invalid typed array length: %d", n)
	}
	return nil
}

// allocTypedArray implements AllocateTypedArray with a length: a typed array
// of n elements over a new ArrayBuffer.
func (r *Realm) allocTypedArray(proto *Object, kind elemType, n int64) (*Object, error) {
	if err := r.checkTypedArrayLength(kind, n); err != nil {
		return nil, err
	}
	size := int(n) << elemShift[kind]
	if r.overBudget(size) {
		return nil, r.RangeError("Invalid typed array length: %d", n)
	}
	buf := r.newBufferObject(r.binaryIntr().ArrayBufferPrototype, ClassArrayBuffer, r.newBytes(size, size), -1)
	return r.newTypedArrayObject(proto, kind, buf, 0, size), nil
}

// typedArrayConstructor returns the [[Construct]] of the kind constructor.
func typedArrayConstructor(kind elemType) NativeCtor {
	return func(r *Realm, args []Value, newTarget *Object) (Value, error) {
		o, err := r.constructTypedArray(kind, args, newTarget)
		if err != nil {
			return Undefined(), err
		}
		return ObjectValue(o), nil
	}
}

// constructTypedArray implements new TypedArray(...args) for the concrete
// constructor of kind.
func (r *Realm) constructTypedArray(kind elemType, args []Value, newTarget *Object) (*Object, error) {
	b := r.binaryIntr()
	ctor, defaultProto := b.typedArrayCtors[kind], b.typedArrayPrototypes[kind]
	first := Arg(args, 0)
	if !first.IsObject() {
		var n int64
		if len(args) > 0 {
			var err error
			if n, err = r.ToIndex(first); err != nil {
				return nil, err
			}
		}
		proto, err := r.GetPrototypeFromConstructor(newTarget, ctor, defaultProto)
		if err != nil {
			return nil, err
		}
		return r.allocTypedArray(proto, kind, n)
	}
	proto, err := r.GetPrototypeFromConstructor(newTarget, ctor, defaultProto)
	if err != nil {
		return nil, err
	}
	src := first.AsObject()
	switch src.class {
	case ClassTypedArray:
		return r.typedArrayFromTypedArray(proto, kind, src.internal.(*typedArray))
	case ClassArrayBuffer, ClassSharedArrayBuffer:
		return r.typedArrayFromBuffer(proto, kind, src, Arg(args, 1), Arg(args, 2))
	}
	if r.fastIterable(first) {
		if o, ok, err := r.typedArrayFromDense(proto, kind, src); ok || err != nil {
			return o, err
		}
	}
	m, err := r.GetMethod(first, iteratorKey)
	if err != nil {
		return nil, err
	}
	if m.IsUndefined() {
		return r.typedArrayFromArrayLike(proto, kind, src)
	}
	vals, err := r.iterableToList(first, m)
	if err != nil {
		return nil, err
	}
	o, err := r.allocTypedArray(proto, kind, int64(len(vals)))
	if err != nil {
		return nil, err
	}
	ta := o.internal.(*typedArray)
	for i, v := range vals {
		if err := interruptEvery(r, int64(i)); err != nil {
			return nil, err
		}
		u, err := r.toRaw(kind, v)
		if err != nil {
			return nil, err
		}
		ta.setRaw(i, u)
	}
	return o, nil
}

// typedArrayFromTypedArray implements InitializeTypedArrayFromTypedArray. A
// valid length allocates unobservably, so the content type check comes
// first.
func (r *Realm) typedArrayFromTypedArray(proto *Object, kind elemType, src *typedArray) (*Object, error) {
	n := src.length()
	if n < 0 {
		return nil, r.TypeError("Cannot construct a TypedArray from a detached or out-of-bounds TypedArray")
	}
	if err := r.checkTypedArrayLength(kind, int64(n)); err != nil {
		return nil, err
	}
	if src.kind.isBigInt() != kind.isBigInt() {
		return nil, mixedContentError(r)
	}
	o, err := r.allocTypedArray(proto, kind, int64(n))
	if err != nil {
		return nil, err
	}
	ta := o.internal.(*typedArray)
	if src.kind == kind {
		return o, r.copyBytes(ta.bytes(0, n), src.bytes(0, n))
	}
	return o, r.convertElements(ta, 0, src, 0, n)
}

// mixedContentError is the TypeError of copying between a BigInt and a
// Number typed array.
func mixedContentError(r *Realm) error {
	return r.TypeError("Cannot mix BigInt and other types, use explicit conversions")
}

// convertElements copies n elements of src from si to dst from di, of a
// different type with the same content type, converting each.
func (r *Realm) convertElements(dst *typedArray, di int, src *typedArray, si, n int) error {
	big := dst.kind.isBigInt()
	for i := range n {
		if err := interruptEvery(r, int64(i)); err != nil {
			return err
		}
		u := src.raw(si + i)
		if !big {
			u = numberToRaw(dst.kind, rawToFloat(src.kind, u))
		}
		dst.setRaw(di+i, u)
	}
	return nil
}

// typedArrayFromBuffer implements InitializeTypedArrayFromArrayBuffer.
func (r *Realm) typedArrayFromBuffer(proto *Object, kind elemType, bufObj *Object, byteOffset, length Value) (*Object, error) {
	buf := bufObj.internal.(*arrayBuffer)
	size := int64(elemSize[kind])
	off, err := r.ToIndex(byteOffset)
	if err != nil {
		return nil, err
	}
	if off%size != 0 {
		return nil, r.RangeError("Start offset of %s should be a multiple of %d", typedArrayCtorNames[kind].GoString(), size)
	}
	var newLen int64
	if !length.IsUndefined() {
		if newLen, err = r.ToIndex(length); err != nil {
			return nil, err
		}
	}
	if buf.detached {
		return nil, r.TypeError("Cannot construct a TypedArray on a detached ArrayBuffer")
	}
	n := int64(len(buf.data))
	byteLen := -1
	switch {
	case length.IsUndefined() && buf.resizable():
		if off > n {
			return nil, r.RangeError("Start offset %d is outside the bounds of the buffer", off)
		}
	case length.IsUndefined():
		if n%size != 0 {
			return nil, r.RangeError("Byte length of %s should be a multiple of %d", typedArrayCtorNames[kind].GoString(), size)
		}
		if off > n {
			return nil, r.RangeError("Start offset %d is outside the bounds of the buffer", off)
		}
		byteLen = int(n - off)
	default:
		if off+newLen*size > n {
			return nil, r.RangeError("Invalid typed array length: %d", newLen)
		}
		byteLen = int(newLen * size)
	}
	return r.newTypedArrayObject(proto, kind, bufObj, int(off), byteLen), nil
}

// typedArrayFromDense is the constructor's path for an array fastIterable
// reports: when every element is present and a Number (a BigInt for a
// BigInt type), converting them as the iterator yields them is
// unobservable. ok is false when the path does not apply.
func (r *Realm) typedArrayFromDense(proto *Object, kind elemType, arr *Object) (o *Object, ok bool, err error) {
	n := int(arr.internal.(*ArrayData).length)
	if n > len(arr.elements) {
		return nil, false, nil
	}
	elems := arr.elements[:n]
	big := kind.isBigInt()
	for i, v := range elems {
		if err := interruptEvery(r, int64(i)); err != nil {
			return nil, true, err
		}
		if big && !v.IsBigInt() || !big && !v.IsNumber() {
			return nil, false, nil
		}
	}
	if o, err = r.allocTypedArray(proto, kind, int64(n)); err != nil {
		return nil, true, err
	}
	ta := o.internal.(*typedArray)
	for i, v := range elems {
		if err := interruptEvery(r, int64(i)); err != nil {
			return nil, true, err
		}
		if big {
			ta.setRaw(i, v.AsBigInt().Uint64())
		} else {
			ta.setRaw(i, numberToRaw(kind, v.AsNumber()))
		}
	}
	return o, true, nil
}

// iterableToList implements IteratorToList(GetIteratorFromMethod(v, m)). A
// list longer than any typed array is a RangeError without waiting for the
// iterator to finish.
func (r *Realm) iterableToList(v, m Value) ([]Value, error) {
	it, pos, err := r.iterFromMethod(v, m)
	if err != nil {
		return nil, err
	}
	var vals []Value
	for k := int64(0); ; k++ {
		if err := interruptEvery(r, k); err != nil {
			return nil, err
		}
		e, done, err := r.iterStep(&it, &pos)
		if err != nil {
			return nil, err
		}
		if done {
			return vals, nil
		}
		if len(vals) == maxBufferLength {
			return nil, r.RangeError("Invalid typed array length: %d", len(vals)+1)
		}
		vals = append(vals, e)
	}
}

// typedArrayFromArrayLike implements InitializeTypedArrayFromArrayLike.
func (r *Realm) typedArrayFromArrayLike(proto *Object, kind elemType, src *Object) (*Object, error) {
	n, err := r.LengthOfArrayLike(src)
	if err != nil {
		return nil, err
	}
	o, err := r.allocTypedArray(proto, kind, n)
	if err != nil {
		return nil, err
	}
	ta := o.internal.(*typedArray)
	for i := int64(0); i < n; i++ {
		if err := interruptEvery(r, i); err != nil {
			return nil, err
		}
		v, err := getIndex(r, src, i)
		if err != nil {
			return nil, err
		}
		u, err := r.toRaw(kind, v)
		if err != nil {
			return nil, err
		}
		ta.setRaw(int(i), u)
	}
	return o, nil
}

// validateTypedArray implements ValidateTypedArray(v): v must be a typed
// array that is not out of bounds. It returns the payload and the length.
func (r *Realm) validateTypedArray(v Value, method string) (*typedArray, int, error) {
	if v.IsObject() {
		if o := v.AsObject(); o.class == ClassTypedArray {
			ta := o.internal.(*typedArray)
			if n := ta.length(); n >= 0 {
				return ta, n, nil
			}
			return nil, 0, outOfBoundsTypedArray(r, method)
		}
	}
	return nil, 0, r.TypeError("Method %s called on incompatible receiver %s", method, r.DisplayString(v))
}

func outOfBoundsTypedArray(r *Realm, method string) error {
	return r.TypeError("Cannot perform %s on a detached or out-of-bounds TypedArray", method)
}

// typedArrayCreate implements TypedArrayCreateFromConstructor(c, args). The
// realm's own constructors are called directly.
func (r *Realm) typedArrayCreate(c *Object, args []Value, method string) (*Object, *typedArray, error) {
	var v Value
	var err error
	if k, ok := r.binaryIntr().typedArrayKind(c); ok {
		var o *Object
		if o, err = r.constructTypedArray(k, args, c); err == nil {
			v = ObjectValue(o)
		}
	} else {
		v, err = r.Construct(ObjectValue(c), args, nil)
	}
	if err != nil {
		return nil, nil, err
	}
	ta, n, err := r.validateTypedArray(v, method)
	if err != nil {
		return nil, nil, err
	}
	if len(args) == 1 && args[0].IsNumber() && float64(n) < args[0].AsNumber() {
		return nil, nil, r.TypeError("%s: the constructor returned a TypedArray of length %d, shorter than %s",
			method, n, NumberToGoString(args[0].AsNumber()))
	}
	return v.AsObject(), ta, nil
}

// typedArrayCreateLength is typedArrayCreate(c, «n»).
func (r *Realm) typedArrayCreateLength(c *Object, n int64, method string) (*Object, *typedArray, error) {
	argv := r.pushArgs(1)
	defer r.popArgs(1)
	argv[0] = Int64Value(n)
	return r.typedArrayCreate(c, argv, method)
}

// typedArraySpeciesCreate implements TypedArraySpeciesCreate(o, args): the
// result must have o's content type.
func (r *Realm) typedArraySpeciesCreate(o *Object, args []Value, method string) (*Object, *typedArray, error) {
	kind := o.internal.(*typedArray).kind
	c, err := r.speciesConstructor(o, r.binaryIntr().typedArrayCtors[kind])
	if err != nil {
		return nil, nil, err
	}
	no, nta, err := r.typedArrayCreate(c, args, method)
	if err != nil {
		return nil, nil, err
	}
	if nta.kind.isBigInt() != kind.isBigInt() {
		return nil, nil, r.TypeError("%s: the species constructor returned a TypedArray of another content type", method)
	}
	return no, nta, nil
}

// typedArraySpeciesCreateLength is typedArraySpeciesCreate(o, «n»).
func (r *Realm) typedArraySpeciesCreateLength(o *Object, n int, method string) (*Object, *typedArray, error) {
	argv := r.pushArgs(1)
	defer r.popArgs(1)
	argv[0] = IntValue(n)
	return r.typedArraySpeciesCreate(o, argv, method)
}

// typedArrayCreateSameType implements TypedArrayCreateSameType(ta, n): the
// intrinsic constructor of ta's type reads nothing observable.
func (r *Realm) typedArrayCreateSameType(kind elemType, n int) (*Object, *typedArray, error) {
	o, err := r.allocTypedArray(r.binaryIntr().typedArrayPrototypes[kind], kind, int64(n))
	if err != nil {
		return nil, nil, err
	}
	return o, o.internal.(*typedArray), nil
}

// --- %TypedArray%.from and of ------------------------------------------------------

// typedArrayFrom implements %TypedArray%.from(source, mapfn, thisArg).
func typedArrayFrom(r *Realm, this Value, args []Value) (Value, error) {
	const method = "%TypedArray%.from"
	if !IsConstructor(this) {
		return Undefined(), r.TypeError("%s is not a constructor", r.DisplayString(this))
	}
	c := this.AsObject()
	source, mapfn, thisArg := Arg(args, 0), Arg(args, 1), Arg(args, 2)
	mapping := !mapfn.IsUndefined()
	if mapping && !IsCallable(mapfn) {
		return Undefined(), r.TypeError("%s is not a function", r.DisplayString(mapfn))
	}
	if !mapping && source.IsObject() && r.fastIterable(source) {
		b := r.binaryIntr()
		if k, ok := b.typedArrayKind(c); ok {
			if o, ok, err := r.typedArrayFromDense(b.typedArrayPrototypes[k], k, source.AsObject()); err != nil {
				return Undefined(), err
			} else if ok {
				return ObjectValue(o), nil
			}
		}
	}
	m, err := r.GetMethod(source, iteratorKey)
	if err != nil {
		return Undefined(), err
	}
	argv := r.pushArgs(2)
	defer r.popArgs(2)
	if !m.IsUndefined() {
		vals, err := r.iterableToList(source, m)
		if err != nil {
			return Undefined(), err
		}
		o, ta, err := r.typedArrayCreateLength(c, int64(len(vals)), method)
		if err != nil {
			return Undefined(), err
		}
		for k, v := range vals {
			if err := interruptEvery(r, int64(k)); err != nil {
				return Undefined(), err
			}
			if mapping {
				argv[0], argv[1] = v, IntValue(k)
				if v, err = r.Call(mapfn, thisArg, argv); err != nil {
					return Undefined(), err
				}
			}
			if err := r.typedArraySetElement(ta, k, v); err != nil {
				return Undefined(), err
			}
		}
		return ObjectValue(o), nil
	}
	src, err := r.ToObject(source)
	if err != nil {
		return Undefined(), err
	}
	n, err := r.LengthOfArrayLike(src)
	if err != nil {
		return Undefined(), err
	}
	o, ta, err := r.typedArrayCreateLength(c, n, method)
	if err != nil {
		return Undefined(), err
	}
	for k := int64(0); k < n; k++ {
		if err := interruptEvery(r, k); err != nil {
			return Undefined(), err
		}
		v, err := getIndex(r, src, k)
		if err != nil {
			return Undefined(), err
		}
		if mapping {
			argv[0], argv[1] = v, Int64Value(k)
			if v, err = r.Call(mapfn, thisArg, argv); err != nil {
				return Undefined(), err
			}
		}
		if err := r.typedArraySetElement(ta, int(k), v); err != nil {
			return Undefined(), err
		}
	}
	return ObjectValue(o), nil
}

// typedArrayOf implements %TypedArray%.of(...items).
func typedArrayOf(r *Realm, this Value, args []Value) (Value, error) {
	if !IsConstructor(this) {
		return Undefined(), r.TypeError("%s is not a constructor", r.DisplayString(this))
	}
	o, ta, err := r.typedArrayCreateLength(this.AsObject(), int64(len(args)), "%TypedArray%.of")
	if err != nil {
		return Undefined(), err
	}
	for k, v := range args {
		if err := r.typedArraySetElement(ta, k, v); err != nil {
			return Undefined(), err
		}
	}
	return ObjectValue(o), nil
}

// NewUint8Array creates a Uint8Array over a new ArrayBuffer that adopts
// data, as NewArrayBuffer does, without calling the constructor: the
// object new Uint8Array(buffer) gives, whose [[Prototype]] is proto, or
// Uint8Array.prototype when proto is nil (proto is a subclass's
// prototype for an instance of the subclass, as a constructor's
// newTarget would give it). The array views all of data, which it does
// not copy: JavaScript reads and writes data itself, so the host must not
// modify data, or hand it to another buffer, while JavaScript may read it.
// The two objects are charged to the memory limit; the bytes of data,
// which the host allocated, are the host's to charge (ChargeMemory), as
// for NewArrayBuffer. A data longer than 2 GiB - 1 bytes is a RangeError.
func (r *Realm) NewUint8Array(data []byte, proto *Object) (*Object, error) {
	ab, err := r.NewArrayBuffer(data)
	if err != nil {
		return nil, err
	}
	if proto == nil {
		proto = r.binaryIntr().typedArrayPrototypes[elemUint8]
	}
	return r.newTypedArrayObject(proto, elemUint8, ab, 0, len(data)), nil
}
