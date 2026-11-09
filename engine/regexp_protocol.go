package engine

import (
	"math/bits"
	"unsafe"
)

// The RegExp Symbol protocol (ES2025 §22.2.6): RegExp.prototype[@@match],
// [@@matchAll], [@@replace], [@@search] and [@@split], RegExpExec,
// %RegExpStringIteratorPrototype% and IsRegExp. String.prototype.match,
// matchAll, replace, replaceAll, search and split dispatch to these methods
// through the argument's well-known-symbol properties (builtin_string.go).
//
// The generic algorithms read `exec`, `flags`, `lastIndex` and the result
// objects through [[Get]], so a RegExp subclass or a replaced exec is
// observed step by step. A pristine RegExp -- an instance with its creation
// shape (own lastIndex only, prototype %RegExp.prototype%) while the
// prototype properties an algorithm reads are the originals -- runs the
// direct matchers instead (regexpMatch, regexpReplace, regexpSplit), which
// produce the same results without the per-match result objects.

// The %RegExp.prototype% properties the protocol reads, watched by
// regexpGuard.
const (
	rxgExec = iota
	rxgFlags
	rxgHasIndices
	rxgGlobal
	rxgIgnoreCase
	rxgMultiline
	rxgDotAll
	rxgUnicode
	rxgUnicodeSets
	rxgSticky
	rxgMatch
	rxgMatchAll
	rxgReplace
	rxgSearch
	rxgSplit
	rxgConstructor
	rxgTest
	numRegExpGuards
)

var (
	execKey        = StringKey(AtomExec)
	flagsKey       = StringKey(AtomFlags)
	zeroKey        = IndexKey(0)
	resultIndexKey = StringKey(AtomIndex)
	groupsKey      = StringKey(AtomGroups)
)

var regexpGuardKeys = [numRegExpGuards]PropertyKey{
	rxgExec:        execKey,
	rxgFlags:       flagsKey,
	rxgHasIndices:  StringKey(AtomHasIndices),
	rxgGlobal:      StringKey(AtomGlobal),
	rxgIgnoreCase:  StringKey(AtomIgnoreCase),
	rxgMultiline:   StringKey(AtomMultiline),
	rxgDotAll:      StringKey(AtomDotAll),
	rxgUnicode:     StringKey(AtomUnicode),
	rxgUnicodeSets: StringKey(AtomUnicodeSets),
	rxgSticky:      StringKey(AtomSticky),
	rxgMatch:       SymbolKey(SymMatch),
	rxgMatchAll:    SymbolKey(SymMatchAll),
	rxgReplace:     SymbolKey(SymReplace),
	rxgSearch:      SymbolKey(SymSearch),
	rxgSplit:       SymbolKey(SymSplit),
	rxgConstructor: constructorKey,
	rxgTest:        StringKey(AtomTest),
}

// Masks of regexpGuard properties an algorithm depends on.
const (
	rxExec     = 1 << rxgExec
	rxFlags    = (1<<(rxgSticky+1) - 1) &^ rxExec // get flags and the eight flag getters it reads
	rxMatch    = 1 << rxgMatch
	rxMatchAll = 1 << rxgMatchAll
	rxReplace  = 1 << rxgReplace
	rxSearch   = 1 << rxgSearch
	rxSplit    = 1 << rxgSplit
	rxSpecies  = 1 << rxgConstructor // SpeciesConstructor(rx, %RegExp%) is %RegExp% (with guardRegExpSpecies)
	rxTest     = 1 << rxgTest
)

// regexpGuard watches the %RegExp.prototype% properties of regexpGuardKeys
// in a mutable realm: want holds the original values (recorded by
// installRegExp) and slot their slots in shape, the prototype shape they
// were last resolved at. Replacing a method or getter writes a new value
// (defineProperty of an accessor allocates a new pair), which the slot
// compare sees; adding or deleting properties changes the shape. A
// dictionary-mode prototype is never cached, so it keeps every RegExp on
// the generic algorithms.
type regexpGuard struct {
	shape   *Shape
	missing uint32 // keys absent at shape
	slot    [numRegExpGuards]uint32
	want    [numRegExpGuards]Value
}

