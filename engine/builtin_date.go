package engine

import (
	"math"
	"time"
	"unsafe"
)

// Date (ES2024 §21.4, with Annex B getYear, setYear and toGMTString). The
// time value arithmetic and the local time zone are in date_time.go, the
// string forms in date_format.go and Date.parse in date_parse.go.

// DateData is the internal payload of ClassDate objects: the time value in
// milliseconds since the epoch, NaN for an invalid date.
type DateData struct {
	tv float64
}

// TimeValue returns the time value (ms since the epoch; NaN when invalid).
func (d *DateData) TimeValue() float64 { return d.tv }

// dateObject co-allocates a Date instance and its payload.
type dateObject struct {
	obj  Object
	data DateData
}

// DateValue returns the time value of a Date object; ok is false for other
// objects.
func (o *Object) DateValue() (float64, bool) {
	d := o.dateData()
	if d == nil {
		return 0, false
	}
	return d.tv, true
}

func (o *Object) dateData() *DateData {
	if o.class != ClassDate {
		return nil
	}
	d := dataOf(o, kindDate)
	return d
}

// newDateObject creates a Date with prototype proto and time value tv.
func (r *Realm) newDateObject(proto *Object, tv float64) *Object {
	do := &dateObject{}
	r.chargeObject(unsafe.Sizeof(dateObject{}))
	o := &do.obj
	r.markPrototype(proto)
	o.shape = r.rootShapeFor(proto)
	o.class = ClassDate
	o.flags = flagExtensible
	do.data.tv = tv
	setData(o, kindDate, &do.data)
	return o
}

// NewDate creates a Date object for t (Go-callable entry point).
func (r *Realm) NewDate(t time.Time) *Object {
	return r.newDateObject(r.DatePrototype, timeClip(float64(t.UnixMilli())))
}

var (
	atomUTC                = staticAtom("UTC")
	atomToTimeString       = staticAtom("toTimeString")
	atomToGMTString        = staticAtom("toGMTString")
	atomToLocaleDateString = staticAtom("toLocaleDateString")
	atomToLocaleTimeString = staticAtom("toLocaleTimeString")
	atomGetTimezoneOffset  = staticAtom("getTimezoneOffset")
	atomSetTime            = staticAtom("setTime")
	atomGetYear            = staticAtom("getYear")
	atomSetYear            = staticAtom("setYear")
)

// Field indexes of the date and time components the getters and setters
// address: YearFromTime .. msFromTime.
const (
	dfYear = iota
	dfMonth
	dfDate
	dfHours
	dfMinutes
	dfSeconds
	dfMilliseconds
	dfDay // WeekDay; getters only
)

// dateAccessor names one component and its local and UTC accessors.
type dateAccessor struct {
	field  int
	get    string // getFullYear ...
	getUTC string
	set    string // "" for getDay
	setUTC string
}

var dateAccessors = [...]dateAccessor{
	{dfDate, "getDate", "getUTCDate", "setDate", "setUTCDate"},
	{dfDay, "getDay", "getUTCDay", "", ""},
	{dfYear, "getFullYear", "getUTCFullYear", "setFullYear", "setUTCFullYear"},
	{dfHours, "getHours", "getUTCHours", "setHours", "setUTCHours"},
	{dfMilliseconds, "getMilliseconds", "getUTCMilliseconds", "setMilliseconds", "setUTCMilliseconds"},
	{dfMinutes, "getMinutes", "getUTCMinutes", "setMinutes", "setUTCMinutes"},
	{dfMonth, "getMonth", "getUTCMonth", "setMonth", "setUTCMonth"},
	{dfSeconds, "getSeconds", "getUTCSeconds", "setSeconds", "setUTCSeconds"},
}

var dateCtorMethods = []builtinDef{
	{AtomNow, dateNow, 0},
	{AtomParse, dateParse, 1},
	{atomUTC, dateUTC, 7},
}

