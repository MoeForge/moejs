package engine

// Intl.PluralRules (ECMA-402 §17): the cardinal and ordinal rules of every
// CLDR language (intl_number_tables.go), the digit options of
// SetNumberFormatDigitOptions and the plural ranges of pluralRanges.xml.
// The available locales are CLDR's, as Intl.Segmenter's, and the four
// languages with rules but no locale data; the rules of a locale are those
// of its tag truncated until CLDR has some.
//
// With notation "compact", "scientific" or "engineering", a number selects
// with the exponent its notation gives it (the c and e operands), as ICU
// selects on a formatted number: the compact exponents are those of the
// locale's data locale (its tag truncated, else the default locale).

import "unsafe"

func installIntlPluralRules(r *Realm, x *intlIntrinsics) {
	a := intlNames()
	n := intlNumberNames()
	x.PluralRulesPrototype = r.newIntrinsic(ClassObject, r.ObjectPrototype, 5)
	x.PluralRulesCtor = r.newConstructor(n.pluralRulesName, 0, requireNew("Intl.PluralRules"), intlPluralRulesConstruct, x.PluralRulesPrototype)
	r.installBuiltins(x.PluralRulesCtor, []builtinDef{{a.supportedLocalesOf, intlPluralRulesSupportedLocalesOf, 1}})
	r.installBuiltins(x.PluralRulesPrototype, []builtinDef{
		{a.resolvedOptions, intlPluralRulesResolvedOptions, 0},
		{n.selectName, intlPluralRulesSelect, 1},
		{n.selectRange, intlPluralRulesSelectRange, 2},
	})
	r.installToStringTag(x.PluralRulesPrototype, n.pluralRulesTag)
	r.installValues(x.Intl, []valueDef{{n.pluralRulesName, ObjectValue(x.PluralRulesCtor), attrHidden}})
}

// intlPluralAvailable reports whether a locale is an available locale of
// Intl.PluralRules: CLDR's, and the languages plurals.xml has rules for
// without locale data (ars, guw, nah, smi), which ICU has too.
func intlPluralAvailable(tag string) bool {
	if langTagAvailable(tag) {
		return true
	}
	v, ok := intlRecord(intlPluralLocales, tag)
	return ok && v[0] != ','
}

// intlPluralRulesSupportedLocalesOf implements
// Intl.PluralRules.supportedLocalesOf(locales, options).
func intlPluralRulesSupportedLocalesOf(r *Realm, this Value, args []Value) (Value, error) {
	return r.intlFilterLocales(args, intlPluralAvailable)
}

// intlPluralRulesData is the internal of an Intl.PluralRules.
type intlPluralRulesData struct {
	locale  *String
	ordinal bool
	rules   *intlPluralRuleSet
	// nf holds the notation, the digit options and the compact data that
	// give a number its formatted string and exponent.
	nf intlNumberFormat
}