func (g *regexpGuard) rearm(p *Object) bool {
	g.shape = nil
	if p.flags&(flagDict|flagHasLazy) != 0 && !p.onlyLegacyPending() {
		return false // compile, which no path watches, may be pending
	}
	g.missing = 0
	for i, key := range regexpGuardKeys {
		slot, _, ok := p.shape.Lookup(key)
		if !ok {
			g.missing |= 1 << i
			continue
		}
		g.slot[i] = slot
	}
	g.shape = p.shape
	return true
}

// pristineRegExp returns rx's payload when rx is a RegExp with the realm's
// instance shape and the prototype properties in mask are intact, else nil.
// The instance shape implies the prototype is %RegExp.prototype% and the
// only own property is a writable lastIndex (an ordinary object shaped
// alike has no payload). A shared realm's intrinsics are frozen; in a
// mutable realm the properties in mask must hold their original values
// (and, with rxSpecies, %RegExp%[@@species] its original getter). The
// guard is checked in this one frame: it runs on every String.prototype
// match/replace/search/split and RegExp.prototype.test call.
func (r *Realm) pristineRegExp(rx *Object, mask uint32) *RegExpData {
	st := r.regexps
	if st == nil || rx.shape != st.shape {
		return nil
	}
	if !r.sharedIntrinsics {
		g, p := &r.regexpGuard, r.RegExpPrototype
		if p.shape != g.shape && !g.rearm(p) || mask&g.missing != 0 {
			return nil
		}
		for m := mask; m != 0; m &= m - 1 {
			i := bits.TrailingZeros32(m)
			if p.slots[g.slot[i]] != g.want[i] {
				return nil
			}
		}
		if mask&rxSpecies != 0 && !r.guardHolds(&r.protoGuards[guardRegExpSpecies]) {
			return nil
		}
	}
	return rx.RegExpData()
}

// regexpProtoIntact reports whether the %RegExp.prototype% properties in
// mask hold their original values: pristineRegExp's check of the
// prototype, for code that has no instance to check (literalTest).
func (r *Realm) regexpProtoIntact(mask uint32) bool {
	if r.sharedIntrinsics {
		return true
	}
	g, p := &r.regexpGuard, r.RegExpPrototype
	if p.shape != g.shape && !g.rearm(p) || mask&g.missing != 0 {
		return false
	}
	for m := mask; m != 0; m &= m - 1 {
		i := bits.TrailingZeros32(m)
		if p.slots[g.slot[i]] != g.want[i] {
			return false
		}
	}
	return true
}

// --- abstract operations ------------------------------------------------------------

// isRegExp implements IsRegExp(v).
func (r *Realm) isRegExp(v Value) (bool, error) {
	if !v.IsObject() {
		return false, nil
	}
	o := v.AsObject()
	if r.pristineRegExp(o, rxMatch) != nil {
		return true, nil
	}
	d := o.RegExpData()
	if d == nil && r.lacksWellKnown(o, regexpGuardKeys[rxgMatch]) {
		return false, nil
	}
	m, err := o.GetProp(r, regexpGuardKeys[rxgMatch])
	if err != nil {
		return false, err
	}
	if !m.IsUndefined() {
		return ToBoolean(m), nil
	}
	return d != nil, nil
}

// regexpExecMethod performs the lookup of RegExpExec(rx, S): d is non-nil
// when RegExpBuiltinExec applies (exec is the original
// RegExp.prototype.exec, or not callable, on a RegExp), else exec is the
// method to call.
func (r *Realm) regexpExecMethod(rx *Object) (d *RegExpData, exec Value, err error) {
	if d := r.pristineRegExp(rx, rxExec); d != nil {
		return d, Undefined(), nil
	}
	if exec, err = rx.GetProp(r, execKey); err != nil {
		return nil, Undefined(), err
	}
	callable := IsCallable(exec)
	if callable && exec != r.regexpGuard.want[rxgExec] {
		return nil, exec, nil
	}
	if d := rx.RegExpData(); d != nil {
		return d, Undefined(), nil
	}
	if callable {
		return nil, Undefined(), r.TypeError("RegExp.prototype.exec requires that 'this' be a RegExp object")
	}
	return nil, Undefined(), r.TypeError("RegExp exec method called on an incompatible receiver %s", r.DisplayString(ObjectValue(rx)))
}

