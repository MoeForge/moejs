package engine

import "unsafe"

// DataView (ECMA-262 §25.3): a byte range of an ArrayBuffer or
// SharedArrayBuffer read and written through a get and a set method for
// every element type but Uint8Clamped, in either byte order.

// dataView is the payload of a DataView: [[ViewedArrayBuffer]] (buf, and
// data its payload), [[ByteOffset]] and [[ByteLength]], -1 for auto (a view
// that tracks the length of a resizable buffer).
type dataView struct {
	buf    *Object
	data   *arrayBuffer
	offset int
	length int
}

// viewObject co-allocates a DataView object with its payload.
type viewObject struct {
	obj  Object
	view dataView
}

// byteLength implements IsViewOutOfBounds and GetViewByteLength: the view's
// length, or -1 when it is out of bounds (its buffer detached, or resized
// to end before the view does).
func (v *dataView) byteLength() int {
	b := v.data
	if b.detached {
		return -1
	}
	n := len(b.data)
	if v.offset > n {
		return -1
	}
	if v.length < 0 {
		return n - v.offset
	}
	if v.offset+v.length > n {
		return -1
	}
	return v.length
}

// bytes returns the bytes of the buffer the view views, nil when it is
// out of bounds.
func (v *dataView) bytes() []byte {
	n := v.byteLength()
	if n < 0 {
		return nil
	}
	return v.data.data[v.offset : v.offset+n : v.offset+n]
}

// thisDataView implements RequireInternalSlot(this, [[DataView]]).
func thisDataView(r *Realm, this Value, method string) (*dataView, error) {
	if this.IsObject() {
		if o := this.AsObject(); o.class == ClassDataView {
			if v, ok := dataOk(o, kindDataView); ok {
				return v, nil
			}
		}
	}
	return nil, r.TypeError("Method %s called on incompatible receiver %s", method, r.DisplayString(this))
}

// dataViewTypes names the element types of the get and set methods of
// DataView.prototype (binaryAtoms.get and set).
var dataViewTypes = [...]struct {
	name string
	t    elemType
}{
	{"Int8", elemInt8},
	{"Uint8", elemUint8},
	{"Int16", elemInt16},
	{"Uint16", elemUint16},
	{"Int32", elemInt32},
	{"Uint32", elemUint32},
	{"Float16", elemFloat16},
	{"Float32", elemFloat32},
	{"Float64", elemFloat64},
	{"BigInt64", elemBigInt64},
	{"BigUint64", elemBigUint64},
}

// dataViewMethods builds the get and set methods table (binaryTables).
func dataViewMethods(a *binaryAtoms) []builtinDef {
	defs := make([]builtinDef, 0, 2*len(dataViewTypes))
	for i, d := range dataViewTypes {
		defs = append(defs, builtinDef{a.get[i], dataViewGet(d.t, "DataView.prototype.get"+d.name), 1})
	}
	for i, d := range dataViewTypes {
		defs = append(defs, builtinDef{a.set[i], dataViewSet(d.t, "DataView.prototype.set"+d.name), 2})
	}
	return defs
}

// installDataView builds DataView and DataView.prototype (installBinary).
func installDataView(r *Realm, b *binaryIntrinsics, d *binaryDefs) {
	props := 1 + len(d.dataViewGetters) + len(d.dataViewMethods) + 1
	b.DataViewPrototype = r.newIntrinsic(ClassObject, r.ObjectPrototype, props)
	b.DataViewCtor = r.newConstructor(AtomDataView, 1, d.dataViewCall, dataViewConstruct, b.DataViewPrototype)
	r.installGetters(b.DataViewPrototype, d.dataViewGetters)
	r.installBuiltins(b.DataViewPrototype, d.dataViewMethods)
	r.installToStringTag(b.DataViewPrototype, AtomDataView)
}

// dataViewConstruct implements new DataView(buffer, byteOffset, byteLength).
// The view is checked against the buffer length read before byteLength's
// conversion (bufferByteLength), then again against the live length after
// reading newTarget.prototype; either can detach or resize the buffer.
func dataViewConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	bv := Arg(args, 0)
	var buf *arrayBuffer
	if bv.IsObject() {
		if c := bv.AsObject().class; c == ClassArrayBuffer || c == ClassSharedArrayBuffer {
			buf = extDataOf(bv.AsObject(), kindArrayBuffer)
		}
	}
	if buf == nil {
		return Undefined(), r.TypeError("First argument to DataView constructor must be an ArrayBuffer")
	}
	off, err := r.ToIndex(Arg(args, 1))
	if err != nil {
		return Undefined(), err
	}
	if buf.detached {
		return Undefined(), r.TypeError("Cannot construct a DataView on a detached ArrayBuffer")
	}
	n0 := int64(len(buf.data))
	if off > n0 {
		return Undefined(), r.RangeError("Start offset %d is outside the bounds of the buffer", off)
	}
	offset, length := int(off), -1
	lv := Arg(args, 2)
	if !lv.IsUndefined() {
		l, err := r.ToIndex(lv)
		if err != nil {
			return Undefined(), err
		}
		if off+l > n0 {
			return Undefined(), r.RangeError("Invalid DataView length %d", l)
		}
		length = int(l)
	} else if !buf.resizable() {
		length = int(n0) - offset
	}
	b := r.binaryIntr()
	proto, err := r.GetPrototypeFromConstructor(newTarget, b.DataViewCtor, b.DataViewPrototype)
	if err != nil {
		return Undefined(), err
	}
	if buf.detached {
		return Undefined(), r.TypeError("Cannot construct a DataView on a detached ArrayBuffer")
	}
	if n := len(buf.data); offset > n || !lv.IsUndefined() && offset+length > n {
		return Undefined(), r.RangeError("Invalid DataView length %d", length)
	}
	return ObjectValue(r.newDataViewObject(proto, bv.AsObject(), offset, length)), nil
}

