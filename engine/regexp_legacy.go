package engine

// The legacy RegExp features (ES2025 B.2.4 and the legacy RegExp features
// proposal): RegExp.prototype.compile, with the proposal's realm and
// [[LegacyFeaturesEnabled]] checks, and the static accessors of %RegExp%
// (RegExp.$1-$9, input, lastMatch, lastParen, leftContext, rightContext
// and their aliases).
//
// The statics are paid for as they are read. A successful match stores
// only its subject, its program and where it started in the realm
// (noteMatch: two pointers and an int32 in regexpState, no allocation);
// the first read after it matches again from there, and keeps the capture
// indices for the reads that follow (regexpStatics).

var compileKey = StringKey(AtomCompile)

// regexpProtoCompile reinitializes a RegExp in place: RegExpInitialize with
// the source and flags of a RegExp pattern, or pattern and flags coerced
// to strings. Only a RegExp whose legacy is the calling realm qualifies: r
// is the realm of the compile function called (a native runs with the
// realm that calls it, and moejs has no cross-realm functions), so a RegExp
// of another realm, which a host may hand over, or a subclass instance
// throws a TypeError, and so does a shared object, which no program's realm
// created. The payload is replaced before lastIndex is set to 0, which
// throws when lastIndex is not writable, as the spec's Set does after
// RegExpInitialize has recompiled. The paths that hold the payload while
// user code runs (execLastIndex, regexpReplace, simpleReplace) read the
// program again or keep the one they started with, as the spec's.
func regexpProtoCompile(r *Realm, this Value, args []Value) (Value, error) {
	o, d, err := thisRegExp(r, this, "compile")
	if err != nil {
		return Undefined(), err
	}
	if d.legacy != r {
		return Undefined(), r.TypeError("RegExp.prototype.compile requires a RegExp created by this realm's RegExp constructor")
	}
	pattern, flags := Arg(args, 0), Arg(args, 1)
	ps, fs := AtomEmpty, AtomEmpty
	if _, pd, ok := isRegExpObject(pattern); ok {
		if !flags.IsUndefined() {
			return Undefined(), r.TypeError("Cannot supply flags when constructing one RegExp from another")
		}
		ps, fs = pd.source, pd.flags
	} else {
		if !pattern.IsUndefined() {
			if ps, err = r.ToString(pattern); err != nil {
				return Undefined(), err
			}
		}
		if !flags.IsUndefined() {
			if fs, err = r.ToString(flags); err != nil {
				return Undefined(), err
			}
		}
	}
	f, ascii := fs.ASCII()
	fl, valid := parseRegExpFlags(f)
	if !ascii || !valid {
		return Undefined(), r.SyntaxError("Invalid flags supplied to RegExp constructor '%s'", fs.GoString())
	}
	c, err := r.compileRegExp(ps, fl)
	if err != nil {
		return Undefined(), err
	}
	if canonical := fl.String(); canonical != f {
		fs = asciiString(canonical)
	}
	d.source, d.flags, d.c = ps, fs, c
	if err := r.setRegExpLastIndex(o, 0); err != nil {
		return Undefined(), err
	}
	return this, nil
}

// installRegExpCompile defines RegExp.prototype.compile, last: at once while
// building the shared template, on first use in a mutable realm
// (pendingCompile), where it would cost every realm a function, a shape
// and a transition few programs use.
func installRegExpCompile(r *Realm) {
	p := r.RegExpPrototype
	if r.buildingShared {
		p.addNamed(r, compileKey, compileCell(r))
		return
	}
	p.mustBeMutable()
	setExtData(p, kindPendingCompile, (*pendingCompile)(r))
	p.flags |= flagHasLazy
	if p.flags&flagDict == 0 && !p.shape.shared {
		p.shape.noFill = noFillAll
	}
}

func compileCell(r *Realm) propCell {
	return propCell{value: ObjectValue(r.NewNativeFunction(AtomCompile, 2, regexpProtoCompile)), attrs: attrHidden}
}

// pendingCompile is the internal of %RegExp.prototype% in a mutable realm
// until compile is defined: by a lookup of compile, or before a write,
// redefinition or deletion of any key and before the key list, after the
// properties the prototype has then, where the shared template has it
// (installRegExpCompile runs last). A lookup of any other key finds the
// prototype as it is, so the pristine RegExp paths (regexpGuard) and the
// inline caches of its methods work as without it; the prototype's shape
// is noFill meanwhile, as a deferInstall object's is, so no cache filled
// for compile on another object of that shape hits it.
type pendingCompile Realm