// regexpExec implements RegExpExec(rx, s): the match result object or null.
func (r *Realm) regexpExec(rx *Object, s *String) (Value, error) {
	d, exec, err := r.regexpExecMethod(rx)
	if err != nil {
		return Undefined(), err
	}
	if d != nil {
		return r.RegExpExec(rx, s)
	}
	return r.callExec(rx, exec, s)
}

// callExec calls a user exec method and checks its result.
func (r *Realm) callExec(rx *Object, exec Value, s *String) (Value, error) {
	res, err := r.Call(exec, ObjectValue(rx), []Value{StringValue(s)})
	if err != nil {
		return Undefined(), err
	}
	if !res.IsObject() && !res.IsNull() {
		return Undefined(), r.TypeError("RegExp exec method returned something other than an Object or null")
	}
	return res, nil
}

// regexpFlagsString implements ToString(Get(rx, "flags")).
func (r *Realm) regexpFlagsString(rx *Object) (*String, error) {
	v, err := rx.GetProp(r, flagsKey)
	if err != nil {
		return nil, err
	}
	return r.ToString(v)
}

// hasFlag reports whether the flags string contains c.
func hasFlag(flags *String, c byte) bool {
	for i := range flags.Len() {
		if flags.At(i) == uint16(c) {
			return true
		}
	}
	return false
}

// advanceStringIndex implements AdvanceStringIndex(s, index, unicode).
func advanceStringIndex(s *String, index int64, unicode bool) int64 {
	if !unicode || index+1 >= int64(s.Len()) {
		return index + 1
	}
	if isHighSurrogate(rune(s.At(int(index)))) && isLowSurrogate(rune(s.At(int(index)+1))) {
		return index + 2
	}
	return index + 1
}

// matchString implements ToString(Get(result, "0")).
func (r *Realm) matchString(result Value) (*String, error) {
	v, err := result.AsObject().GetProp(r, zeroKey)
	if err != nil {
		return nil, err
	}
	return r.ToString(v)
}

// advanceAfterEmptyMatch performs the lastIndex step the global loops take
// after an empty match: Set(rx, "lastIndex", AdvanceStringIndex(S,
// ToLength(Get(rx, "lastIndex")), fullUnicode)).
func (r *Realm) advanceAfterEmptyMatch(rx *Object, s *String, fullUnicode bool) error {
	thisIndex, err := r.regexpLastIndex(rx)
	if err != nil {
		return err
	}
	return rx.SetProp(r, lastIndexKey, Int64Value(advanceStringIndex(s, thisIndex, fullUnicode)))
}

// regexpCreateFrom implements RegExpCreate(pattern, flags) for the String
// methods' fallbacks.
func (r *Realm) regexpCreateFrom(pattern Value, flags *String) (*Object, error) {
	p := AtomEmpty
	if !pattern.IsUndefined() {
		var err error
		if p, err = r.ToString(pattern); err != nil {
			return nil, err
		}
	}
	return r.regexpFromStrings(r.RegExpPrototype, p, flags)
}

// regexpSymbolMethod performs GetMethod(o, key) for the String.prototype
// dispatch of guard g: builtin reports that the method is the original
// RegExp.prototype one (found without a lookup on a pristine RegExp), which
// the caller runs directly.
func (r *Realm) regexpSymbolMethod(o *Object, g int) (m Value, builtin bool, err error) {
	if r.pristineRegExp(o, 1<<g) != nil {
		return Undefined(), true, nil
	}
	if m, err = r.GetMethod(ObjectValue(o), regexpGuardKeys[g]); err != nil {
		return Undefined(), false, err
	}
	return m, !m.IsUndefined() && m == r.regexpGuard.want[g], nil
}

// regexpRequireGlobal is the flags check String.prototype.matchAll and
// replaceAll apply to an IsRegExp argument.
func (r *Realm) regexpRequireGlobal(o *Object, method string) error {
	if d := r.pristineRegExp(o, rxFlags); d != nil {
		if d.c.flags.global {
			return nil
		}
	} else {
		fv, err := o.GetProp(r, flagsKey)
		if err != nil {
			return err
		}
		if fv.IsNullish() {
			return r.TypeError("String.prototype.%s called with a RegExp whose flags are null or undefined", method)
		}
		flags, err := r.ToString(fv)
		if err != nil {
			return err
		}
		if hasFlag(flags, 'g') {
			return nil
		}
	}
	return r.TypeError("%s must be called with a global RegExp", method)
}

