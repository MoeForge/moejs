package engine

// Intl (ECMA-402): the namespace object, the abstract operations its
// constructors share (locale lists, locale matching, options) and
// Intl.getCanonicalLocales. The constructors are in builtin_intl_locale.go
// and builtin_intl_segmenter.go, the language tag machinery in langtag.go
// and the text segmentation in segment.go.
//
// Intl is a late group (lateGlobal): a realm defines it on the first lookup
// of its name, the shared realms build it once per process, and the data
// behind it (CLDR aliases and likely subtags, the Unicode break properties)
// is decoded on first use and shared by the process. A program that never
// names Intl pays nothing.
//
// The default locale is "en-US" whatever the environment, so a program
// formats and segments the same on every host. The available locales are
// CLDR's (langTagAvailable); the "best fit" matcher is the lookup matcher.

import (
	"slices"
	"strings"
	"unicode/utf16"
)

func init() { lateGlobal(StringKey(AtomIntl), installIntl) }

var lateIntl = lateIndex(StringKey(AtomIntl))

// intlDefaultLocale is DefaultLocale (ECMA-402 §6.2.3).
const intlDefaultLocale = "en-US"

// intlIntrinsics are the intrinsics of the Intl group.
type intlIntrinsics struct {
	Intl                                        *Object
	LocalePrototype, LocaleCtor                 *Object
	SegmenterPrototype, SegmenterCtor           *Object
	SegmentsPrototype, SegmentIteratorPrototype *Object
	// segmentIterNextFn is the original %IntlSegmentIteratorPrototype%.next,
	// whose iterators iterStep steps directly.
	segmentIterNextFn *Object

	NumberFormatPrototype, NumberFormatCtor     *Object
	PluralRulesPrototype, PluralRulesCtor       *Object
	DateTimeFormatPrototype, DateTimeFormatCtor *Object
}

// intlIntr returns the realm's Intl intrinsics, defining the Intl group if
// the realm has yet to.
func (r *Realm) intlIntr() *intlIntrinsics {
	r.lateAt(lateIntl)
	return r.intl
}

func (x *intlIntrinsics) visit(visit func(*Object)) {
	for _, o := range [...]*Object{
		x.Intl, x.LocalePrototype, x.LocaleCtor, x.SegmenterPrototype, x.SegmenterCtor,
		x.SegmentsPrototype, x.SegmentIteratorPrototype, x.segmentIterNextFn,
		x.NumberFormatPrototype, x.NumberFormatCtor, x.PluralRulesPrototype, x.PluralRulesCtor,
		x.DateTimeFormatPrototype, x.DateTimeFormatCtor,
	} {
		if o != nil {
			visit(o)
		}
	}
}

// installIntl is the late installer of Intl.
func installIntl(r *Realm) {
	a := intlNames()
	x := &intlIntrinsics{}
	r.intl = x
	x.Intl = r.newIntrinsic(ClassObject, r.ObjectPrototype, 8)
	x.Intl.flags &^= flagIsPrototype
	r.installBuiltins(x.Intl, []builtinDef{{a.getCanonicalLocales, intlGetCanonicalLocales, 1}})
	installIntlLocale(r, x, a)
	installIntlLocaleInfo(r, x)
	installIntlSegmenter(r, x, a)
	// Each binds its constructor on Intl.
	installIntlNumberFormat(r, x)
	installIntlPluralRules(r, x)
	installIntlDateTimeFormat(r, x)
	r.installValues(x.Intl, []valueDef{
		{a.localeName, ObjectValue(x.LocaleCtor), attrHidden},
		{a.segmenterName, ObjectValue(x.SegmenterCtor), attrHidden},
	})
	r.installToStringTag(x.Intl, AtomIntl)
	r.bindGlobal(AtomIntl, ObjectValue(x.Intl))
}

// intlArg returns args[i], undefined when absent.
func intlArg(args []Value, i int) Value {
	if i < len(args) {
		return args[i]
	}
	return Undefined()
}

// thisIntl implements the receiver check of the Intl members: this must
// be an object of class with an internal of type T.
func thisIntl[T any](r *Realm, this Value, class Class, method string) (T, error) {
	if this.IsObject() && this.AsObject().class == class {
		if d, ok := this.AsObject().payload().(T); ok {
			return d, nil
		}
	}
	var zero T
	return zero, r.TypeError("Method %s called on incompatible receiver %s", method, r.DisplayString(this))
}