// intlPluralRulesConstruct implements Intl.PluralRules(locales, options).
func intlPluralRulesConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	const ctor = "Intl.PluralRules"
	x := r.intlIntr()
	proto, err := r.GetPrototypeFromConstructor(newTarget, x.PluralRulesCtor, x.PluralRulesPrototype)
	if err != nil {
		return Undefined(), err
	}
	requested, err := r.canonicalizeLocaleList(intlArg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	opts, err := r.coerceOptionsToObject(intlArg(args, 1))
	if err != nil {
		return Undefined(), err
	}
	if _, err := r.intlLocaleMatcher(opts, ctor); err != nil {
		return Undefined(), err
	}
	locale := ""
	for _, l := range requested {
		if locale = intlLookupIn(l, intlPluralAvailable); locale != "" {
			break
		}
	}
	if locale == "" {
		locale = r.intlDefaultFor(intlPluralAvailable)
	}
	n := intlNumberNames()
	pr := &intlPluralRulesData{locale: r.intlString(locale)}
	t, err := r.intlStringOption(opts, ctor, n.typeName, n.cardinal, n.ordinal)
	if err != nil {
		return Undefined(), err
	}
	pr.ordinal = t == n.ordinal
	notation, err := r.intlStringOption(opts, ctor, n.notation, n.standard, n.scientific, n.engineering, n.compact)
	if err != nil {
		return Undefined(), err
	}
	nf := &pr.nf
	switch notation {
	case n.scientific:
		nf.notation = intlNotationScientific
	case n.engineering:
		nf.notation = intlNotationEngineering
	case n.compact:
		nf.notation = intlNotationCompact
	}
	cd, err := r.intlStringOption(opts, ctor, n.compactDisplay, n.short, n.long)
	if err != nil {
		return Undefined(), err
	}
	if nf.notation == intlNotationCompact {
		nf.compactLong = cd == n.long
	}
	if nf.priority, err = r.intlSetDigitOptions(&nf.digits, opts, ctor, 0, 3, nf.notation == intlNotationCompact); err != nil {
		return Undefined(), err
	}
	pr.rules = intlPluralRules(locale, pr.ordinal)
	if nf.notation == intlNotationCompact {
		i, ok := intlDataIndex(intlLookupIn(locale, intlHasData))
		if !ok {
			i, _ = intlDataIndex(r.intlDefaultFor(intlHasData))
		}
		sys := &intlNumLocaleAt(i).systems[0]
		table := sys.compactShort
		if nf.compactLong && sys.compactLong != "" {
			table = sys.compactLong
		}
		nf.compact = parseIntlCompact(table)
	}
	size := int(unsafe.Sizeof(intlPluralRulesData{}))
	if nf.compact != nil {
		size += int(unsafe.Sizeof(intlCompactData{}))
	}
	r.charge(size)
	r.markPrototype(proto)
	o := r.newObject(ClassIntlPluralRules, r.rootShapeFor(proto))
	setExtData(o, kindIntlPluralRules, pr)
	return ObjectValue(o), nil
}