// thisRegExpObject validates the receiver of a RegExp.prototype[@@...] method.
func thisRegExpObject(r *Realm, this Value, method string) (*Object, error) {
	if !this.IsObject() {
		return nil, r.TypeError("RegExp.prototype[%s] called on non-object %s", method, r.DisplayString(this))
	}
	return this.AsObject(), nil
}

// --- RegExp.prototype[@@match] --------------------------------------------------------

func regexpProtoSymMatch(r *Realm, this Value, args []Value) (Value, error) {
	rx, err := thisRegExpObject(r, this, "Symbol.match")
	if err != nil {
		return Undefined(), err
	}
	return regexpSymMatch(r, rx, Arg(args, 0))
}

// regexpSymMatch implements RegExp.prototype[@@match] with rx as the receiver.
func regexpSymMatch(r *Realm, rx *Object, str Value) (Value, error) {
	s, err := r.ToString(str)
	if err != nil {
		return Undefined(), err
	}
	if d := r.pristineRegExp(rx, rxExec|rxFlags); d != nil {
		return regexpMatch(r, rx, d, s)
	}
	flags, err := r.regexpFlagsString(rx)
	if err != nil {
		return Undefined(), err
	}
	if !hasFlag(flags, 'g') {
		return r.regexpExec(rx, s)
	}
	fullUnicode := hasFlag(flags, 'u') || hasFlag(flags, 'v')
	if err := rx.SetProp(r, lastIndexKey, IntValue(0)); err != nil {
		return Undefined(), err
	}
	var items []Value
	for {
		if err := interruptEvery(r, int64(len(items))); err != nil {
			return Undefined(), err
		}
		result, err := r.regexpExec(rx, s)
		if err != nil {
			return Undefined(), err
		}
		if result.IsNull() {
			break
		}
		matchStr, err := r.matchString(result)
		if err != nil {
			return Undefined(), err
		}
		items = append(items, StringValue(matchStr))
		if matchStr.Len() == 0 {
			if err := r.advanceAfterEmptyMatch(rx, s, fullUnicode); err != nil {
				return Undefined(), err
			}
		}
	}
	if items == nil {
		return Null(), nil
	}
	return ObjectValue(r.NewArrayFromSlice(items)), nil
}

// --- RegExp.prototype[@@replace] ------------------------------------------------------

func regexpProtoSymReplace(r *Realm, this Value, args []Value) (Value, error) {
	rx, err := thisRegExpObject(r, this, "Symbol.replace")
	if err != nil {
		return Undefined(), err
	}
	return regexpSymReplace(r, rx, Arg(args, 0), Arg(args, 1))
}

// regexpSymReplace implements RegExp.prototype[@@replace] with rx as the
// receiver.
func regexpSymReplace(r *Realm, rx *Object, str, replaceValue Value) (Value, error) {
	s, err := r.ToString(str)
	if err != nil {
		return Undefined(), err
	}
	functional := IsCallable(replaceValue)
	var tmpl *String
	if !functional {
		if tmpl, err = r.ToString(replaceValue); err != nil {
			return Undefined(), err
		}
		replaceValue = StringValue(tmpl)
	}
	if d := r.pristineRegExp(rx, rxExec|rxFlags); d != nil {
		return regexpReplace(r, rx, d, s, replaceValue)
	}
	return regexpSymReplaceGeneric(r, rx, s, replaceValue, functional, tmpl)
}

