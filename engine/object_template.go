package engine

import "unsafe"

// ObjectTemplate is the layout of ordinary objects that a host makes many
// of with the same prototype and the same data properties, such as the
// Stats objects of node:fs: NewObjectFrom creates one in one allocation,
// with the shape the template computed once, rather than adding the
// properties one at a time. A template belongs to the realm that made it.
type ObjectTemplate struct {
	r     *Realm
	proto *Object
	keys  []PropertyKey
	attrs uint8
	// shape is the shape of the objects, nil when the template has more
	// properties than a shape holds (maxShapeProps): NewObjectFrom then
	// adds them one at a time, as the object would get them.
	shape *Shape
}

// NewObjectTemplate returns the template of the ordinary objects whose
// [[Prototype]] is proto (nil for none) and whose own properties are the
// data properties keys, in that order, each with the attributes attrs (of
// Writable, Enumerable and Configurable). An object NewObjectFrom
// creates is the object NewObjectWithProto(proto) is once
// DefineOwnDataFast has defined keys on it in order: the same shape, the
// same order of keys, the same descriptors. keys must be distinct and not
// array indices; NewObjectTemplate panics otherwise.
func (r *Realm) NewObjectTemplate(proto *Object, keys []PropertyKey, attrs uint8) *ObjectTemplate {
	attrs &^= attrAccessor
	seen := make(map[PropertyKey]bool, len(keys))
	for _, k := range keys {
		if k.IsIndex() {
			panic("engine: NewObjectTemplate: an array index as a key")
		}
		if seen[k] {
			panic("engine: NewObjectTemplate: a key twice")
		}
		seen[k] = true
	}
	t := &ObjectTemplate{r: r, proto: proto, keys: append([]PropertyKey(nil), keys...), attrs: attrs}
	if len(keys) <= maxShapeProps {
		if proto != nil {
			r.markPrototype(proto)
		}
		s := r.rootShapeFor(proto)
		for _, k := range keys {
			s = s.addProperty(r, k, attrs)
		}
		t.shape = s
	}
	return t
}

// Len returns the number of properties of the template's objects.
func (t *ObjectTemplate) Len() int { return len(t.keys) }

// NewObjectFrom creates an object of the template t, with vals, one per
// key in the template's order, as the values of its properties.
func (r *Realm) NewObjectFrom(t *ObjectTemplate, vals []Value) *Object {
	if t.r != r {
		panic("engine: NewObjectFrom: a template of another realm")
	}
	if len(vals) != len(t.keys) {
		panic("engine: NewObjectFrom: not one value per key")
	}
	if t.shape == nil {
		o := r.NewObjectWithProto(t.proto)
		for i, k := range t.keys {
			o.DefineOwnDataFast(r, k, vals[i], t.attrs)
		}
		return o
	}
	if t.proto != nil {
		r.markPrototype(t.proto)
	}
	o, slots, size := allocSlotted(len(vals))
	copy(slots, vals)
	o.slots = slots
	r.chargeObject(size)
	return initObject(o, ClassObject, t.shape)
}

// Templates of nine to sixteen properties are one allocation too.
type (
	object12 struct {
		Object
		buf [12]Value
	}
	object16 struct {
		Object
		buf [16]Value
	}
)

// allocSlotted allocates an object with its n slots, in one allocation for
// n up to 16, and returns them and the bytes allocated.
func allocSlotted(n int) (*Object, []Value, uintptr) {
	switch {
	case n == 0:
		return new(Object), nil, unsafe.Sizeof(Object{})
	case n == 1:
		x := &object1{}
		return &x.Object, x.buf[:], unsafe.Sizeof(*x)
	case n == 2:
		x := &object2{}
		return &x.Object, x.buf[:], unsafe.Sizeof(*x)
	case n == 3:
		x := &object3{}
		return &x.Object, x.buf[:], unsafe.Sizeof(*x)
	case n == 4:
		x := &object4{}
		return &x.Object, x.buf[:], unsafe.Sizeof(*x)
	case n == 5:
		x := &object5{}
		return &x.Object, x.buf[:], unsafe.Sizeof(*x)
	case n == 6:
		x := &object6{}
		return &x.Object, x.buf[:], unsafe.Sizeof(*x)
	case n == 7:
		x := &object7{}
		return &x.Object, x.buf[:], unsafe.Sizeof(*x)
	case n == 8:
		x := &object8{}
		return &x.Object, x.buf[:], unsafe.Sizeof(*x)
	case n <= 12:
		x := &object12{}
		return &x.Object, x.buf[:n:n], unsafe.Sizeof(*x)
	case n <= 16:
		x := &object16{}
		return &x.Object, x.buf[:n:n], unsafe.Sizeof(*x)
	}
	return new(Object), make([]Value, n), unsafe.Sizeof(Object{}) + uintptr(n*valueSize)
}
