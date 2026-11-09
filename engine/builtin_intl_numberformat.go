package engine

// Intl.NumberFormat (ECMA-402 §16): the constructor reads its options in
// the specification's order (InitializeNumberFormat, SetNumberFormatUnitOptions,
// SetNumberFormatDigitOptions) and resolves the locale among the data
// locales with the "nu" key; the formatting is intl_number_format.go's.
//
// The constructor's normative optional legacy mode (ChainNumberFormat,
// UnwrapNumberFormat) is not implemented.

import (
	"strings"
	"unsafe"
)

func installIntlNumberFormat(r *Realm, x *intlIntrinsics) {
	a := intlNames()
	n := intlNumberNames()
	x.NumberFormatPrototype = r.newIntrinsic(ClassObject, r.ObjectPrototype, 7)
	x.NumberFormatCtor = r.newConstructor(n.numberFormatName, 0, intlNumberFormatCall, intlNumberFormatConstruct, x.NumberFormatPrototype)
	r.installBuiltins(x.NumberFormatCtor, []builtinDef{{a.supportedLocalesOf, intlSupportedLocalesOfData, 1}})
	r.installBuiltins(x.NumberFormatPrototype, []builtinDef{{a.resolvedOptions, intlNumberFormatResolvedOptions, 0}})
	r.installGetters(x.NumberFormatPrototype, []getterDef{{n.format, n.formatGetter, intlNumberFormatFormat}})
	r.installBuiltins(x.NumberFormatPrototype, []builtinDef{
		{n.formatRange, intlNumberFormatFormatRange, 2},
		{n.formatRangeToParts, intlNumberFormatFormatRangeToParts, 2},
		{n.formatToParts, intlNumberFormatFormatToParts, 1},
	})
	r.installToStringTag(x.NumberFormatPrototype, n.numberFormatTag)
	r.installValues(x.Intl, []valueDef{{n.numberFormatName, ObjectValue(x.NumberFormatCtor), attrHidden}})
}

// intlNumberFormatCall implements Intl.NumberFormat called as a function: a
// new NumberFormat, as if constructed.
func intlNumberFormatCall(r *Realm, this Value, args []Value) (Value, error) {
	return intlNumberFormatConstruct(r, args, nil)
}

// intlNumberFormatConstruct implements Intl.NumberFormat(locales, options).
func intlNumberFormatConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	x := r.intlIntr()
	proto, err := r.GetPrototypeFromConstructor(newTarget, x.NumberFormatCtor, x.NumberFormatPrototype)
	if err != nil {
		return Undefined(), err
	}
	nf, err := r.intlNewNumberFormat(intlArg(args, 0), intlArg(args, 1))
	if err != nil {
		return Undefined(), err
	}
	r.markPrototype(proto)
	o := r.newObject(ClassIntlNumberFormat, r.rootShapeFor(proto))
	setExtData(o, kindIntlNumberFormat, nf)
	return ObjectValue(o), nil
}

