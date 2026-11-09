package engine

// Intl.Locale (ECMA-402 §15) over the language tags of langtag.go. The
// Locale info methods (getCalendars, getWeekInfo, ...) are in
// builtin_intl_locale_info.go.

// The localeExtensionKeys of Intl.Locale (%Intl.Locale%.[[LocaleExtensionKeys]]),
// in the order the constructor applies them.
const (
	localeKeyCa = iota
	localeKeyCo
	localeKeyFw
	localeKeyHc
	localeKeyKf
	localeKeyKn
	localeKeyNu
	numLocaleKeys
)

var localeKeyNames = [numLocaleKeys]string{"ca", "co", "fw", "hc", "kf", "kn", "nu"}

// intlLocale is the internal of an Intl.Locale: its [[Locale]], parsed and
// as the string toString returns, and the values of its localeExtensionKeys
// slots ([[Calendar]], ..., nil for undefined; [[Numeric]] is
// kw[localeKeyKn] being "true" or ""). newLocaleRecord makes it and charges
// what it keeps.
type intlLocale struct {
	tag *langTag
	str *String
	kw  [numLocaleKeys]*String
}

func installIntlLocale(r *Realm, x *intlIntrinsics, a *intlAtoms) {
	x.LocalePrototype = r.newIntrinsic(ClassObject, r.ObjectPrototype, 26)
	x.LocaleCtor = r.newConstructor(a.localeName, 1, requireNew("Intl.Locale"), intlLocaleConstruct, x.LocalePrototype)
	r.installBuiltins(x.LocalePrototype, []builtinDef{
		{a.maximize, intlLocaleMaximize, 0},
		{a.minimize, intlLocaleMinimize, 0},
		{AtomToString, intlLocaleToString, 0},
	})
	r.installGetters(x.LocalePrototype, []getterDef{
		{name: a.baseName, get: intlLocaleBaseName},
		{name: a.calendar, get: intlLocaleKeyword(localeKeyCa, "calendar")},
		{name: a.caseFirst, get: intlLocaleKeyword(localeKeyKf, "caseFirst")},
		{name: a.collation, get: intlLocaleKeyword(localeKeyCo, "collation")},
		{name: a.firstDayOfWeek, get: intlLocaleKeyword(localeKeyFw, "firstDayOfWeek")},
		{name: a.hourCycle, get: intlLocaleKeyword(localeKeyHc, "hourCycle")},
		{name: a.numeric, get: intlLocaleNumeric},
		{name: a.numberingSystem, get: intlLocaleKeyword(localeKeyNu, "numberingSystem")},
		{name: a.language, get: intlLocaleSubtag((*langTag).language, "language")},
		{name: a.script, get: intlLocaleSubtag((*langTag).script, "script")},
		{name: a.region, get: intlLocaleSubtag((*langTag).region, "region")},
		{name: a.variants, get: intlLocaleSubtag((*langTag).variants, "variants")},
	})
	r.installToStringTag(x.LocalePrototype, a.localeTag)
}

