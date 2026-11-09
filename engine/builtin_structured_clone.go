package engine

// structuredClone(value, {transfer}) (HTML "StructuredSerializeWithTransfer"
// followed by "StructuredDeserializeWithTransfer" into the same realm) for
// the values that exist without host objects: primitives,
// Boolean/Number/BigInt/String wrappers, Date, RegExp, Array, ordinary
// objects, Map, Set, Error objects, ArrayBuffer, SharedArrayBuffer, typed
// arrays and DataView, with cycles and shared references preserved (the
// views of one buffer view one copy of it). Functions, symbols, symbol
// wrappers and objects with other internal slots (WeakMap, iterators, ...)
// throw a TypeError standing in for the DataCloneError DOMException.
//
// An ArrayBuffer's copy has a copy of its bytes; a SharedArrayBuffer's
// shares its data block, as a clone into another agent does. The transfer
// list moves ArrayBuffers instead: each one's copy is created (empty) and
// recorded in the memory map before the value is visited, and takes over
// the bytes, detaching the original, once the whole value has been.
//
// The clone runs in one pass: an object's copy is created (and recorded in
// the memory map) when it is first reached, then its properties are copied
// depth first from an explicit stack, so getters run in the spec's order
// and nesting depth costs no Go stack; nesting more than maxCloneDepth
// objects converted from Go deep is a RangeError. Deserializing only defines data properties on fresh objects,
// so interleaving it with serialization is unobservable.

var (
	AtomStructuredClone = staticAtom("structuredClone")
	atomTransfer        = staticAtom("transfer")
)

func init() { lateGlobal(StringKey(AtomStructuredClone), installStructuredClone) }

func installStructuredClone(r *Realm) {
	r.bindGlobal(AtomStructuredClone, ObjectValue(r.NewNativeFunction(AtomStructuredClone, 1, globalStructuredClone)))
}

