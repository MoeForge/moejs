package engine

import "unsafe"

// An object literal of data properties with static keys (the compiler's
// plainLiteral: no spread, accessor, method, __proto__, computed or index
// key, and no key twice) is one NewObjectLit, which makes its object once
// the values are evaluated, in registers in a row: no code can tell, as
// none can reach the object before the literal completes. The object is
// the one a NewObject and a DefineField of each property would make: the
// room NewObjectCap gives it, charged as it charges, and the shape those
// adds take from r.plainRoot. An add of a data property to an ordinary
// extensible object depends on its shape alone, so the shape is the same
// at every run of the literal: the site's inline cache entry keeps it, from
// the first run (objectLitSlow), and with it the shapes of its chain,
// whose transitions an object that takes the same adds one at a time
// finds. No other instruction names the entry (bytecode.VerifyRegisters),
// so it holds the shape of the literal's keys or none.

// objectLit is NewObjectLit at pc, the X word of the instruction w, of a
// frame of fd whose registers start at base: R[A] = an object with the
// properties K[X.lo] .. K[X.lo+C-1] of the values R[B] .. R[B+C-1], and
// returns the pc after the X word. It runs from coldOp, which stores the
// frame's pc first, as the object and the shapes are charged to the memory
// account. The object has the room NewObjectCap(C) gives it, charged as it
// charges; one of up to eight properties is filled where it is allocated,
// one value at a time: a copy of the values would cost a call of the
// runtime's typed copy.
func (r *Realm) objectLit(fd *FunctionData, base int, w uint32, pc int) int {
	st := &r.interp
	x := fd.code.Code[pc]
	a, b, n := base+int(uint8(w>>8)), base+int(uint8(w>>16)), int(w>>24)
	vals := st.stack[b : b+n : b+n]
	e := &r.ic[fd.icBase+x>>16] // below ICCount (bytecode.VerifyRegisters)
	s := e.Shape
	if s == nil || s.count != uint32(n) {
		lo := int(uint16(x))
		r.objectLitSlow(e, fd.meta.keys[lo:lo+n:lo+n], vals, a)
		return pc + 1
	}
	var o *Object
	switch n {
	case 2:
		x := &object2{}
		x.buf[0], x.buf[1] = vals[0], vals[1]
		o, x.slots = &x.Object, x.buf[:2]
	case 3:
		x := &object4{}
		x.buf[0], x.buf[1], x.buf[2] = vals[0], vals[1], vals[2]
		o, x.slots = &x.Object, x.buf[:3]
	case 4:
		x := &object4{}
		x.buf[0], x.buf[1], x.buf[2], x.buf[3] = vals[0], vals[1], vals[2], vals[3]
		o, x.slots = &x.Object, x.buf[:4]
	case 5, 6:
		x := &object6{}
		for i, v := range vals {
			x.buf[i] = v
		}
		o, x.slots = &x.Object, x.buf[:n]
	case 7, 8:
		x := &object8{}
		for i, v := range vals {
			x.buf[i] = v
		}
		o, x.slots = &x.Object, x.buf[:n]
	default:
		st.stack[a] = ObjectValue(r.newLiteral(s, vals))
		return pc + 1
	}
	o.shape, o.class, o.flags = s, ClassObject, flagExtensible
	r.chargeObject(unsafe.Sizeof(Object{}) + uintptr(cap(o.slots)*valueSize))
	st.stack[a] = ObjectValue(o)
	return pc + 1
}

// newLiteral makes the object of a literal of shape s with the values
// vals, of more than eight or of one property (objectLit).
//
//go:noinline
func (r *Realm) newLiteral(s *Shape, vals []Value) *Object {
	o := r.NewObjectCap(len(vals))
	o.shape = s
	o.slots = append(o.slots, vals...)
	return o
}

// objectLitSlow is objectLit at a site whose entry has no shape yet: it
// makes the object into the register a of the stack as NewObject and a
// DefineField of each property make it (defineFieldSlow), and keeps its
// shape in e once the object has it. The object is in its register while
// its shapes are charged, as it is in the register of a NewObject, where a
// measurement of the memory limit counts it.
//
//go:noinline
func (r *Realm) objectLitSlow(e *ICEntry, keys []PropertyKey, vals []Value, a int) {
	o := r.NewObjectCap(len(vals))
	r.interp.stack[a] = ObjectValue(o)
	for i, k := range keys {
		if o.flags&flagDict == 0 && !o.shape.has(k) {
			o.addNamed(r, k, propCell{value: vals[i], attrs: attrDefault})
			continue
		}
		// A key twice, which no compiled literal has, or the object past
		// maxShapeProps; an ordinary extensible object takes any data
		// property.
		_, _ = o.CreateDataProperty(r, k, vals[i])
	}
	if o.flags&flagDict == 0 && !o.shape.owned && o.shape.count == uint32(len(vals)) {
		e.Shape = o.shape
	}
}