// dateProtoHead and dateProtoTail surround the component accessors,
// toUTCString/toGMTString (one function object) and @@toPrimitive, in V8's
// property order.
var dateProtoHead = []builtinDef{
	{AtomToString, dateProtoToString, 0},
	{AtomToDateString, dateProtoToDateString, 0},
	{atomToTimeString, dateProtoToTimeString, 0},
	{AtomToISOString, dateProtoToISOString, 0},
}

var dateProtoTail = []builtinDef{
	{AtomGetTime, dateProtoGetTime, 0},
	{atomSetTime, dateProtoSetTime, 1},
	{atomGetTimezoneOffset, dateProtoGetTimezoneOffset, 0},
	{AtomValueOf, dateProtoGetTime, 0},
	{atomGetYear, dateProtoGetYear, 0},
	{atomSetYear, dateProtoSetYear, 1},
	{AtomToJSON, dateProtoToJSON, 1},
	{AtomToLocaleString, dateProtoToLocaleString, 0},
	{atomToLocaleDateString, dateProtoToLocaleDateString, 0},
	{atomToLocaleTimeString, dateProtoToLocaleTimeString, 0},
}

// dateAccessorDefs expands dateAccessors into builtin definitions.
var dateAccessorDefs = func() []builtinDef {
	var defs []builtinDef
	for _, a := range dateAccessors {
		defs = append(defs,
			builtinDef{staticAtom(a.get), dateGetter(a.get, a.field, false), 0},
			builtinDef{staticAtom(a.getUTC), dateGetter(a.getUTC, a.field, true), 0})
		if a.set != "" {
			n := dateSetterArity(a.field)
			defs = append(defs,
				builtinDef{staticAtom(a.set), dateSetter(a.set, a.field, false), n},
				builtinDef{staticAtom(a.setUTC), dateSetter(a.setUTC, a.field, true), n})
		}
	}
	return defs
}()

func installDate(r *Realm) {
	fd := r.DateCtor.FunctionData()
	fd.setNative(dateCall)
	fd.setConstructor(dateConstruct)
	r.installBuiltins(r.DateCtor, dateCtorMethods)
	if r.buildingShared {
		installDatePrototype(r, r.DatePrototype)
		return
	}
	// A mutable realm defines the 50 methods when a program first looks at
	// Date.prototype (a Date's method, its key list, a new property).
	r.DatePrototype.deferInstall(r, installDatePrototypeDeferred)
}

// dateProtoSlab backs the deferred Date.prototype install of a mutable realm
// the way bootstrapSlabs back the intrinsics: the 46 function objects, their
// shapes, first transitions and slots come out of one 18,432-byte block
// instead of 140 allocations (TestDateProtoSlabFilled).
type dateProtoSlab struct {
	boot   bootstrapSlabs
	funcs  [46]nativeFuncObject
	shapes [46]Shape
	trans  [45]transition
	slots  [48]Value
}

func newDateProtoSlab() *dateProtoSlab {
	s := &dateProtoSlab{}
	s.boot = bootstrapSlabs{funcs: s.funcs[:0], shapes: s.shapes[:0], trans: s.trans[:0], slots: s.slots[:0]}
	return s
}

func installDatePrototypeDeferred(r *Realm, p *Object) {
	saved := r.boot
	r.boot = &newDateProtoSlab().boot
	installDatePrototype(r, p)
	r.boot = saved
}

func installDatePrototype(r *Realm, p *Object) {
	p.ReserveSlots(r, len(dateProtoHead)+len(dateAccessorDefs)+len(dateProtoTail)+3)
	r.installBuiltins(p, dateProtoHead)
	utc := ObjectValue(r.NewNativeFunction(AtomToUTCString, 0, dateProtoToUTCString))
	r.installOrReplace(p, StringKey(AtomToUTCString), propCell{value: utc, attrs: attrHidden})
	r.installOrReplace(p, StringKey(atomToGMTString), propCell{value: utc, attrs: attrHidden})
	r.installBuiltins(p, dateAccessorDefs)
	r.installBuiltins(p, dateProtoTail)
	toPrim := r.NewNativeFunction(atomToPrimitiveFn, 1, dateProtoToPrimitive)
	r.installOrReplace(p, SymbolKey(SymToPrimitive), propCell{value: ObjectValue(toPrim), attrs: attrConfigurable})
}