// intlLocaleConstruct implements Intl.Locale(tag, options).
func intlLocaleConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	const ctor = "Intl.Locale"
	x := r.intlIntr()
	proto, err := r.GetPrototypeFromConstructor(newTarget, x.LocaleCtor, x.LocalePrototype)
	if err != nil {
		return Undefined(), err
	}
	tv := intlArg(args, 0)
	var s string
	switch {
	case tv.IsObject() && tv.AsObject().class == ClassIntlLocale:
		s = extDataOf(tv.AsObject(), kindIntlLocale).str.GoString()
	case tv.IsString() || tv.IsObject():
		str, err := r.ToString(tv)
		if err != nil {
			return Undefined(), err
		}
		s = str.GoString()
	default:
		return Undefined(), r.TypeError("First argument to Intl.Locale constructor can't be empty or missing")
	}
	options, err := r.coerceOptionsToObject(intlArg(args, 1))
	if err != nil {
		return Undefined(), err
	}
	t, ok := parseLangTag(s)
	if !ok {
		return Undefined(), r.RangeError("Incorrect locale information provided")
	}
	t.canonicalize()
	if err := r.updateLanguageID(t, options); err != nil {
		return Undefined(), err
	}

	// The options of the localeExtensionKeys, in localeKeyNames order.
	a := intlNames()
	var opt [numLocaleKeys]*String
	typeOption := func(i int, name *String, toUValue func(*String) *String) error {
		v, err := r.intlStringOption(options, ctor, name)
		if err != nil || v == nil {
			return err
		}
		if toUValue != nil {
			v = toUValue(v)
		}
		if !isLangTagType(v.GoString()) {
			return r.RangeError("Incorrect locale information provided")
		}
		opt[i] = v
		return nil
	}
	if err := typeOption(localeKeyCa, a.calendar, nil); err != nil {
		return Undefined(), err
	}
	if err := typeOption(localeKeyCo, a.collation, nil); err != nil {
		return Undefined(), err
	}
	if err := typeOption(localeKeyFw, a.firstDayOfWeek, weekdayToUValue); err != nil {
		return Undefined(), err
	}
	if opt[localeKeyHc], err = r.intlStringOption(options, ctor, a.hourCycle, a.h11, a.h12, a.h23, a.h24); err != nil {
		return Undefined(), err
	}
	if opt[localeKeyKf], err = r.intlStringOption(options, ctor, a.caseFirst, a.upper, a.lower, a.falseStr); err != nil {
		return Undefined(), err
	}
	kn, err := r.intlOption(options, a.numeric)
	if err != nil {
		return Undefined(), err
	}
	if !kn.IsUndefined() {
		opt[localeKeyKn] = FromGoString("false")
		if ToBoolean(kn) {
			opt[localeKeyKn] = FromGoString("true")
		}
	}
	if err := typeOption(localeKeyNu, a.numberingSystem, nil); err != nil {
		return Undefined(), err
	}

	// MakeLocaleRecord.
	var kw [numLocaleKeys]string
	var has [numLocaleKeys]bool
	for i, key := range localeKeyNames {
		if v := opt[i]; v != nil {
			kw[i], has[i] = langTagCanonicalUValue(key, v.GoString()), true
			t.setUKeyword(key, kw[i])
		} else {
			kw[i], has[i] = t.uKeyword(key)
		}
	}
	t.canonicalize()
	l := r.newLocaleRecord(t, &kw, &has)
	r.markPrototype(proto)
	o := r.newObject(ClassIntlLocale, r.rootShapeFor(proto))
	setExtData(o, kindIntlLocale, l)
	return ObjectValue(o), nil
}

// updateLanguageID implements UpdateLanguageId (ECMA-402 §15.1.2): the
// language, script, region and variants options replace those subtags of t.
func (r *Realm) updateLanguageID(t *langTag, options *Object) error {
	const ctor = "Intl.Locale"
	a := intlNames()
	for _, f := range [...]struct {
		name  *String
		valid func(string) bool
		set   func(*langTag, string)
	}{
		{a.language, isLangTagLanguage, (*langTag).setLanguage},
		{a.script, isLangTagScript, (*langTag).setScript},
		{a.region, isLangTagRegion, (*langTag).setRegion},
		{a.variants, isLangTagVariants, (*langTag).setVariants},
	} {
		v, err := r.intlStringOption(options, ctor, f.name)
		if err != nil {
			return err
		}
		if v == nil {
			continue
		}
		s := v.GoString()
		if !f.valid(s) {
			return r.RangeError("Incorrect locale information provided")
		}
		f.set(t, s)
	}
	return nil
}