// intlString makes the String Intl returns to JavaScript for s, a language
// tag or a part of one, and charges it to the realm's memory account as the
// other builtins charge the strings they make: a byte per character when s
// is ASCII, two per UTF-16 unit otherwise.
func (r *Realm) intlString(s string) *String {
	n := len(s)
	if !isASCII(s) {
		n = 0
		for _, c := range s {
			n += 2 * utf16.RuneLen(c)
		}
	}
	r.chargeString(n)
	return FromGoString(s)
}

// intlGetCanonicalLocales implements Intl.getCanonicalLocales(locales).
func intlGetCanonicalLocales(r *Realm, this Value, args []Value) (Value, error) {
	tags, err := r.canonicalizeLocaleList(intlArg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	return ObjectValue(r.intlTagArray(tags)), nil
}

// intlTagArray makes an array of language tags.
func (r *Realm) intlTagArray(tags []string) *Object {
	items := make([]Value, len(tags))
	for i, t := range tags {
		items[i] = StringValue(r.intlString(t))
	}
	return r.NewArrayFromSlice(items)
}

// canonicalizeLocaleList implements CanonicalizeLocaleList (ECMA-402
// §9.2.1): the canonical tags of locales, a tag, an Intl.Locale or an
// array-like of them, without duplicates.
func (r *Realm) canonicalizeLocaleList(locales Value) ([]string, error) {
	if locales.IsUndefined() {
		return nil, nil
	}
	if locales.IsString() || locales.IsObject() && locales.AsObject().class == ClassIntlLocale {
		tag, err := r.intlLocaleListElement(locales)
		if err != nil {
			return nil, err
		}
		return []string{tag}, nil
	}
	o, err := r.ToObject(locales)
	if err != nil {
		return nil, err
	}
	n, err := r.LengthOfArrayLike(o)
	if err != nil {
		return nil, err
	}
	var seen []string
	// dup indexes seen once it is long enough for the scan to cost more
	// than a map.
	var dup map[string]struct{}
	for k := range n {
		if err := interruptEvery(r, k); err != nil {
			return nil, err
		}
		key := indexKey(r, k)
		ok, err := r.hasProperty(o, key)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		v, err := o.Get(r, key, ObjectValue(o))
		if err != nil {
			return nil, err
		}
		if !v.IsString() && !v.IsObject() {
			return nil, r.TypeError("Language ID should be string or object.")
		}
		tag, err := r.intlLocaleListElement(v)
		if err != nil {
			return nil, err
		}
		if dup == nil && len(seen) == 32 {
			dup = make(map[string]struct{}, 2*len(seen))
			for _, s := range seen {
				dup[s] = struct{}{}
			}
		}
		if dup != nil {
			if _, ok := dup[tag]; ok {
				continue
			}
			dup[tag] = struct{}{}
		} else if slices.Contains(seen, tag) {
			continue
		}
		seen = append(seen, tag)
	}
	return seen, nil
}

// intlLocaleListElement returns the canonical tag of an element of a locale
// list: an Intl.Locale's [[Locale]], or a string (or an object converted
// with ToString) that is a well-formed language tag.
func (r *Realm) intlLocaleListElement(v Value) (string, error) {
	if v.IsObject() {
		if l, ok := extDataOk(v.AsObject(), kindIntlLocale); ok && v.AsObject().class == ClassIntlLocale {
			return l.str.GoString(), nil
		}
	}
	s, err := r.ToString(v)
	if err != nil {
		return "", err
	}
	tag, ok := canonicalizeLangTag(s.GoString())
	if !ok {
		return "", r.RangeError("Incorrect locale information provided")
	}
	return tag, nil
}

// intlLookupLocale implements LookupMatchingLocaleByPrefix (ECMA-402
// §9.2.3), which also serves for LookupMatchingLocaleByBestFit, for one
// requested locale: the longest prefix of the locale without its Unicode
// extension that CLDR has, or "" when there is none.
func intlLookupLocale(locale string) string { return intlLookupIn(locale, langTagAvailable) }

// intlLookupIn implements LookupMatchingLocaleByPrefix for the available
// locales avail tells. No available locale has an extension or more than
// ltAvailableMaxSubtags subtags, and the specification's walk, which drops
// the last subtag and a singleton it leaves last, reaches the language id
// after cutting the extensions. So the walk starts at the language id's
// first ltAvailableMaxSubtags subtags and reads no more of a long tag. A
// match shorter than the locale is copied, so that it does not keep a long
// tag alive.
func intlLookupIn(locale string, avail func(string) bool) string {
	prefix := locale
	for i, n := 0, 0; i < len(prefix); i++ {
		if prefix[i] != '-' {
			continue
		}
		if n++; n == ltAvailableMaxSubtags || i+2 < len(prefix) && prefix[i+2] == '-' {
			prefix = prefix[:i] // the subtags past the longest locale, or an extension
			break
		}
	}
	for prefix != "" {
		if avail(prefix) {
			if len(prefix) < len(locale) {
				prefix = strings.Clone(prefix)
			}
			return prefix
		}
		i := strings.LastIndexByte(prefix, '-')
		prefix = prefix[:max(i, 0)] // a language id has no singleton to drop
	}
	return ""
}

// intlResolveLocale implements ResolveLocale (ECMA-402 §9.2.7) for a
// constructor without relevant extension keys whose available locales are
// CLDR's: the first requested locale CLDR has a match for, else the
// realm's default locale (intlDefaultFor).
func (r *Realm) intlResolveLocale(requested []string) string {
	for _, l := range requested {
		if m := intlLookupLocale(l); m != "" {
			return m
		}
	}
	return r.intlDefaultFor(langTagAvailable)
}

// intlSupportedLocalesOf is the body of supportedLocalesOf(locales,
// options) (FilterLocales, ECMA-402 §9.2.9) of the constructors whose
// available locales are CLDR's.
func intlSupportedLocalesOf(r *Realm, this Value, args []Value) (Value, error) {
	return r.intlFilterLocales(args, langTagAvailable)
}

// intlSupportedLocalesOfData is supportedLocalesOf of the constructors
// with locale data (intlHasData).
func intlSupportedLocalesOfData(r *Realm, this Value, args []Value) (Value, error) {
	return r.intlFilterLocales(args, intlHasData)
}

// intlFilterLocales implements FilterLocales for the available locales
// avail tells.
func (r *Realm) intlFilterLocales(args []Value, avail func(string) bool) (Value, error) {
	requested, err := r.canonicalizeLocaleList(intlArg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	options, err := r.coerceOptionsToObject(intlArg(args, 1))
	if err != nil {
		return Undefined(), err
	}
	if _, err := r.intlLocaleMatcher(options, "supportedLocalesOf"); err != nil {
		return Undefined(), err
	}
	subset := requested[:0]
	for _, l := range requested {
		if intlLookupIn(l, avail) != "" {
			subset = append(subset, l)
		}
	}
	return ObjectValue(r.intlTagArray(subset)), nil
}

// intlLocaleMatcher reads the localeMatcher option; both matchers are the
// lookup matcher.
func (r *Realm) intlLocaleMatcher(options *Object, ctor string) (*String, error) {
	a := intlNames()
	return r.intlStringOption(options, ctor, a.localeMatcher, a.lookup, a.bestFit)
}

// coerceOptionsToObject implements CoerceOptionsToObject (ECMA-402
// §9.2.11). Undefined gives nil, which the option readers take for the
// empty object without a prototype the specification creates.
func (r *Realm) coerceOptionsToObject(options Value) (*Object, error) {
	if options.IsUndefined() {
		return nil, nil
	}
	return r.ToObject(options)
}

// getOptionsObject implements GetOptionsObject (ECMA-402 §9.2.10), with
// nil for undefined as coerceOptionsToObject has.
func (r *Realm) getOptionsObject(options Value) (*Object, error) {
	switch {
	case options.IsUndefined():
		return nil, nil
	case options.IsObject():
		return options.AsObject(), nil
	}
	return nil, r.TypeError("Options must be an object")
}

// intlOption reads property name of options (nil reads undefined).
func (r *Realm) intlOption(options *Object, name *String) (Value, error) {
	if options == nil {
		return Undefined(), nil
	}
	return options.Get(r, StringKey(name), ObjectValue(options))
}

// intlStringOption implements GetOption (ECMA-402 §9.2.12) for a string
// option without a default: nil when the option is undefined, else its
// ToString, which must be one of values when there are any (the matching
// element of values is returned). ctor names the caller in the RangeError.
func (r *Realm) intlStringOption(options *Object, ctor string, name *String, values ...*String) (*String, error) {
	v, err := r.intlOption(options, name)
	if err != nil || v.IsUndefined() {
		return nil, err
	}
	s, err := r.ToString(v)
	if err != nil || len(values) == 0 {
		return s, err
	}
	for _, c := range values {
		if c.Equals(s) {
			return c, nil
		}
	}
	return nil, r.RangeError("Value %s out of range for %s options property %s", s.GoString(), ctor, name.GoString())
}