// define defines compile on p, whose internal is pc.
func (pc *pendingCompile) define(p *Object) {
	r := (*Realm)(pc)
	p.clearPayload()
	p.flags &^= flagHasLazy
	p.addNamed(r, compileKey, compileCell(r))
}

// onlyLegacyPending reports whether o, which has flagDict or flagHasLazy,
// is a shape-mode object whose only pending properties are legacy RegExp
// ones: compile on %RegExp.prototype% (pendingCompile) or the statics on
// %RegExp% (installRegExpStatics). A lookup of any other key may read its
// shape.
func (o *Object) onlyLegacyPending() bool {
	switch o.payload().(type) {
	case *pendingCompile, *FunctionData:
		return o.flags&flagDict == 0
	}
	return false
}

// --- the static accessors ------------------------------------------------------

// The slots of the statics, which the accessor pairs (name and alias) read.
const (
	staticInput = iota
	staticLastMatch
	staticLastParen
	staticLeftContext
	staticRightContext
	staticParen1 // $1; $2-$9 follow
)

// staticDef is one accessor property of the statics: its key, the slot
// its getter reads and its functions' names (a setter for input and $_
// only).
type staticDef struct {
	key      PropertyKey
	slot     int
	get, set *String
}

// regexpStaticDefs lists the statics in the proposal's order.
var regexpStaticDefs = staticDefs()

func staticDefs() []staticDef {
	defs := make([]staticDef, 0, 19)
	add := func(name string, slot int, setter bool) {
		def := staticDef{key: StringKey(staticAtom(name)), slot: slot, get: staticAtom("get " + name)}
		if setter {
			def.set = staticAtom("set " + name)
		}
		defs = append(defs, def)
	}
	for _, p := range [...]struct {
		name, alias string
		slot        int
	}{
		{"input", "$_", staticInput},
		{"lastMatch", "$&", staticLastMatch},
		{"lastParen", "$+", staticLastParen},
		{"leftContext", "$`", staticLeftContext},
		{"rightContext", "$'", staticRightContext},
	} {
		add(p.name, p.slot, p.slot == staticInput)
		add(p.alias, p.slot, p.slot == staticInput)
	}
	for i := range 9 {
		add("$"+string(rune('1'+i)), staticParen1+i, false)
	}
	return defs
}

// staticGetters are the getters' native functions, by slot (filled by
// init: a variable's initializer may not reach the lazy definition the
// getters' errors can trigger).
var staticGetters [staticParen1 + 9]NativeFunc

func init() {
	for slot := range staticGetters {
		staticGetters[slot] = func(r *Realm, this Value, args []Value) (Value, error) {
			return regexpStaticGet(r, this, slot)
		}
	}
}

// isStaticsKey reports whether key names one of the statics.
func isStaticsKey(key PropertyKey) bool {
	for i := range regexpStaticDefs {
		if regexpStaticDefs[i].key == key {
			return true
		}
	}
	return false
}

// refuseFill reports whether fillGetIC must not fill an entry for key on
// s, a noFill shape: any key for noFillAll; for noFillStatics, a static
// while s is the shape %RegExp% shares with the Set and SharedArrayBuffer
// constructors and the statics are pending. It is kept out of line, so
// that the fill on any other shape costs what it did.
//
//go:noinline
func (r *Realm) refuseFill(s *Shape, key PropertyKey) bool {
	if s.noFill&noFillAll != 0 {
		return true
	}
	rc := r.RegExpCtor
	return s == rc.shape && rc.flags&flagHasLazy != 0 && isStaticsKey(key)
}