// regexpSymReplaceGeneric is regexpSymReplace through the receiver's exec
// and flags. The exec results and the builder are held only in Go while
// exec and the replacer run: a charge made there waits for the return.
func regexpSymReplaceGeneric(r *Realm, rx *Object, s *String, replaceValue Value, functional bool, tmpl *String) (Value, error) {
	defer r.holdingEnd(r.holdingBegin())
	flags, err := r.regexpFlagsString(rx)
	if err != nil {
		return Undefined(), err
	}
	global := hasFlag(flags, 'g')
	fullUnicode := false
	if global {
		fullUnicode = hasFlag(flags, 'u') || hasFlag(flags, 'v')
		if err := rx.SetProp(r, lastIndexKey, IntValue(0)); err != nil {
			return Undefined(), err
		}
	}
	var results []*Object
	for {
		if err := interruptEvery(r, int64(len(results))); err != nil {
			return Undefined(), err
		}
		result, err := r.regexpExec(rx, s)
		if err != nil {
			return Undefined(), err
		}
		if result.IsNull() {
			break
		}
		results = append(results, result.AsObject())
		if !global {
			break
		}
		matchStr, err := r.matchString(result)
		if err != nil {
			return Undefined(), err
		}
		if matchStr.Len() == 0 {
			if err := r.advanceAfterEmptyMatch(rx, s, fullUnicode); err != nil {
				return Undefined(), err
			}
		}
	}
	lengthS := s.Len()
	var sb, scratch StringBuilder
	next := 0
	var captures []Value
	for i, result := range results {
		if err := interruptEvery(r, int64(i)); err != nil {
			return Undefined(), err
		}
		resultLength, err := r.LengthOfArrayLike(result)
		if err != nil {
			return Undefined(), err
		}
		matched, err := r.matchString(ObjectValue(result))
		if err != nil {
			return Undefined(), err
		}
		pv, err := result.GetProp(r, resultIndexKey)
		if err != nil {
			return Undefined(), err
		}
		pf, err := r.ToIntegerOrInfinity(pv)
		if err != nil {
			return Undefined(), err
		}
		position := int(max(0, min(pf, float64(lengthS))))
		captures = captures[:0]
		for n := int64(1); n < resultLength; n++ {
			if err := interruptEvery(r, n); err != nil {
				return Undefined(), err
			}
			c, err := result.GetProp(r, indexKey(r, n))
			if err != nil {
				return Undefined(), err
			}
			if !c.IsUndefined() {
				cs, err := r.ToString(c)
				if err != nil {
					return Undefined(), err
				}
				c = StringValue(cs)
			}
			captures = append(captures, c)
		}
		namedCaptures, err := result.GetProp(r, groupsKey)
		if err != nil {
			return Undefined(), err
		}
		out := &sb
		if position < next {
			scratch = StringBuilder{}
			out = &scratch // computed for its side effects, then dropped
		} else {
			writeSlice(&sb, s, next, position)
		}
		if functional {
			args := make([]Value, 0, len(captures)+4)
			args = append(args, StringValue(matched))
			args = append(args, captures...)
			args = append(args, IntValue(position), StringValue(s))
			if !namedCaptures.IsUndefined() {
				args = append(args, namedCaptures)
			}
			rv, err := r.Call(replaceValue, Undefined(), args)
			if err != nil {
				return Undefined(), err
			}
			rs, err := r.ToString(rv)
			if err != nil {
				return Undefined(), err
			}
			out.WriteString(rs)
		} else {
			if !namedCaptures.IsUndefined() {
				o, err := r.ToObject(namedCaptures)
				if err != nil {
					return Undefined(), err
				}
				namedCaptures = ObjectValue(o)
			}
			if err := r.appendSubstitution(out, s, position, position+matched.Len(), matched, captures, namedCaptures, tmpl); err != nil {
				return Undefined(), err
			}
		}
		if err := sb.checkLength(r); err != nil {
			return Undefined(), err
		}
		if position >= next {
			next = position + matched.Len()
		}
	}
	if next < lengthS {
		writeSlice(&sb, s, next, lengthS)
	}
	if err := sb.checkLength(r); err != nil {
		return Undefined(), err
	}
	return StringValue(r.builtString(&sb)), nil
}

// --- RegExp.prototype[@@search] -------------------------------------------------------

func regexpProtoSymSearch(r *Realm, this Value, args []Value) (Value, error) {
	rx, err := thisRegExpObject(r, this, "Symbol.search")
	if err != nil {
		return Undefined(), err
	}
	return regexpSymSearch(r, rx, Arg(args, 0))
}