// weekdayToUValue implements WeekdayToUValue (ECMA-402 §15.5.15): "0" to
// "7" become the first-day identifiers "sun", "mon", ..., "sun".
func weekdayToUValue(fw *String) *String {
	if fw.Len() == 1 {
		if c := fw.At(0); c >= '0' && c <= '7' {
			return FromGoString([...]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat", "sun"}[c-'0'])
		}
	}
	return fw
}

// newLocaleRecord makes the internal of an Intl.Locale for the canonical
// tag t and the values of its slots (kw[i] when has[i]), and charges what
// the Locale keeps: the [[Locale]] string, the slots' strings and the
// subtags and extensions the tag copied while it was parsed and
// canonicalized.
func (r *Realm) newLocaleRecord(t *langTag, kw *[numLocaleKeys]string, has *[numLocaleKeys]bool) *intlLocale {
	l := &intlLocale{tag: t, str: r.intlString(t.String())}
	for i := range localeKeyNames {
		if has[i] {
			l.kw[i] = r.intlString(kw[i])
		}
	}
	r.charge(t.size())
	return l
}

// newIntlLocale implements Construct(%Intl.Locale%, « tag ») for the tag t
// that maximize or minimize made, which canonicalizes it again.
func (r *Realm) newIntlLocale(t *langTag) Value {
	if c, ok := parseLangTag(t.String()); ok {
		t = c
	}
	t.canonicalize()
	var kw [numLocaleKeys]string
	var has [numLocaleKeys]bool
	for i, key := range localeKeyNames {
		kw[i], has[i] = t.uKeyword(key)
	}
	l := r.newLocaleRecord(t, &kw, &has)
	o := r.newObject(ClassIntlLocale, r.rootShapeFor(r.intlIntr().LocalePrototype))
	setExtData(o, kindIntlLocale, l)
	return ObjectValue(o)
}

func thisLocale(r *Realm, this Value, method string) (*intlLocale, error) {
	return thisIntl[*intlLocale](r, this, ClassIntlLocale, "Intl.Locale.prototype."+method)
}

// intlLocaleMaximize implements Intl.Locale.prototype.maximize().
func intlLocaleMaximize(r *Realm, this Value, args []Value) (Value, error) {
	l, err := thisLocale(r, this, "maximize")
	if err != nil {
		return Undefined(), err
	}
	t := l.tag.clone()
	t.maximize()
	return r.newIntlLocale(t), nil
}

// intlLocaleMinimize implements Intl.Locale.prototype.minimize().
func intlLocaleMinimize(r *Realm, this Value, args []Value) (Value, error) {
	l, err := thisLocale(r, this, "minimize")
	if err != nil {
		return Undefined(), err
	}
	t := l.tag.clone()
	t.minimize()
	return r.newIntlLocale(t), nil
}

// intlLocaleToString implements Intl.Locale.prototype.toString().
func intlLocaleToString(r *Realm, this Value, args []Value) (Value, error) {
	l, err := thisLocale(r, this, "toString")
	if err != nil {
		return Undefined(), err
	}
	return StringValue(l.str), nil
}

func intlLocaleBaseName(r *Realm, this Value, args []Value) (Value, error) {
	l, err := thisLocale(r, this, "baseName")
	if err != nil {
		return Undefined(), err
	}
	return StringValue(r.intlString(l.tag.baseName())), nil
}

func intlLocaleNumeric(r *Realm, this Value, args []Value) (Value, error) {
	l, err := thisLocale(r, this, "numeric")
	if err != nil {
		return Undefined(), err
	}
	kn := l.kw[localeKeyKn]
	return Bool(kn != nil && (kn.Len() == 0 || kn.EqualsGoString("true"))), nil
}

// intlLocaleKeyword returns the getter of the localeExtensionKeys slot i.
func intlLocaleKeyword(i int, name string) NativeFunc {
	return func(r *Realm, this Value, args []Value) (Value, error) {
		l, err := thisLocale(r, this, name)
		if err != nil || l.kw[i] == nil {
			return Undefined(), err
		}
		return StringValue(l.kw[i]), nil
	}
}

// intlLocaleSubtag returns the getter of a subtag of the base name, which
// is undefined when the base name has none.
func intlLocaleSubtag(get func(*langTag) string, name string) NativeFunc {
	return func(r *Realm, this Value, args []Value) (Value, error) {
		l, err := thisLocale(r, this, name)
		if err != nil {
			return Undefined(), err
		}
		s := get(l.tag)
		if s == "" {
			return Undefined(), nil
		}
		return StringValue(r.intlString(s)), nil
	}
}