// installRegExpStatics defines the statics on %RegExp%, after @@species:
// at once while building the shared template; on first use in a mutable
// realm, where 19 accessors (21 functions and 19 transitions) would cost
// every realm for properties few programs read. Until then %RegExp% keeps
// its *FunctionData internal with flagHasLazy: a lookup of a static, or a
// write, redefinition or deletion of any key, the key list, a prototype
// change or preventing extensions defines them all, where the template
// has them (resolveLazy and the rest). A lookup of any other key finds
// %RegExp% as it is, and the realm's matches record their statics as
// they do with the accessors defined. The constructor's shape is the Set
// and SharedArrayBuffer constructors' too, and noFillStatics meanwhile:
// fillGetIC fills no entry for a static on it (refuseFill), so none
// filled on one of them hits %RegExp% with the statics pending. Their
// other properties are cached as before, and the statics too once
// defined.
func installRegExpStatics(r *Realm) {
	o := r.RegExpCtor
	if r.buildingShared {
		defineRegExpStatics(r, o)
		return
	}
	o.mustBeMutable()
	o.flags |= flagHasLazy
	if o.flags&flagDict == 0 && !o.shape.shared {
		o.shape.noFill |= noFillStatics
	}
}

// definePendingStatics defines the statics on o, a mutable realm's
// %RegExp% with them pending.
func definePendingStatics(o *Object, fd *FunctionData) {
	o.flags &^= flagHasLazy
	defineRegExpStatics(fd.realm, o)
}

func defineRegExpStatics(r *Realm, o *Object) {
	o.ReserveSlots(r, len(regexpStaticDefs))
	accs := make([]Accessor, len(regexpStaticDefs)) // one allocation for the table
	for i := range regexpStaticDefs {
		def := &regexpStaticDefs[i]
		accs[i].Get = r.NewNativeFunction(def.get, 0, staticGetters[def.slot])
		if def.set != nil {
			accs[i].Set = r.NewNativeFunction(def.set, 1, regexpStaticSetInput)
		}
		o.addNamed(r, def.key, propCell{value: accessorValue(&accs[i]), attrs: attrConfigurable | attrAccessor})
	}
}

// --- the record ----------------------------------------------------------------

// The record of the last successful match is regexpState's lastS, lastC
// and lastAt: the subject, the program that matched and where to match
// again to find the same match, which is
//
//   - lastAt >= 0: a RegExpBuiltinExec from code unit lastAt (its
//     lastIndex: 0 unless global or sticky), as regexpMatchFrom;
//   - matchedLast: the last match of the global loop (findAll);
//   - matchedAt(u): a match a search found at code unit u, matched again
//     sticky at u (the last match of a global loop, a split piece).
//
// lastC is nil in a realm's initial state, where every static is "", or a
// sentinel: staticsInvalid after a match by a RegExp whose legacy is not
// the realm (a subclass instance, another realm's RegExp), where every
// static throws, or staticsInput after the input setter, which keeps its
// value in lastS and what the other statics read in regexpStatics. Any
// match replaces all of it, the setter's value included.
var staticsInvalid, staticsInput compiledRegExp

// matchedLast is the lastAt of the last match of a global loop.
const matchedLast int32 = -1

// matchedAt is the lastAt of a match a search found at code unit u.
func matchedAt(u int) int32 { return int32(-2 - u) }

// noteMatch records a successful match of c, d's program, on s, which the
// statics then describe (UpdateLegacyRegExpStaticProperties); at is where
// to match again (see staticsInvalid). A RegExp whose legacy is not the
// realm invalidates them (InvalidateLegacyRegExpStaticProperties). The
// proposal ignores a match by another realm's RegExp; here it
// invalidates, so a realm's statics never describe a match it cannot
// tell from its own. It is called after lastIndex is set, before any user
// code the operation runs afterwards (a replacer). It is inlined, at the
// inliner's budget: the realm check takes the cold call.
func (r *Realm) noteMatch(d *RegExpData, s *String, c *compiledRegExp, at int32) {
	if d.legacy == r {
		st := r.regexps // the realm created d, through compileRegExp
		st.lastS, st.lastC, st.lastAt = s, c, at
	} else {
		r.invalidateStatics()
	}
}

//go:noinline
func (r *Realm) invalidateStatics() {
	st := r.regexpState()
	st.lastS, st.lastC = nil, &staticsInvalid
}

// regexpStatics is the cold side of the statics (realmLazy.statics): the
// match of the record (s, c, at) with its capture indices m in code units,
// which the first read after a match computes; or, while lastC is
// staticsInput, the state the setter left the other statics in: c nil
// (initial), staticsInvalid, or a match with its m.
type regexpStatics struct {
	s  *String
	c  *compiledRegExp
	at int32
	m  []int
}