// regexpSymSearch implements RegExp.prototype[@@search] with rx as the
// receiver. On a pristine RegExp the lastIndex save/restore around the
// exec is unobservable: the search runs from 0 (at 0 when sticky) and
// leaves lastIndex as it was.
func regexpSymSearch(r *Realm, rx *Object, str Value) (Value, error) {
	s, err := r.ToString(str)
	if err != nil {
		return Undefined(), err
	}
	if d := r.pristineRegExp(rx, rxExec); d != nil {
		var sub reSubject
		r.initRegExpSubject(&sub, s, d.c)
		m, err := r.regexpMatchFrom(d.c, s, &sub, 0, d.c.flags.sticky)
		if err != nil {
			return Undefined(), err
		}
		if m == nil {
			return IntValue(-1), nil
		}
		r.noteMatch(d, s, d.c, 0)
		return IntValue(m[0]), nil
	}
	previous, err := rx.GetProp(r, lastIndexKey)
	if err != nil {
		return Undefined(), err
	}
	if !SameValue(previous, IntValue(0)) {
		if err := rx.SetProp(r, lastIndexKey, IntValue(0)); err != nil {
			return Undefined(), err
		}
	}
	result, err := r.regexpExec(rx, s)
	if err != nil {
		return Undefined(), err
	}
	current, err := rx.GetProp(r, lastIndexKey)
	if err != nil {
		return Undefined(), err
	}
	if !SameValue(current, previous) {
		if err := rx.SetProp(r, lastIndexKey, previous); err != nil {
			return Undefined(), err
		}
	}
	if result.IsNull() {
		return IntValue(-1), nil
	}
	return result.AsObject().GetProp(r, resultIndexKey)
}

// --- RegExp.prototype[@@split] --------------------------------------------------------

func regexpProtoSymSplit(r *Realm, this Value, args []Value) (Value, error) {
	rx, err := thisRegExpObject(r, this, "Symbol.split")
	if err != nil {
		return Undefined(), err
	}
	return regexpSymSplit(r, rx, Arg(args, 0), Arg(args, 1))
}

// regexpSymSplit implements RegExp.prototype[@@split] with rx as the
// receiver. On a pristine RegExp the species lookup, the flags read and the
// sticky splitter the spec constructs are unobservable (the splitter is a
// fresh RegExp no script can reach), so regexpSplit runs on rx's own
// program once the limit is coerced.
func regexpSymSplit(r *Realm, rx *Object, str, limit Value) (Value, error) {
	s, err := r.ToString(str)
	if err != nil {
		return Undefined(), err
	}
	if limit.IsUndefined() || limit.IsNumber() {
		if d := r.pristineRegExp(rx, rxExec|rxFlags|rxMatch|rxSpecies); d != nil {
			lim := uint32(0xFFFFFFFF)
			if !limit.IsUndefined() {
				if lim, err = r.ToUint32(limit); err != nil {
					return Undefined(), err
				}
			}
			return regexpSplit(r, d, s, lim)
		}
	}
	c, err := r.speciesConstructor(rx, r.RegExpCtor)
	if err != nil {
		return Undefined(), err
	}
	flags, err := r.regexpFlagsString(rx)
	if err != nil {
		return Undefined(), err
	}
	unicodeMatching := hasFlag(flags, 'u') || hasFlag(flags, 'v')
	newFlags := flags
	if !hasFlag(flags, 'y') {
		var sb StringBuilder
		sb.WriteString(flags)
		sb.WriteASCII('y')
		newFlags = sb.String()
	}
	sv, err := r.Construct(ObjectValue(c), []Value{ObjectValue(rx), StringValue(newFlags)}, nil)
	if err != nil {
		return Undefined(), err
	}
	splitter := sv.AsObject()
	lim := uint32(0xFFFFFFFF)
	if !limit.IsUndefined() {
		if lim, err = r.ToUint32(limit); err != nil {
			return Undefined(), err
		}
	}
	if lim == 0 {
		return ObjectValue(r.NewArrayLen(0)), nil
	}
	size := int64(s.Len())
	if size == 0 {
		z, err := r.regexpExec(splitter, s)
		if err != nil {
			return Undefined(), err
		}
		if !z.IsNull() {
			return ObjectValue(r.NewArrayLen(0)), nil
		}
		return ObjectValue(r.NewArray(StringValue(s))), nil
	}
	var items []Value
	var p, q int64
	for steps := int64(0); q < size; steps++ {
		if err := interruptEvery(r, steps); err != nil {
			return Undefined(), err
		}
		if err := splitter.SetProp(r, lastIndexKey, Int64Value(q)); err != nil {
			return Undefined(), err
		}
		z, err := r.regexpExec(splitter, s)
		if err != nil {
			return Undefined(), err
		}
		if z.IsNull() {
			q = advanceStringIndex(s, q, unicodeMatching)
			continue
		}
		e, err := r.regexpLastIndex(splitter)
		if err != nil {
			return Undefined(), err
		}
		e = min(e, size)
		if e == p {
			q = advanceStringIndex(s, q, unicodeMatching)
			continue
		}
		items = append(items, StringValue(s.Substring(int(p), int(q))))
		if uint32(len(items)) == lim {
			return ObjectValue(r.NewArrayFromSlice(items)), nil
		}
		p = e
		zo := z.AsObject()
		n, err := r.LengthOfArrayLike(zo)
		if err != nil {
			return Undefined(), err
		}
		for i := int64(1); i < n; i++ {
			if err := interruptEvery(r, i); err != nil {
				return Undefined(), err
			}
			c, err := zo.GetProp(r, indexKey(r, i))
			if err != nil {
				return Undefined(), err
			}
			items = append(items, c)
			if uint32(len(items)) == lim {
				return ObjectValue(r.NewArrayFromSlice(items)), nil
			}
		}
		q = p
	}
	items = append(items, StringValue(s.Substring(int(p), int(size))))
	return ObjectValue(r.NewArrayFromSlice(items)), nil
}