func dateNow(r *Realm, this Value, args []Value) (Value, error) {
	return Int64Value(r.now().UnixMilli()), nil
}

// dateCall implements Date() called as a function: the current time in the
// Date.prototype.toString format.
func dateCall(r *Realm, this Value, args []Value) (Value, error) {
	return StringValue(r.toDateString(r.nowTimeValue())), nil
}

// toDateString implements ToDateString.
func (r *Realm) toDateString(tv float64) *String {
	if tv != tv {
		return asciiString("Invalid Date")
	}
	return r.dateToString(tv)
}

func dateConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	var tv float64
	switch len(args) {
	case 0:
		tv = r.nowTimeValue()
	case 1:
		v := args[0]
		if v.IsObject() {
			if t, ok := v.AsObject().DateValue(); ok {
				tv = t
				break
			}
		}
		p, err := r.ToPrimitive(v, HintDefault)
		if err != nil {
			return Undefined(), err
		}
		if p.IsString() {
			tv = r.parseDateString(p.AsString())
		} else {
			f, err := r.ToNumber(p)
			if err != nil {
				return Undefined(), err
			}
			tv = timeClip(f)
		}
	default:
		d, err := r.dateFromComponents(args)
		if err != nil {
			return Undefined(), err
		}
		tv = r.utc(d)
	}
	proto, err := r.GetPrototypeFromConstructor(newTarget, r.DateCtor, r.DatePrototype)
	if err != nil {
		return Undefined(), err
	}
	return ObjectValue(r.newDateObject(proto, tv)), nil
}

// dateFromComponents coerces (year, month[, date[, hours[, minutes[,
// seconds[, ms]]]]]) in order and returns MakeDate(MakeDay(MakeFullYear(y),
// m, dt), MakeTime(h, min, s, milli)), shared by the constructor and
// Date.UTC. A missing year is NaN, a missing date 1, the rest 0.
func (r *Realm) dateFromComponents(args []Value) (float64, error) {
	c := [7]float64{math.NaN(), 0, 1, 0, 0, 0, 0}
	for i := 0; i < len(args) && i < len(c); i++ {
		f, err := r.ToNumber(args[i])
		if err != nil {
			return 0, err
		}
		c[i] = f
	}
	return makeDate(makeDay(makeFullYear(c[0]), c[1], c[2]), makeTime(c[3], c[4], c[5], c[6])), nil
}

func dateUTC(r *Realm, this Value, args []Value) (Value, error) {
	d, err := r.dateFromComponents(args)
	if err != nil {
		return Undefined(), err
	}
	return NumberValue(timeClip(d)), nil
}