// intlNewNumberFormat implements the steps of the NumberFormat constructor
// after OrdinaryCreateFromConstructor: the options read and resolved, the
// formatter set up.
func (r *Realm) intlNewNumberFormat(locales, options Value) (*intlNumberFormat, error) {
	const ctor = "Intl.NumberFormat"
	a := intlNames()
	n := intlNumberNames()
	requested, err := r.canonicalizeLocaleList(locales)
	if err != nil {
		return nil, err
	}
	opts, err := r.coerceOptionsToObject(options)
	if err != nil {
		return nil, err
	}
	if _, err := r.intlLocaleMatcher(opts, ctor); err != nil {
		return nil, err
	}
	nu, err := r.intlStringOption(opts, ctor, a.numberingSystem)
	if err != nil {
		return nil, err
	}
	if nu != nil && !isLangTagType(nu.GoString()) {
		return nil, r.RangeError("Invalid numberingSystem : %s", nu.GoString())
	}
	res := r.intlResolve(requested, intlHasData, []intlRelevantKey{{key: "nu", option: nu, values: intlNumberingValuesOf}})
	nf := &intlNumberFormat{locale: r.intlString(res.locale), nu: res.values[0]}
	nf.nuString = r.intlString(nf.nu)

	// SetNumberFormatUnitOptions.
	style, err := r.intlStringOption(opts, ctor, n.style, n.decimal, n.percent, n.currency, n.unit)
	if err != nil {
		return nil, err
	}
	switch style {
	case n.percent:
		nf.style = intlStylePercent
	case n.currency:
		nf.style = intlStyleCurrency
	case n.unit:
		nf.style = intlStyleUnit
	}
	currency, err := r.intlStringOption(opts, ctor, n.currency)
	if err != nil {
		return nil, err
	}
	if currency == nil {
		if nf.style == intlStyleCurrency {
			return nil, r.TypeError("Currency code is required with currency style.")
		}
	} else if !intlWellFormedCurrency(currency.GoString()) {
		return nil, r.RangeError("Invalid currency code : %s", currency.GoString())
	}
	cd, err := r.intlStringOption(opts, ctor, n.currencyDisplay, n.code, n.symbol, n.narrowSymbol, n.name)
	if err != nil {
		return nil, err
	}
	cs, err := r.intlStringOption(opts, ctor, n.currencySign, n.standard, n.accounting)
	if err != nil {
		return nil, err
	}
	unit, err := r.intlStringOption(opts, ctor, n.unit)
	if err != nil {
		return nil, err
	}
	if unit == nil {
		if nf.style == intlStyleUnit {
			return nil, r.TypeError("Unit is required with unit style.")
		}
	} else if !intlWellFormedUnit(unit.GoString()) {
		return nil, r.RangeError("Invalid unit argument for Intl.NumberFormat() '%s'", unit.GoString())
	}
	ud, err := r.intlStringOption(opts, ctor, n.unitDisplay, n.short, n.narrow, n.long)
	if err != nil {
		return nil, err
	}
	if nf.style == intlStyleCurrency {
		nf.currency = strings.ToUpper(currency.GoString())
		nf.currencyString = r.intlString(nf.currency)
		switch cd {
		case n.code:
			nf.currencyDisplay = intlCurrencyCode
		case n.narrowSymbol:
			nf.currencyDisplay = intlCurrencyNarrow
		case n.name:
			nf.currencyDisplay = intlCurrencyName
		}
		nf.accounting = cs == n.accounting
	}
	nf.unitDisplay = intlUnitShort
	if nf.style == intlStyleUnit {
		nf.unit = unit.GoString()
		nf.unitString = r.intlString(nf.unit)
		switch ud {
		case n.narrow:
			nf.unitDisplay = intlUnitNarrow
		case n.long:
			nf.unitDisplay = intlUnitLong
		}
	}

	notation, err := r.intlStringOption(opts, ctor, n.notation, n.standard, n.scientific, n.engineering, n.compact)
	if err != nil {
		return nil, err
	}
	switch notation {
	case n.scientific:
		nf.notation = intlNotationScientific
	case n.engineering:
		nf.notation = intlNotationEngineering
	case n.compact:
		nf.notation = intlNotationCompact
	}
	mnfdDefault, mxfdDefault := 0, 3
	switch {
	case nf.style == intlStyleCurrency && nf.notation == intlNotationStandard:
		c := intlCurrencyDigitsOf(nf.currency)
		mnfdDefault, mxfdDefault = c, c
	case nf.style == intlStylePercent:
		mxfdDefault = 0
	}
	if nf.priority, err = r.intlSetDigitOptions(&nf.digits, opts, ctor, mnfdDefault, mxfdDefault, nf.notation == intlNotationCompact); err != nil {
		return nil, err
	}
	compactDisplay, err := r.intlStringOption(opts, ctor, n.compactDisplay, n.short, n.long)
	if err != nil {
		return nil, err
	}
	defaultGrouping := intlGroupingAuto
	if nf.notation == intlNotationCompact {
		nf.compactLong = compactDisplay == n.long
		defaultGrouping = intlGroupingMin2
	}
	if nf.useGrouping, err = r.intlUseGrouping(opts, defaultGrouping); err != nil {
		return nil, err
	}
	sd, err := r.intlStringOption(opts, ctor, n.signDisplay, n.auto, n.never, n.always, n.exceptZero, n.negative)
	if err != nil {
		return nil, err
	}
	switch sd {
	case n.never:
		nf.signDisplay = intlSignNever
	case n.always:
		nf.signDisplay = intlSignAlways
	case n.exceptZero:
		nf.signDisplay = intlSignExceptZero
	case n.negative:
		nf.signDisplay = intlSignNegative
	}
	i, _ := intlDataIndex(res.dataLocale)
	nf.setup(i)
	// The formatter's own data: its rendered affixes and names.
	r.charge(nf.size())
	return nf, nil
}