// --- RegExp.prototype[@@matchAll] and %RegExpStringIteratorPrototype% ------------------

var AtomRegExpStringIterator = staticAtom("RegExp String Iterator")

func regexpProtoSymMatchAll(r *Realm, this Value, args []Value) (Value, error) {
	rx, err := thisRegExpObject(r, this, "Symbol.matchAll")
	if err != nil {
		return Undefined(), err
	}
	return regexpSymMatchAll(r, rx, Arg(args, 0))
}

// regexpSymMatchAll implements RegExp.prototype[@@matchAll] with rx as the
// receiver. A pristine RegExp is cloned directly (the spec's species
// constructor and flags read are unobservable).
func regexpSymMatchAll(r *Realm, rx *Object, str Value) (Value, error) {
	s, err := r.ToString(str)
	if err != nil {
		return Undefined(), err
	}
	var matcher *Object
	var global, fullUnicode bool
	if d := r.pristineRegExp(rx, rxFlags|rxMatch|rxSpecies); d != nil {
		if matcher, err = r.regexpCreate(r.RegExpPrototype, d.source, d.flags, d.c.flags); err != nil {
			return Undefined(), err
		}
		global, fullUnicode = d.c.flags.global, d.c.flags.unicode || d.c.flags.unicodeSets
	} else {
		c, err := r.speciesConstructor(rx, r.RegExpCtor)
		if err != nil {
			return Undefined(), err
		}
		flags, err := r.regexpFlagsString(rx)
		if err != nil {
			return Undefined(), err
		}
		mv, err := r.Construct(ObjectValue(c), []Value{ObjectValue(rx), StringValue(flags)}, nil)
		if err != nil {
			return Undefined(), err
		}
		matcher = mv.AsObject()
		global, fullUnicode = hasFlag(flags, 'g'), hasFlag(flags, 'u') || hasFlag(flags, 'v')
	}
	lastIndex, err := r.regexpLastIndex(rx)
	if err != nil {
		return Undefined(), err
	}
	if err := matcher.SetProp(r, lastIndexKey, Int64Value(lastIndex)); err != nil {
		return Undefined(), err
	}
	return ObjectValue(r.newRegExpStringIterator(matcher, s, global, fullUnicode)), nil
}

// regexpStringIter is the state of a RegExp String Iterator (the closure
// of CreateRegExpStringIterator); running rejects re-entrant next calls as
// a generator does.
type regexpStringIter struct {
	matcher                            *Object
	s                                  *String
	global, fullUnicode, done, running bool
}