// newDataViewObject creates a DataView over length bytes (-1:
// length-tracking) of buf from offset.
func (r *Realm) newDataViewObject(proto, buf *Object, offset, length int) *Object {
	r.markPrototype(proto)
	vo := &viewObject{view: dataView{buf: buf, data: extDataOf(buf, kindArrayBuffer), offset: offset, length: length}}
	r.chargeObject(unsafe.Sizeof(viewObject{}))
	o := &vo.obj
	o.shape = r.rootShapeFor(proto)
	o.class = ClassDataView
	o.flags = flagExtensible
	setData(o, kindDataView, &vo.view)
	return o
}

func dataViewBuffer(r *Realm, this Value, args []Value) (Value, error) {
	v, err := thisDataView(r, this, "DataView.prototype.buffer")
	if err != nil {
		return Undefined(), err
	}
	return ObjectValue(v.buf), nil
}

func dataViewByteLength(r *Realm, this Value, args []Value) (Value, error) {
	const method = "DataView.prototype.byteLength"
	v, err := thisDataView(r, this, method)
	if err != nil {
		return Undefined(), err
	}
	n := v.byteLength()
	if n < 0 {
		return Undefined(), outOfBoundsView(r, method)
	}
	return IntValue(n), nil
}

func dataViewByteOffset(r *Realm, this Value, args []Value) (Value, error) {
	const method = "DataView.prototype.byteOffset"
	v, err := thisDataView(r, this, method)
	if err != nil {
		return Undefined(), err
	}
	if v.byteLength() < 0 {
		return Undefined(), outOfBoundsView(r, method)
	}
	return IntValue(v.offset), nil
}

func outOfBoundsView(r *Realm, method string) error {
	return r.TypeError("Cannot perform %s on a detached or out-of-bounds DataView", method)
}

// viewBytes implements the bounds steps of GetViewValue and SetViewValue:
// the bytes of the element of type t at byte index idx of v.
func (r *Realm) viewBytes(v *dataView, idx int64, t elemType, method string) ([]byte, error) {
	n := v.byteLength()
	if n < 0 {
		return nil, outOfBoundsView(r, method)
	}
	size := elemSize[t]
	if idx+int64(size) > int64(n) {
		return nil, r.RangeError("Offset is outside the bounds of the DataView")
	}
	i := v.offset + int(idx)
	return v.data.data[i : i+size], nil
}

// dataViewGet returns DataView.prototype.get<t>(byteOffset, littleEndian)
// (GetViewValue).
func dataViewGet(t elemType, method string) NativeFunc {
	return func(r *Realm, this Value, args []Value) (Value, error) {
		v, err := thisDataView(r, this, method)
		if err != nil {
			return Undefined(), err
		}
		idx, err := r.ToIndex(Arg(args, 0))
		if err != nil {
			return Undefined(), err
		}
		little := ToBoolean(Arg(args, 1))
		b, err := r.viewBytes(v, idx, t, method)
		if err != nil {
			return Undefined(), err
		}
		return rawToValue(t, loadRaw(b, len(b), little)), nil
	}
}

// dataViewSet returns DataView.prototype.set<t>(byteOffset, value,
// littleEndian) (SetViewValue).
func dataViewSet(t elemType, method string) NativeFunc {
	return func(r *Realm, this Value, args []Value) (Value, error) {
		v, err := thisDataView(r, this, method)
		if err != nil {
			return Undefined(), err
		}
		idx, err := r.ToIndex(Arg(args, 0))
		if err != nil {
			return Undefined(), err
		}
		raw, err := r.toRaw(t, Arg(args, 1))
		if err != nil {
			return Undefined(), err
		}
		little := ToBoolean(Arg(args, 2))
		b, err := r.viewBytes(v, idx, t, method)
		if err != nil {
			return Undefined(), err
		}
		storeRaw(b, len(b), raw, little)
		return Undefined(), nil
	}
}