func globalStructuredClone(r *Realm, this Value, args []Value) (Value, error) {
	// The value is a required argument in Web IDL; undefined is a value.
	if len(args) == 0 {
		return Undefined(), r.TypeError("structuredClone requires 1 argument")
	}
	transfer, err := structuredCloneTransfer(r, Arg(args, 1))
	if err != nil {
		return Undefined(), err
	}
	c := cloner{r: r}
	if err := c.reserveTransfer(transfer); err != nil {
		return Undefined(), err
	}
	out, err := c.run(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	if err := c.transfer(transfer); err != nil {
		return Undefined(), err
	}
	return out, nil
}

// structuredCloneTransfer converts the options dictionary and returns its
// transfer list, a sequence<object>: each element is checked as the
// iteration reaches it.
func structuredCloneTransfer(r *Realm, opts Value) ([]*Object, error) {
	if opts.IsNullish() {
		return nil, nil
	}
	if !opts.IsObject() {
		return nil, r.TypeError("structuredClone: options must be an object")
	}
	t, err := opts.AsObject().GetProp(r, StringKey(atomTransfer))
	if err != nil || t.IsUndefined() {
		return nil, err
	}
	if !t.IsObject() {
		return nil, r.TypeError("structuredClone: transfer must be a sequence")
	}
	it, err := r.getIterator(t)
	if err != nil {
		return nil, err
	}
	var list []*Object
	for k := int64(0); ; k++ {
		if err := interruptEvery(r, k); err != nil {
			return nil, err
		}
		v, done, err := it.step(r)
		if err != nil {
			return nil, err
		}
		if done {
			return list, nil
		}
		if !v.IsObject() {
			return nil, r.TypeError("structuredClone: transfer list elements must be objects")
		}
		list = append(list, v.AsObject())
	}
}

// reserveTransfer implements the first loop of
// StructuredSerializeWithTransfer: every element of the transfer list must
// be an ArrayBuffer (not a SharedArrayBuffer) listed once. Each one's copy,
// empty until transfer fills it, is recorded as the copy the value's
// references to the buffer clone to.
func (c *cloner) reserveTransfer(list []*Object) error {
	r := c.r
	for _, o := range list {
		if _, ok := extDataOk(o, kindArrayBuffer); !ok || o.class != ClassArrayBuffer {
			return r.TypeError("DataCloneError: %s could not be transferred", r.DisplayString(ObjectValue(o)))
		}
		if _, ok := c.memory[o]; ok {
			return r.TypeError("DataCloneError: an ArrayBuffer is listed twice in the transfer list")
		}
		c.remember(o, r.newBufferObject(r.binaryIntr().ArrayBufferPrototype, ClassArrayBuffer, nil, -1))
	}
	return nil
}

// transfer implements the second loop of StructuredSerializeWithTransfer
// and the first of StructuredDeserializeWithTransfer: each ArrayBuffer of
// the transfer list, which cloning the value may have detached (a
// DataCloneError) or resized, moves its data block to its copy and is
// detached, in list order.
func (c *cloner) transfer(list []*Object) error {
	for _, o := range list {
		b := extDataOf(o, kindArrayBuffer)
		if b.detached {
			return c.r.TypeError("DataCloneError: a detached ArrayBuffer could not be transferred")
		}
		*extDataOf(c.memory[o], kindArrayBuffer) = arrayBuffer{data: b.data, max: b.max}
		b.data, b.detached = nil, true
	}
	return nil
}

// cloneFrame is an object whose copy still needs its contents: the
// enumerable own string keys of an Array or ordinary object, or the entry
// snapshot of a Map (key, value, key, value, ...) or Set.
type cloneFrame struct {
	src, dst *Object
	keys     []PropertyKey
	items    []Value
	coll     *collection // dst's entries (Map or Set)
	isMap    bool
	host     bool  // src was converted from Go (cloner.hostDepth)
	key      Value // a Map entry's copied key, waiting for its value
	i        int
}

type cloner struct {
	r         *Realm
	memory    map[*Object]*Object
	stack     []cloneFrame
	steps     int64
	hostDepth int // frames of the stack whose src was converted from Go
}

func (c *cloner) run(v Value) (Value, error) {
	out, err := c.clone(v)
	if err != nil {
		return Undefined(), err
	}
	for len(c.stack) > 0 {
		c.steps++
		if err := interruptEvery(c.r, c.steps); err != nil {
			return Undefined(), err
		}
		if err := c.step(); err != nil {
			return Undefined(), err
		}
	}
	return out, nil
}

// step copies one property or collection element of the innermost frame.
func (c *cloner) step() error {
	r := c.r
	top := len(c.stack) - 1
	f := &c.stack[top]
	if f.coll != nil {
		if f.i == len(f.items) {
			c.pop()
			return nil
		}
		v := f.items[f.i]
		f.i++
		out, err := c.clone(v)
		if err != nil {
			return err
		}
		// clone may have pushed a frame (and moved the stack): the frame
		// is still at index top.
		f = &c.stack[top]
		switch {
		case !f.isMap:
			f.coll.add(c.r, out)
		case f.i%2 == 1:
			f.key = out
		default:
			f.coll.set(c.r, f.key, out)
		}
		return nil
	}
	for f.i < len(f.keys) {
		key := f.keys[f.i]
		f.i++
		src, dst := f.src, f.dst
		cell, ok := src.getOwnCell(key)
		if !ok {
			continue
		}
		v := cell.value
		if cell.attrs&attrAccessor != 0 {
			var err error
			if v, err = src.GetProp(r, key); err != nil {
				return err
			}
		}
		out, err := c.clone(v)
		if err != nil {
			return err
		}
		_, err = dst.CreateDataProperty(r, key, out)
		return err
	}
	c.pop()
	return nil
}

// pop drops the innermost frame.
func (c *cloner) pop() {
	top := len(c.stack) - 1
	if c.stack[top].host {
		c.hostDepth--
	}
	c.stack = c.stack[:top]
}

// clone returns v's copy, pushing a frame for the contents of a new
// Array, ordinary object, Map or Set copy.
func (c *cloner) clone(v Value) (Value, error) {
	r := c.r
	if !v.IsObject() {
		if v.IsSymbol() {
			return Undefined(), c.uncloneable(v)
		}
		return v, nil
	}
	o := v.AsObject()
	if dst, ok := c.memory[o]; ok {
		return ObjectValue(dst), nil
	}
	var dst *Object
	var frame cloneFrame
	switch o.class {
	case ClassBoolean, ClassNumber, ClassString, ClassBigInt:
		pv, _ := o.PrimitiveValue()
		dst, _ = r.ToObject(pv)
	case ClassDate:
		d, ok := dataOk(o, kindDate)
		if !ok {
			return Undefined(), c.uncloneable(v)
		}
		dst = r.newDateObject(r.DatePrototype, d.tv)
	case ClassRegExp:
		d := o.RegExpData()
		if d == nil {
			return Undefined(), c.uncloneable(v)
		}
		var err error
		if dst, err = r.regexpFromStrings(r.RegExpPrototype, d.Source(), d.Flags()); err != nil {
			return Undefined(), err
		}
	case ClassMap, ClassSet:
		src, ok := dataOk(o, kindCollection)
		if !ok {
			return Undefined(), c.uncloneable(v)
		}
		proto := r.MapPrototype
		if o.class == ClassSet {
			proto = r.SetPrototype
		}
		var coll *collection
		dst, coll = r.newCollectionObject(proto, o.class)
		frame = cloneFrame{coll: coll, isMap: o.class == ClassMap, items: collectionSnapshot(src, o.class == ClassMap)}
	case ClassError:
		if o.ErrorData() == nil {
			return Undefined(), c.uncloneable(v)
		}
		var err error
		if dst, err = c.cloneError(o); err != nil {
			return Undefined(), err
		}
	case ClassArray:
		dst = r.NewArrayLen(o.ArrayLength())
		frame = cloneFrame{src: o, dst: dst, keys: o.OwnEnumerableStringKeys()}
	case ClassArrayBuffer, ClassSharedArrayBuffer, ClassDataView, ClassTypedArray:
		var err error
		if dst, err = c.cloneBinary(o); err != nil {
			return Undefined(), err
		}
	case ClassObject:
		switch o.Internal().(type) { // Internal hides a host map node's Go map
		case nil, *lazyProps, *lazyKeys, *pendingCompile, *pendingDescs, *Realm: // deferred properties, not a payload
		default:
			return Undefined(), c.uncloneable(v)
		}
		dst = r.NewObject()
		frame = cloneFrame{src: o, dst: dst, keys: o.OwnEnumerableStringKeys()}
	default:
		return Undefined(), c.uncloneable(v)
	}
	c.remember(o, dst)
	if len(frame.keys) != 0 || len(frame.items) != 0 {
		if o.flags&flagHostNode != 0 {
			if c.hostDepth >= maxCloneDepth {
				return Undefined(), r.RangeError("Maximum call stack size exceeded")
			}
			c.hostDepth++
			frame.host = true
		}
		c.stack = append(c.stack, frame)
	}
	return ObjectValue(dst), nil
}

// maxCloneDepth bounds how many objects converted from Go a clone copies
// nested in each other. The explicit stack costs no Go stack, so JavaScript
// nesting is not bounded, but a Go container that contains itself converts
// to an unbounded tree of fresh objects (hostlazy.go), whose copy would grow
// until memory ran out.
const maxCloneDepth = MaxToGoDepth

// remember records dst as the copy of o in the memory map.
func (c *cloner) remember(o, dst *Object) {
	if c.memory == nil {
		c.memory = make(map[*Object]*Object)
	}
	c.memory[o] = dst
}

// cloneBinary copies a buffer or a view. An ArrayBuffer's copy has a copy
// of its bytes, and is resizable to the same maximum when it is; a
// SharedArrayBuffer's shares its data block. A view's copy has its type,
// offset and length (length-tracking if it is) over the copy of its
// buffer. A detached buffer and a view out of its buffer's bounds throw.
// No property is copied.
func (c *cloner) cloneBinary(o *Object) (*Object, error) {
	r := c.r
	var v *dataView
	ta, isTA := dataOk(o, kindTypedArray)
	switch x := o.payload().(type) {
	case *arrayBuffer:
		if o.class == ClassSharedArrayBuffer {
			return r.shareBuffer(x), nil
		}
		if x.detached {
			return nil, r.TypeError("DataCloneError: a detached ArrayBuffer could not be cloned")
		}
		data := r.newBytes(len(x.data), len(x.data))
		if err := r.copyBytes(data, x.data); err != nil {
			return nil, err
		}
		return r.newBufferObject(r.binaryIntr().ArrayBufferPrototype, ClassArrayBuffer, data, x.max), nil
	case *typedArray:
		v = &x.view
	case *dataView:
		v = x
	default:
		return nil, c.uncloneable(ObjectValue(o))
	}
	if v.byteLength() < 0 {
		name := "DataView"
		if isTA {
			name = typedArrayCtorNames[ta.kind].GoString()
		}
		return nil, r.TypeError("DataCloneError: an out-of-bounds %s could not be cloned", name)
	}
	buf, err := c.clone(ObjectValue(v.buf))
	if err != nil {
		return nil, err
	}
	b := r.binaryIntr()
	if isTA {
		return r.newTypedArrayObject(b.typedArrayPrototypes[ta.kind], ta.kind, buf.AsObject(), v.offset, v.length), nil
	}
	return r.newDataViewObject(b.DataViewPrototype, buf.AsObject(), v.offset, v.length), nil
}

// collectionSnapshot copies a Map's live entries as key, value pairs or a
// Set's as keys (the spec's copiedList: entries added while the clone runs
// are not visited).
func collectionSnapshot(src *collection, isMap bool) []Value {
	if src.size() == 0 {
		return nil
	}
	n := src.size()
	if isMap {
		n *= 2
	}
	items := make([]Value, 0, n)
	var buf [8][]collEntry
	for _, s := range src.t.segments(buf[:0]) {
		for i := range s {
			e := &s[i]
			if e.key.IsHole() {
				continue
			}
			items = append(items, e.key)
			if isMap {
				items = append(items, e.value)
			}
		}
	}
	return items
}

// cloneError copies an Error: its name picks one of the native error
// prototypes (Error for any other name) and an own data message is
// converted to a string. Other properties, cause included, are not copied
// (HTML leaves the rest to the user agent); the copy's stack is captured
// at the structuredClone call.
func (c *cloner) cloneError(o *Object) (*Object, error) {
	r := c.r
	name, err := o.GetProp(r, StringKey(AtomName))
	if err != nil {
		return nil, err
	}
	kind := KindError
	if name.IsString() {
		for k := KindError; k < numErrorKinds; k++ {
			if name.AsString().Equals(k.Name()) {
				kind = k
				break
			}
		}
	}
	message := Undefined()
	if cell, ok := o.getOwnCell(StringKey(AtomMessage)); ok && cell.attrs&attrAccessor == 0 {
		s, err := r.ToString(cell.value)
		if err != nil {
			return nil, err
		}
		message = StringValue(s)
	}
	return r.newErrorObject(r.errorProtos[kind], message), nil
}

func (c *cloner) uncloneable(v Value) error {
	return c.r.TypeError("DataCloneError: %s could not be cloned", c.r.DisplayString(v))
}