type regexpStringIterObject struct {
	obj  Object
	data regexpStringIter
}

func (r *Realm) newRegExpStringIterator(matcher *Object, s *String, global, fullUnicode bool) *Object {
	it := &regexpStringIterObject{data: regexpStringIter{matcher: matcher, s: s, global: global, fullUnicode: fullUnicode}}
	r.chargeObject(unsafe.Sizeof(regexpStringIterObject{}))
	o := &it.obj
	proto := r.RegExpStringIteratorPrototype
	o.shape = r.rootShapeFor(proto)
	o.class = ClassRegExpStringIterator
	o.flags = flagExtensible
	setData(o, kindRegExpStringIter, &it.data)
	return o
}

// regexpStringIteratorNext implements %RegExpStringIteratorPrototype%.next.
func regexpStringIteratorNext(r *Realm, this Value, args []Value) (Value, error) {
	var it *regexpStringIter
	if this.IsObject() && this.AsObject().class == ClassRegExpStringIterator {
		it = dataOf(this.AsObject(), kindRegExpStringIter)
	}
	if it == nil {
		return Undefined(), r.TypeError("%%RegExpStringIteratorPrototype%%.next called on incompatible receiver %s", r.DisplayString(this))
	}
	if it.running {
		return Undefined(), r.TypeError("RegExp String Iterator is already running")
	}
	if it.done {
		return r.createIterResult(Undefined(), true), nil
	}
	it.running = true
	match, err := it.step(r)
	it.running = false
	if err != nil || match.IsNull() {
		it.finish()
		if err != nil {
			return Undefined(), err
		}
		return r.createIterResult(Undefined(), true), nil
	}
	if !it.global {
		it.finish()
	}
	return r.createIterResult(match, false), nil
}

// step runs one iteration of the closure: the next match, or null at the end.
func (it *regexpStringIter) step(r *Realm) (Value, error) {
	match, err := r.regexpExec(it.matcher, it.s)
	if err != nil || match.IsNull() || !it.global {
		return match, err
	}
	matchStr, err := r.matchString(match)
	if err != nil {
		return Undefined(), err
	}
	if matchStr.Len() == 0 {
		if err := r.advanceAfterEmptyMatch(it.matcher, it.s, it.fullUnicode); err != nil {
			return Undefined(), err
		}
	}
	return match, nil
}

// finish completes the iterator and releases the matcher and subject.
func (it *regexpStringIter) finish() {
	it.done = true
	it.matcher, it.s = nil, nil
}

// regexpSymbolMethods are the RegExp.prototype[@@...] methods.
var regexpSymbolMethods = []symbolMethodDef{
	{SymMatch, atomMatchFn, regexpProtoSymMatch, 1},
	{SymMatchAll, atomMatchAllFn, regexpProtoSymMatchAll, 1},
	{SymReplace, atomReplaceFn, regexpProtoSymReplace, 2},
	{SymSearch, atomSearchFn, regexpProtoSymSearch, 1},
	{SymSplit, atomSplitFn, regexpProtoSymSplit, 2},
}

// installRegExpProtocol defines the RegExp.prototype[@@...] methods and
// %RegExpStringIteratorPrototype%, and records the originals regexpGuard
// compares against (after every watched property exists).
func installRegExpProtocol(r *Realm) {
	p := r.RegExpPrototype
	r.installSymbolMethods(p, regexpSymbolMethods, attrHidden)

	r.RegExpStringIteratorPrototype = r.newIntrinsic(ClassObject, r.IteratorPrototype, r.deferredCap(2, 0))
	r.installDeferred(r.RegExpStringIteratorPrototype, installRegExpStringIteratorPrototype)

	g := &r.regexpGuard
	for i, key := range regexpGuardKeys {
		g.want[i] = bootstrapOwnValue(p, key)
	}
}

func installRegExpStringIteratorPrototype(r *Realm, p *Object) {
	p.ReserveSlots(r, 2)
	r.regexpStringIterNextFn = r.NewNativeFunction(AtomNext, 0, regexpStringIteratorNext)
	r.installOrReplace(p, nextKey, propCell{value: ObjectValue(r.regexpStringIterNextFn), attrs: attrHidden})
	r.installToStringTag(p, AtomRegExpStringIterator)
}