// resolve implements ResolvePlural: the category of a number (consumed)
// and its formatted string, appended to buf.
func (pr *intlPluralRulesData) resolve(d *intlDecimal, buf []byte) (intlPluralCategory, []byte) {
	if d.kind != intlFinite {
		// NaN and the infinities: other, and a string that is not a
		// finite number's.
		return intlPluralOther, append(buf, "\x00"[0], byte(d.kind), boolByte(d.neg))
	}
	nf := &pr.nf
	e := 0
	if nf.notation != intlNotationStandard && !d.isZero() {
		e = nf.computeExponent(d)
		d.scale(-e)
	}
	buf = nf.digits.formatNumericToString(d, buf)
	ops := intlOperands(buf, e)
	if e != 0 {
		// The exponent is part of what the number formats to.
		buf = append(buf, 'e', byte(e), byte(e>>8))
	}
	return pr.rules.category(&ops), buf
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

// intlPluralRulesSelect implements Intl.PluralRules.prototype.select(value).
func intlPluralRulesSelect(r *Realm, this Value, args []Value) (Value, error) {
	pr, err := thisIntl[*intlPluralRulesData](r, this, ClassIntlPluralRules, "Intl.PluralRules.prototype.select")
	if err != nil {
		return Undefined(), err
	}
	var db [32]byte
	d := intlDecimal{digits: db[:0]}
	if err := r.toIntlMathematicalValue(intlArg(args, 0), &d); err != nil {
		return Undefined(), err
	}
	var sb [64]byte
	c, _ := pr.resolve(&d, sb[:0])
	return StringValue(intlPluralName(c)), nil
}

// intlPluralRulesSelectRange implements
// Intl.PluralRules.prototype.selectRange(start, end).
func intlPluralRulesSelectRange(r *Realm, this Value, args []Value) (Value, error) {
	pr, err := thisIntl[*intlPluralRulesData](r, this, ClassIntlPluralRules, "Intl.PluralRules.prototype.selectRange")
	if err != nil {
		return Undefined(), err
	}
	start, end := intlArg(args, 0), intlArg(args, 1)
	if start.IsUndefined() || end.IsUndefined() {
		return Undefined(), r.TypeError("start and end are required")
	}
	var xb, yb [32]byte
	x := intlDecimal{digits: xb[:0]}
	y := intlDecimal{digits: yb[:0]}
	if err := r.toIntlMathematicalValue(start, &x); err != nil {
		return Undefined(), err
	}
	if err := r.toIntlMathematicalValue(end, &y); err != nil {
		return Undefined(), err
	}
	if x.kind == intlNaN || y.kind == intlNaN {
		return Undefined(), r.RangeError("Invalid range: NaN")
	}
	var sx, sy [64]byte
	cx, fx := pr.resolve(&x, sx[:0])
	cy, fy := pr.resolve(&y, sy[:0])
	if string(fx) == string(fy) {
		return StringValue(intlPluralName(cx)), nil
	}
	return StringValue(intlPluralName(intlPluralRange(intlPluralRangesOf(pr.locale.GoString()), cx, cy))), nil
}

// intlPluralName returns the name of a category.
func intlPluralName(c intlPluralCategory) *String {
	n := intlNumberNames()
	return [...]*String{n.zero, n.one, n.two, n.few, n.many, n.other}[c]
}

// intlPluralRulesResolvedOptions implements
// Intl.PluralRules.prototype.resolvedOptions().
func intlPluralRulesResolvedOptions(r *Realm, this Value, args []Value) (Value, error) {
	pr, err := thisIntl[*intlPluralRulesData](r, this, ClassIntlPluralRules, "Intl.PluralRules.prototype.resolvedOptions")
	if err != nil {
		return Undefined(), err
	}
	a := intlNames()
	n := intlNumberNames()
	o := r.NewObjectCap(14)
	add := func(name *String, v Value) {
		o.addNamed(r, StringKey(name), propCell{value: v, attrs: attrDefault})
	}
	num := func(name *String, v int) {
		if v >= 0 {
			add(name, IntValue(v))
		}
	}
	nf := &pr.nf
	d := &nf.digits
	add(a.locale, StringValue(pr.locale))
	t := n.cardinal
	if pr.ordinal {
		t = n.ordinal
	}
	add(n.typeName, StringValue(t))
	add(n.notation, StringValue([...]*String{n.standard, n.scientific, n.engineering, n.compact}[nf.notation]))
	if nf.notation == intlNotationCompact {
		cd := n.short
		if nf.compactLong {
			cd = n.long
		}
		add(n.compactDisplay, StringValue(cd))
	}
	num(n.minimumIntegerDigits, d.minInt)
	num(n.minimumFractionDigits, d.minFrac)
	num(n.maximumFractionDigits, d.maxFrac)
	num(n.minimumSignificantDigits, d.minSig)
	num(n.maximumSignificantDigits, d.maxSig)
	cats := pr.rules.categories()
	items := make([]Value, len(cats))
	for i, c := range cats {
		items[i] = StringValue(intlPluralName(c))
	}
	add(n.pluralCategories, ObjectValue(r.NewArrayFromSlice(items)))
	num(n.roundingIncrement, d.increment)
	add(n.roundingMode, StringValue([...]*String{n.ceil, n.floor, n.expand, n.trunc, n.halfCeil, n.halfFloor, n.halfExpand, n.halfTrunc, n.halfEven}[d.mode]))
	add(n.roundingPriority, StringValue([...]*String{n.auto, n.morePrecision, n.lessPrecision}[nf.priority]))
	tzd := n.auto
	if d.stripIfInteger {
		tzd = n.stripIfInteger
	}
	add(n.trailingZeroDisplay, StringValue(tzd))
	return ObjectValue(o), nil
}