// staticsMatch returns the cold state for the record, a match or the
// setter's saved state, matching the record again if the cache describes
// another one.
func (r *Realm) staticsMatch() (*regexpStatics, error) {
	st, lz := r.regexps, r.lazyState()
	cold := lz.statics
	if st.lastC == &staticsInput || cold != nil && cold.s == st.lastS && cold.c == st.lastC && cold.at == st.lastAt {
		return cold, nil
	}
	s, c, at := st.lastS, st.lastC, st.lastAt
	var sub reSubject
	r.initRegExpSubject(&sub, s, c)
	var m []int
	var err error
	switch {
	case at >= 0:
		m, err = r.regexpMatchFrom(c, s, &sub, int(at), c.flags.sticky)
	case at == matchedLast:
		var all [][]int
		if all, err = c.findAll(r, &sub); len(all) != 0 {
			m = all[len(all)-1]
			sub.toUnits(m)
		}
	default:
		m, err = r.regexpMatchFrom(c, s, &sub, int(-2-at), true)
	}
	if err != nil {
		return nil, err
	}
	if cold == nil {
		cold = &regexpStatics{}
		lz.statics = cold
	}
	cold.s, cold.c, cold.at, cold.m = s, c, at, m
	return cold, nil
}

// regexpStaticGet implements GetLegacyRegExpStaticProperty for slot.
func regexpStaticGet(r *Realm, this Value, slot int) (Value, error) {
	if !this.IsObject() || this.AsObject() != r.RegExpCtor {
		return Undefined(), r.TypeError("RegExp legacy static getter called on %s, not the RegExp constructor", r.DisplayString(this))
	}
	st := r.regexps
	if st == nil {
		return StringValue(AtomEmpty), nil
	}
	c := st.lastC
	switch {
	case c == &staticsInput:
		if slot == staticInput {
			return StringValue(st.lastS), nil
		}
		c = r.lazy.statics.c
	case c == nil, c == &staticsInvalid:
	case slot == staticInput:
		return StringValue(st.lastS), nil
	}
	switch c {
	case nil:
		return StringValue(AtomEmpty), nil
	case &staticsInvalid:
		return Undefined(), r.TypeError("RegExp legacy static properties are unavailable after a match by a RegExp subclass instance or another realm's RegExp")
	}
	cold, err := r.staticsMatch()
	if err != nil {
		return Undefined(), err
	}
	return StringValue(staticValue(cold, slot)), nil
}

// staticValue returns the value of slot for cold's match.
func staticValue(cold *regexpStatics, slot int) *String {
	s, m := cold.s, cold.m
	if m == nil {
		return AtomEmpty // no match again: unreachable, as matching is deterministic
	}
	n := cold.c.ngroups()
	switch slot {
	case staticInput:
		return s
	case staticLastMatch:
		return s.Substring(m[0], m[1])
	case staticLastParen:
		return capturedValue(s, m, n)
	case staticLeftContext:
		return s.Substring(0, m[0])
	case staticRightContext:
		return s.Substring(m[1], s.Len())
	}
	if i := slot - staticParen1 + 1; i <= n {
		return capturedValue(s, m, i)
	}
	return AtomEmpty
}

// capturedValue returns capture i of m on s, "" when it did not
// participate or i is 0 (lastParen without groups).
func capturedValue(s *String, m []int, i int) *String {
	if i == 0 || m[2*i] < 0 {
		return AtomEmpty
	}
	return s.Substring(m[2*i], m[2*i+1])
}

// regexpStaticSetInput implements SetLegacyRegExpStaticProperty for
// [[RegExpInput]] (set input, set $_): the other statics keep what they
// read, which is saved in the cold state when it is a match.
func regexpStaticSetInput(r *Realm, this Value, args []Value) (Value, error) {
	if !this.IsObject() || this.AsObject() != r.RegExpCtor {
		return Undefined(), r.TypeError("RegExp legacy static setter called on %s, not the RegExp constructor", r.DisplayString(this))
	}
	v, err := r.ToString(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	st := r.regexpState()
	switch c := st.lastC; c {
	case &staticsInput:
	case nil, &staticsInvalid:
		r.lazyState().statics = &regexpStatics{c: c}
	default:
		if _, err := r.staticsMatch(); err != nil {
			return Undefined(), err
		}
	}
	st.lastS, st.lastC = v, &staticsInput
	return Undefined(), nil
}