func dateParse(r *Realm, this Value, args []Value) (Value, error) {
	s, err := r.ToString(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	return NumberValue(r.parseDateString(s)), nil
}

// thisDate returns the Date payload of this, or a TypeError naming method.
func thisDate(r *Realm, this Value, method string) (*DateData, error) {
	if this.IsObject() {
		if d := this.AsObject().dateData(); d != nil {
			return d, nil
		}
	}
	return nil, r.TypeError("Date.prototype.%s requires that 'this' be a Date object", method)
}

func thisTimeValue(r *Realm, this Value, method string) (float64, error) {
	d, err := thisDate(r, this, method)
	if err != nil {
		return 0, err
	}
	return d.tv, nil
}

// dateComponents returns the seven components of an integral time value.
func dateComponents(t int64) [7]float64 {
	f := splitTime(t)
	return [7]float64{float64(f.year), float64(f.month), float64(f.day),
		float64(f.hour), float64(f.minute), float64(f.second), float64(f.ms)}
}

// dateGetter builds a component getter: the component of LocalTime(t), or of
// t for the UTC variant; NaN for an invalid date.
func dateGetter(name string, field int, utc bool) NativeFunc {
	return func(r *Realm, this Value, args []Value) (Value, error) {
		tv, err := thisTimeValue(r, this, name)
		if err != nil {
			return Undefined(), err
		}
		if tv != tv {
			return NumberValue(math.NaN()), nil
		}
		t := int64(tv)
		if !utc {
			t = r.localTime(t)
		}
		if field == dfDay {
			return IntValue(int(floorMod(dayOf(t)+4, 7))), nil
		}
		return NumberValue(dateComponents(t)[field]), nil
	}
}

// dateSetterArity is the number of components a setter takes: the date
// setters run up to the day of month, the time setters up to milliseconds.
func dateSetterArity(field int) int {
	if field <= dfDate {
		return dfDate + 1 - field
	}
	return dfMilliseconds + 1 - field
}

// dateSetter builds setFullYear .. setMilliseconds and their UTC variants.
// As the spec orders it, the time value is read first, then every present
// argument (at least the first) is converted with ToNumber, and only then is
// an invalid date checked: setFullYear starts from +0, the others return
// NaN. The date setters keep TimeWithinDay(t); the time setters keep Day(t).
func dateSetter(name string, field int, utc bool) NativeFunc {
	arity := dateSetterArity(field)
	return func(r *Realm, this Value, args []Value) (Value, error) {
		d, err := thisDate(r, this, name)
		if err != nil {
			return Undefined(), err
		}
		tv := d.tv
		n := max(1, min(len(args), arity))
		var vals [4]float64
		for i := range n {
			if vals[i], err = r.ToNumber(Arg(args, i)); err != nil {
				return Undefined(), err
			}
		}
		var t int64
		switch {
		case tv == tv:
			t = int64(tv)
			if !utc {
				t = r.localTime(t)
			}
		case field != dfYear:
			return NumberValue(math.NaN()), nil
		}
		c := dateComponents(t)
		copy(c[field:field+n], vals[:n])
		var nd float64
		if field <= dfDate {
			nd = makeDate(makeDay(c[dfYear], c[dfMonth], c[dfDate]), float64(timeWithinDay(t)))
		} else {
			nd = makeDate(float64(dayOf(t)), makeTime(c[dfHours], c[dfMinutes], c[dfSeconds], c[dfMilliseconds]))
		}
		if utc {
			d.tv = timeClip(nd)
		} else {
			d.tv = r.utc(nd)
		}
		return NumberValue(d.tv), nil
	}
}

func dateProtoGetTime(r *Realm, this Value, args []Value) (Value, error) {
	tv, err := thisTimeValue(r, this, "getTime")
	if err != nil {
		return Undefined(), err
	}
	return NumberValue(tv), nil
}

func dateProtoSetTime(r *Realm, this Value, args []Value) (Value, error) {
	d, err := thisDate(r, this, "setTime")
	if err != nil {
		return Undefined(), err
	}
	f, err := r.ToNumber(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	d.tv = timeClip(f)
	return NumberValue(d.tv), nil
}

// dateProtoGetTimezoneOffset returns (t - LocalTime(t)) / msPerMinute,
// truncated to whole minutes as V8 does for offsets with seconds (local mean
// time before a zone's first transition).
func dateProtoGetTimezoneOffset(r *Realm, this Value, args []Value) (Value, error) {
	tv, err := thisTimeValue(r, this, "getTimezoneOffset")
	if err != nil {
		return Undefined(), err
	}
	if tv != tv {
		return NumberValue(math.NaN()), nil
	}
	return Int64Value(-r.utcOffset(int64(tv)) / msPerMinute), nil
}

// dateProtoGetYear is Annex B getYear: YearFromTime(LocalTime(t)) - 1900.
func dateProtoGetYear(r *Realm, this Value, args []Value) (Value, error) {
	tv, err := thisTimeValue(r, this, "getYear")
	if err != nil {
		return Undefined(), err
	}
	if tv != tv {
		return NumberValue(math.NaN()), nil
	}
	return Int64Value(splitTime(r.localTime(int64(tv))).year - 1900), nil
}

// dateProtoSetYear is Annex B setYear: years 0-99 mean 1900-1999.
func dateProtoSetYear(r *Realm, this Value, args []Value) (Value, error) {
	d, err := thisDate(r, this, "setYear")
	if err != nil {
		return Undefined(), err
	}
	tv := d.tv
	y, err := r.ToNumber(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	var t int64
	if tv == tv {
		t = r.localTime(int64(tv))
	}
	c := dateComponents(t)
	d.tv = r.utc(makeDate(makeDay(makeFullYear(y), c[dfMonth], c[dfDate]), float64(timeWithinDay(t))))
	return NumberValue(d.tv), nil
}

// dateStringMethod builds the methods that format a valid date and return
// "Invalid Date" otherwise.
func dateStringMethod(name string, format func(r *Realm, tv float64) *String) NativeFunc {
	return func(r *Realm, this Value, args []Value) (Value, error) {
		tv, err := thisTimeValue(r, this, name)
		if err != nil {
			return Undefined(), err
		}
		if tv != tv {
			return StringValue(asciiString("Invalid Date")), nil
		}
		return StringValue(format(r, tv)), nil
	}
}

var (
	dateProtoToString     = dateStringMethod("toString", (*Realm).dateToString)
	dateProtoToDateString = dateStringMethod("toDateString", (*Realm).dateToDateString)
	dateProtoToTimeString = dateStringMethod("toTimeString", (*Realm).dateToTimeString)
	dateProtoToUTCString  = dateStringMethod("toUTCString", func(_ *Realm, tv float64) *String {
		return dateToUTCString(tv)
	})
)

func dateProtoToISOString(r *Realm, this Value, args []Value) (Value, error) {
	tv, err := thisTimeValue(r, this, "toISOString")
	if err != nil {
		return Undefined(), err
	}
	if tv != tv {
		return Undefined(), r.RangeError("Invalid time value")
	}
	return StringValue(asciiString(formatISO(tv))), nil
}

func dateProtoToJSON(r *Realm, this Value, args []Value) (Value, error) {
	o, err := r.ToObject(this)
	if err != nil {
		return Undefined(), err
	}
	tv, err := r.ToPrimitive(ObjectValue(o), HintNumber)
	if err != nil {
		return Undefined(), err
	}
	if tv.IsNumber() {
		if f := tv.AsNumber(); f != f || math.IsInf(f, 0) {
			return Null(), nil
		}
	}
	fn, err := o.GetProp(r, StringKey(AtomToISOString))
	if err != nil {
		return Undefined(), err
	}
	return r.Call(fn, ObjectValue(o), nil)
}

// dateProtoToPrimitive implements Date.prototype[@@toPrimitive](hint):
// "string" and "default" try toString first, "number" valueOf first.
func dateProtoToPrimitive(r *Realm, this Value, args []Value) (Value, error) {
	if !this.IsObject() {
		return Undefined(), r.TypeError("Date.prototype[Symbol.toPrimitive] called on non-object")
	}
	hint := Arg(args, 0)
	if hint.IsString() {
		switch s := hint.AsString(); {
		case s.EqualsGoString("string"), s.EqualsGoString("default"):
			return r.OrdinaryToPrimitive(this.AsObject(), HintString)
		case s.EqualsGoString("number"):
			return r.OrdinaryToPrimitive(this.AsObject(), HintNumber)
		}
	}
	return Undefined(), r.TypeError("Invalid hint: %s", r.DisplayString(hint))
}