// intlWellFormedCurrency implements IsWellFormedCurrencyCode (ECMA-402
// §6.3.1).
func intlWellFormedCurrency(s string) bool {
	if len(s) != 3 {
		return false
	}
	for i := range 3 {
		if c := s[i] | 0x20; c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}

// intlWellFormedUnit implements IsWellFormedUnitIdentifier (ECMA-402
// §6.6.1).
func intlWellFormedUnit(s string) bool {
	if intlIsSanctionedUnit(s) {
		return true
	}
	num, den, ok := strings.Cut(s, "-per-")
	return ok && !strings.Contains(den, "-per-") && intlIsSanctionedUnit(num) && intlIsSanctionedUnit(den)
}

// intlUseGrouping implements the useGrouping steps of the constructor with
// GetBooleanOrStringNumberFormatOption (ECMA-402 §9.2.12).
func (r *Realm) intlUseGrouping(opts *Object, def uint8) (uint8, error) {
	n := intlNumberNames()
	v, err := r.intlOption(opts, n.useGrouping)
	if err != nil || v.IsUndefined() {
		return def, err
	}
	if v.IsTrue() {
		return intlGroupingAlways, nil
	}
	if !ToBoolean(v) {
		return intlGroupingOff, nil
	}
	s, err := r.ToString(v)
	if err != nil {
		return 0, err
	}
	switch {
	case s.Equals(n.min2):
		return intlGroupingMin2, nil
	case s.Equals(n.auto):
		return intlGroupingAuto, nil
	case s.Equals(n.always):
		return intlGroupingAlways, nil
	case s.Equals(n.trueStr) || s.EqualsGoString("false"):
		// Accepted for historical reasons: the default.
		return def, nil
	}
	return 0, r.RangeError("Value %s out of range for Intl.NumberFormat options property useGrouping", s.GoString())
}

// intlDefaultNumberOption implements DefaultNumberOption (ECMA-402
// §9.2.13); ok is false for undefined (the fallback undefined).
func (r *Realm) intlDefaultNumberOption(v Value, minimum, maximum int, name *String) (int, bool, error) {
	if v.IsUndefined() {
		return 0, false, nil
	}
	f, err := r.ToNumber(v)
	if err != nil {
		return 0, false, err
	}
	if f != f || f < float64(minimum) || f > float64(maximum) {
		return 0, false, r.RangeError("%s value is out of range.", name.GoString())
	}
	return int(f), true, nil
}

// intlNumberOption implements GetNumberOption (ECMA-402 §9.2.14).
func (r *Realm) intlNumberOption(opts *Object, name *String, minimum, maximum, fallback int) (int, error) {
	v, err := r.intlOption(opts, name)
	if err != nil {
		return 0, err
	}
	n, ok, err := r.intlDefaultNumberOption(v, minimum, maximum, name)
	if !ok {
		n = fallback
	}
	return n, err
}

// intlSetDigitOptions implements SetNumberFormatDigitOptions (ECMA-402
// §16.1.2) and returns [[ComputedRoundingPriority]]. The slots it leaves
// undefined are negative (minFrac and maxFrac, or minSig and maxSig).
func (r *Realm) intlSetDigitOptions(o *intlDigitOptions, opts *Object, ctor string, mnfdDefault, mxfdDefault int, compact bool) (uint8, error) {
	n := intlNumberNames()
	mnid, err := r.intlNumberOption(opts, n.minimumIntegerDigits, 1, 21, 1)
	if err != nil {
		return 0, err
	}
	var vals [4]Value
	for i, name := range []*String{n.minimumFractionDigits, n.maximumFractionDigits, n.minimumSignificantDigits, n.maximumSignificantDigits} {
		if vals[i], err = r.intlOption(opts, name); err != nil {
			return 0, err
		}
	}
	mnfd, mxfd, mnsd, mxsd := vals[0], vals[1], vals[2], vals[3]
	o.minInt = mnid
	inc, err := r.intlNumberOption(opts, n.roundingIncrement, 1, 5000, 1)
	if err != nil {
		return 0, err
	}
	switch inc {
	case 1, 2, 5, 10, 20, 25, 50, 100, 200, 250, 500, 1000, 2000, 2500, 5000:
	default:
		return 0, r.RangeError("roundingIncrement value is out of range.")
	}
	modes := []*String{n.ceil, n.floor, n.expand, n.trunc, n.halfCeil, n.halfFloor, n.halfExpand, n.halfTrunc, n.halfEven}
	mode, err := r.intlStringOption(opts, ctor, n.roundingMode, modes...)
	if err != nil {
		return 0, err
	}
	prio, err := r.intlStringOption(opts, ctor, n.roundingPriority, n.auto, n.morePrecision, n.lessPrecision)
	if err != nil {
		return 0, err
	}
	tzd, err := r.intlStringOption(opts, ctor, n.trailingZeroDisplay, n.auto, n.stripIfInteger)
	if err != nil {
		return 0, err
	}
	if inc != 1 {
		mxfdDefault = mnfdDefault
	}
	o.increment = inc
	o.mode = intlRoundHalfExpand
	for i, m := range modes {
		if m == mode {
			o.mode = intlRoundingMode(i)
		}
	}
	o.stripIfInteger = tzd == n.stripIfInteger
	hasSd := !mnsd.IsUndefined() || !mxsd.IsUndefined()
	hasFd := !mnfd.IsUndefined() || !mxfd.IsUndefined()
	needSd, needFd := true, true
	if prio == nil || prio == n.auto {
		needSd = hasSd
		if needSd || !hasFd && compact {
			needFd = false
		}
	}
	o.minFrac, o.maxFrac, o.minSig, o.maxSig = -1, -1, -1, -1
	if needSd {
		o.minSig, o.maxSig = 1, 21
		if hasSd {
			lo, _, err := r.intlDefaultNumberOption(mnsd, 1, 21, n.minimumSignificantDigits)
			if err != nil {
				return 0, err
			}
			if mnsd.IsUndefined() {
				lo = 1
			}
			hi, ok, err := r.intlDefaultNumberOption(mxsd, lo, 21, n.maximumSignificantDigits)
			if err != nil {
				return 0, err
			}
			if !ok {
				hi = 21
			}
			o.minSig, o.maxSig = lo, hi
		}
	}
	if needFd {
		lo, hi := mnfdDefault, mxfdDefault
		if hasFd {
			l, lok, err := r.intlDefaultNumberOption(mnfd, 0, 100, n.minimumFractionDigits)
			if err != nil {
				return 0, err
			}
			h, hok, err := r.intlDefaultNumberOption(mxfd, 0, 100, n.maximumFractionDigits)
			if err != nil {
				return 0, err
			}
			switch {
			case !lok:
				lo, hi = min(mnfdDefault, h), h
			case !hok:
				lo, hi = l, max(mxfdDefault, l)
			case l > h:
				return 0, r.RangeError("maximumFractionDigits value is out of range.")
			default:
				lo, hi = l, h
			}
		}
		o.minFrac, o.maxFrac = lo, hi
	}
	priority := intlPriorityAuto
	switch {
	case !needSd && !needFd:
		o.minFrac, o.maxFrac, o.minSig, o.maxSig = 0, 0, 1, 2
		o.roundingType = intlMorePrecision
		priority = intlPriorityMore
	case prio == n.morePrecision:
		o.roundingType = intlMorePrecision
		priority = intlPriorityMore
	case prio == n.lessPrecision:
		o.roundingType = intlLessPrecision
		priority = intlPriorityLess
	case hasSd:
		o.roundingType = intlSignificantDigits
	default:
		o.roundingType = intlFractionDigits
	}
	if inc != 1 {
		if o.roundingType != intlFractionDigits {
			return 0, r.TypeError("roundingIncrement requires fraction digits rounding")
		}
		if o.maxFrac != o.minFrac {
			return 0, r.RangeError("maximumFractionDigits must equal minimumFractionDigits with a roundingIncrement")
		}
	}
	return priority, nil
}

// toIntlMathematicalValue implements ToIntlMathematicalValue (ECMA-402
// §16.5.15) into d.
func (r *Realm) toIntlMathematicalValue(v Value, d *intlDecimal) error {
	prim, err := r.ToPrimitive(v, HintNumber)
	if err != nil {
		return err
	}
	switch {
	case prim.IsBigInt():
		d.setBigInt(prim.AsBigInt())
	case prim.IsString():
		d.setString(prim.AsString())
	default:
		f, err := r.ToNumber(prim)
		if err != nil {
			return err
		}
		d.setFloat(f)
	}
	return nil
}

// format implements FormatNumeric of any value (ToIntlMathematicalValue).
func (nf *intlNumberFormat) format(r *Realm, x Value) (*String, error) {
	if x.IsNumber() {
		return nf.formatFloat(x.AsNumber()), nil
	}
	var db [32]byte
	d := intlDecimal{digits: db[:0]}
	if err := r.toIntlMathematicalValue(x, &d); err != nil {
		return nil, err
	}
	return nf.formatDecimal(&d), nil
}

// intlNumberFormatFormat implements get Intl.NumberFormat.prototype.format:
// a function bound to the NumberFormat, cached on it.
func intlNumberFormatFormat(r *Realm, this Value, args []Value) (Value, error) {
	nf, err := thisIntl[*intlNumberFormat](r, this, ClassIntlNumberFormat, "get Intl.NumberFormat.prototype.format")
	if err != nil {
		return Undefined(), err
	}
	if nf.boundFormat == nil {
		nf.boundFormat = r.NewNativeDataFunction(emptyString, 1, intlNumberFormatBound, nf)
	}
	return ObjectValue(nf.boundFormat), nil
}

func intlNumberFormatBound(r *Realm, fd *FunctionData, this Value, args []Value) (Value, error) {
	s, err := fd.nat().data.(*intlNumberFormat).format(r, intlArg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	return StringValue(r.intlCharged(s)), nil
}

// intlNumberFormatFormatToParts implements
// Intl.NumberFormat.prototype.formatToParts(value).
func intlNumberFormatFormatToParts(r *Realm, this Value, args []Value) (Value, error) {
	nf, err := thisIntl[*intlNumberFormat](r, this, ClassIntlNumberFormat, "Intl.NumberFormat.prototype.formatToParts")
	if err != nil {
		return Undefined(), err
	}
	var db [32]byte
	d := intlDecimal{digits: db[:0]}
	if err := r.toIntlMathematicalValue(intlArg(args, 0), &d); err != nil {
		return Undefined(), err
	}
	var ub [96]uint16
	var rb [16]intlRun
	var pb [16]intlPart
	u, parts := nf.formatToParts(&d, ub[:0], rb[:0], pb[:0])
	return ObjectValue(r.intlPartsArray(nf, u, parts, false)), nil
}

// intlNumberFormatFormatRange implements
// Intl.NumberFormat.prototype.formatRange(start, end).
func intlNumberFormatFormatRange(r *Realm, this Value, args []Value) (Value, error) {
	nf, x, y, err := intlRangeArgs(r, this, args, "Intl.NumberFormat.prototype.formatRange")
	if err != nil {
		return Undefined(), err
	}
	u, _ := nf.formatRange(&x, &y, false)
	return StringValue(r.intlCharged(intlStringOf(u))), nil
}

// intlNumberFormatFormatRangeToParts implements
// Intl.NumberFormat.prototype.formatRangeToParts(start, end).
func intlNumberFormatFormatRangeToParts(r *Realm, this Value, args []Value) (Value, error) {
	nf, x, y, err := intlRangeArgs(r, this, args, "Intl.NumberFormat.prototype.formatRangeToParts")
	if err != nil {
		return Undefined(), err
	}
	u, parts := nf.formatRange(&x, &y, true)
	return ObjectValue(r.intlPartsArray(nf, u, parts, true)), nil
}

// intlRangeArgs implements the steps formatRange and formatRangeToParts
// share: the receiver, the values and the NaN check of
// PartitionNumberRangePattern.
func intlRangeArgs(r *Realm, this Value, args []Value, method string) (*intlNumberFormat, intlDecimal, intlDecimal, error) {
	var x, y intlDecimal
	nf, err := thisIntl[*intlNumberFormat](r, this, ClassIntlNumberFormat, method)
	if err != nil {
		return nil, x, y, err
	}
	start, end := intlArg(args, 0), intlArg(args, 1)
	if start.IsUndefined() || end.IsUndefined() {
		return nil, x, y, r.TypeError("start and end are required")
	}
	if err := r.toIntlMathematicalValue(start, &x); err != nil {
		return nil, x, y, err
	}
	if err := r.toIntlMathematicalValue(end, &y); err != nil {
		return nil, x, y, err
	}
	if x.kind == intlNaN || y.kind == intlNaN {
		return nil, x, y, r.RangeError("Invalid range: NaN")
	}
	return nf, x, y, nil
}

// intlPartObject is a part object of formatToParts with its slots, in one
// allocation.
type intlPartObject struct {
	obj   Object
	slots [3]Value
}

// intlPartsArray makes the array of formatToParts (or formatRangeToParts,
// with the sources), with V8's names of the fields: each part object is one
// allocation, and its value one more unless it is one ASCII character.
func (r *Realm) intlPartsArray(nf *intlNumberFormat, u []uint16, parts []intlPart, sources bool) *Object {
	n := intlNumberNames()
	shape := r.plainRoot.addProperty(r, StringKey(n.typeName), attrDefault).addProperty(r, StringKey(n.value), attrDefault)
	if sources {
		shape = shape.addProperty(r, StringKey(n.source), attrDefault)
	}
	items := make([]Value, len(parts))
	for i, p := range parts {
		text := u[p.start:p.end]
		var typ *String
		switch p.field {
		case intlFieldLiteral:
			typ = n.literal
		case intlFieldInteger:
			typ = n.integer
		case intlFieldGroup:
			typ = n.group
		case intlFieldDecimal:
			typ = n.decimal
		case intlFieldFraction:
			typ = n.fraction
		case intlFieldSign:
			typ = n.minusSign
			if text[0] == '+' {
				typ = n.plusSign
			}
		case intlFieldPercent:
			typ = n.percentSign
			if nf.style == intlStyleUnit {
				typ = n.unit
			}
		case intlFieldCurrency:
			typ = n.currency
		case intlFieldUnit:
			typ = n.unit
		case intlFieldCompact:
			typ = n.compact
		case intlFieldExpSeparator:
			typ = n.exponentSeparator
		case intlFieldExpMinus:
			typ = n.exponentMinusSign
		case intlFieldExpInteger:
			typ = n.exponentInteger
		case intlFieldApprox:
			typ = n.approximatelySign
		case intlFieldNaN:
			typ = n.nan
		case intlFieldInfinity:
			typ = n.infinity
		}
		d := &intlPartObject{}
		r.chargeObject(unsafe.Sizeof(intlPartObject{}))
		o := &d.obj
		o.slots = d.slots[:2]
		o.shape = shape
		o.class = ClassObject
		o.flags = flagExtensible
		var value *String
		if len(text) == 1 && text[0] < 0x80 {
			value = singleCharStrings[text[0]]
		} else {
			value = r.intlCharged(intlStringOf(text))
		}
		o.slots[0] = StringValue(typ)
		o.slots[1] = StringValue(value)
		if sources {
			src := n.shared
			switch p.source {
			case 1:
				src = n.startRange
			case 2:
				src = n.endRange
			}
			o.slots = d.slots[:3]
			o.slots[2] = StringValue(src)
		}
		items[i] = ObjectValue(o)
	}
	return r.NewArrayFromSlice(items)
}

// intlNumberFormatResolvedOptions implements
// Intl.NumberFormat.prototype.resolvedOptions().
func intlNumberFormatResolvedOptions(r *Realm, this Value, args []Value) (Value, error) {
	nf, err := thisIntl[*intlNumberFormat](r, this, ClassIntlNumberFormat, "Intl.NumberFormat.prototype.resolvedOptions")
	if err != nil {
		return Undefined(), err
	}
	a := intlNames()
	n := intlNumberNames()
	o := r.NewObjectCap(20)
	add := func(name *String, v Value) {
		o.addNamed(r, StringKey(name), propCell{value: v, attrs: attrDefault})
	}
	str := func(name, v *String) { add(name, StringValue(v)) }
	num := func(name *String, v int) {
		if v >= 0 {
			add(name, IntValue(v))
		}
	}
	str(a.locale, nf.locale)
	add(a.numberingSystem, StringValue(nf.nuString))
	str(n.style, [...]*String{n.decimal, n.percent, n.currency, n.unit}[nf.style])
	if nf.style == intlStyleCurrency {
		add(n.currency, StringValue(nf.currencyString))
		str(n.currencyDisplay, [...]*String{n.symbol, n.code, n.narrowSymbol, n.name}[nf.currencyDisplay])
		sign := n.standard
		if nf.accounting {
			sign = n.accounting
		}
		str(n.currencySign, sign)
	}
	if nf.style == intlStyleUnit {
		add(n.unit, StringValue(nf.unitString))
		str(n.unitDisplay, [...]*String{n.long, n.short, n.narrow}[nf.unitDisplay])
	}
	d := &nf.digits
	num(n.minimumIntegerDigits, d.minInt)
	num(n.minimumFractionDigits, d.minFrac)
	num(n.maximumFractionDigits, d.maxFrac)
	num(n.minimumSignificantDigits, d.minSig)
	num(n.maximumSignificantDigits, d.maxSig)
	switch nf.useGrouping {
	case intlGroupingOff:
		add(n.useGrouping, False())
	default:
		str(n.useGrouping, [...]*String{n.auto, n.min2, n.always}[nf.useGrouping])
	}
	str(n.notation, [...]*String{n.standard, n.scientific, n.engineering, n.compact}[nf.notation])
	if nf.notation == intlNotationCompact {
		cd := n.short
		if nf.compactLong {
			cd = n.long
		}
		str(n.compactDisplay, cd)
	}
	str(n.signDisplay, [...]*String{n.auto, n.never, n.always, n.exceptZero, n.negative}[nf.signDisplay])
	num(n.roundingIncrement, d.increment)
	str(n.roundingMode, [...]*String{n.ceil, n.floor, n.expand, n.trunc, n.halfCeil, n.halfFloor, n.halfExpand, n.halfTrunc, n.halfEven}[d.mode])
	str(n.roundingPriority, [...]*String{n.auto, n.morePrecision, n.lessPrecision}[nf.priority])
	tzd := n.auto
	if d.stripIfInteger {
		tzd = n.stripIfInteger
	}
	str(n.trailingZeroDisplay, tzd)
	return ObjectValue(o), nil
}
