package engine

import (
	"errors"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

// RegExp builtins. A pattern is parsed once (regexpsyntax.Parse)
// and runs on one of two engines. When RE2 has JavaScript's semantics for it
// (reExact) it is translated by regexp_translate.go and executed by Go's
// regexp package: ASCII subjects run on their own bytes (byte index == code
// unit index); UTF-16 subjects are converted once to a UTF-8 working copy
// with byte<->unit index maps that is cached per realm. Every other pattern,
// and an exact one on the few subjects where RE2 would differ (useBT), runs
// on the backtracking VM of regexp_bt.go directly over the code units.
// Compiled patterns are shared by every realm of the process
// (regexpPrograms, keyed by pattern and flags) behind a small per-realm
// front cache.
//
// The Symbol.match/matchAll/replace/search/split protocol lives in
// regexp_protocol.go: its generic algorithms read exec, flags and lastIndex
// through [[Get]], and a pristine RegExp runs the hooks at the end of this
// file (regexpMatch, regexpSplit, regexpReplace) directly.

// RegExpData is the internal payload of ClassRegExp objects.
type RegExpData struct {
	source *String // [[OriginalSource]]
	flags  *String // [[OriginalFlags]] in canonical "dgimsuvy" order
	c      *compiledRegExp
	// legacy is [[Realm]] when [[LegacyFeaturesEnabled]] (the object was
	// created by %RegExp% itself or a literal, not a subclass), else nil:
	// RegExp.prototype.compile (regexp_legacy.go) accepts only an object
	// whose legacy is the calling realm.
	legacy *Realm
}

// Source returns the pattern text ([[OriginalSource]]).
func (d *RegExpData) Source() *String { return d.source }

// Flags returns the flags in canonical order ([[OriginalFlags]]).
func (d *RegExpData) Flags() *String { return d.flags }

// RegExpData returns the payload of a RegExp object, or nil.
func (o *Object) RegExpData() *RegExpData {
	if o.class != ClassRegExp {
		return nil
	}
	d, _ := o.internal.(*RegExpData)
	return d
}

// compiledRegExp is one cache entry: the RE2 program plus the lazily built
// variants used to start a search at a non-zero position with correct
// `^`/`\b` context (see matchAt), and the backtracking program.
type compiledRegExp struct {
	// tr is the translation; without an RE2 program only names, hasNames
	// and minLen0 are set.
	tr    *translatedRegExp
	flags regexpFlags
	key   regexpCacheKey
	// re is the RE2 program, nil when the pattern runs on the backtracking
	// VM only.
	re *regexp.Regexp
	// btCR is set when the pattern has an m-mode ^ or $, which RE2 does not
	// apply at \r, U+2028 or U+2029; btFoldWord when a u-mode \b or \B
	// ignores case, which makes U+017F and U+212A word characters. A subject
	// with those characters runs on the backtracking VM (useBT).
	btCR, btFoldWord bool
	// simple is the single-class matcher when the pattern is one character
	// class with a greedy quantifier (regexp_simple.go), else nil.
	simple *simpleClass
	// run matches an anchored run pattern on ASCII subjects
	// (string_regexp.go), else nil.
	run *runPattern
	// Lazily compiled variants. A compiledRegExp is shared by every realm of
	// the process (regexpPrograms), so the variants are published atomically;
	// two realms racing on the first use compile equal programs and either
	// one may win.
	anchRe    atomic.Pointer[regexp.Regexp] // \A(?:body)
	ctxRe     atomic.Pointer[regexp.Regexp] // (?s:.)(?:body)
	ctxAnchRe atomic.Pointer[regexp.Regexp] // \A(?s:.)(?:body)
	// bt is the backtracking program: compiled with the pattern when re is
	// nil, else on the first subject that needs it (btProgram).
	bt atomic.Pointer[btProg]
}

func (c *compiledRegExp) ngroups() int { return len(c.tr.names) - 1 }

func (c *compiledRegExp) anchored() (*regexp.Regexp, error) {
	return c.variant(&c.anchRe, `\A(?:`)
}

func (c *compiledRegExp) context() (*regexp.Regexp, error) {
	return c.variant(&c.ctxRe, `(?s:.)(?:`)
}

func (c *compiledRegExp) contextAnchored() (*regexp.Regexp, error) {
	return c.variant(&c.ctxAnchRe, variantOpenLargest)
}

// variantOpenLargest opens the largest search variant; a body that parses
// inside it parses inside the other two (they drop a leaf of the same
// concatenation), so compileRegExp validates only this one.
const variantOpenLargest = `\A(?s:.)(?:`

// variant returns the lazily compiled program open + body + ")".
// compileRegExp parsed the largest variant, so compilation cannot fail; a
// failure is an internal error, never a panic.
func (c *compiledRegExp) variant(slot *atomic.Pointer[regexp.Regexp], open string) (*regexp.Regexp, error) {
	if re := slot.Load(); re != nil {
		return re, nil
	}
	re, err := regexp.Compile(open + c.tr.body + `)`)
	if err != nil {
		return nil, errors.New("regexp search variant failed to compile: " + err.Error())
	}
	slot.Store(re)
	return re, nil
}

// btProgram returns the backtracking program, compiling it from the cache
// key on first use. The pattern parsed when the entry was built, so a
// failure is an internal error, never a panic.
func (c *compiledRegExp) btProgram() (*btProg, error) {
	if p := c.bt.Load(); p != nil {
		return p, nil
	}
	ast, err := parseRegExp(c.key.units(), c.flags)
	if err != nil {
		return nil, errors.New("regexp backtracking program failed to compile: " + err.Error())
	}
	p := compileBT(ast, c.flags)
	c.bt.Store(p)
	return p, nil
}

// useBT reports whether sub runs on the backtracking VM.
func (c *compiledRegExp) useBT(sub *reSubject) bool {
	return c.re == nil || sub.private || c.btCR && sub.crLS || c.btFoldWord && sub.foldWord
}

// regexpCacheKey identifies a compiled pattern. An ASCII pattern is its own
// text; any other pattern is regexpWideKey (a byte ASCII text never holds)
// followed by its code units as little-endian byte pairs, so lone surrogates
// stay distinct. The marker keeps the key at 24 bytes: every realm that
// evaluates a regexp allocates a map of them.
type regexpCacheKey struct {
	pattern string
	flags   regexpFlags
}

const regexpWideKey = 0xFF

// regexpKey returns the cache key of pattern with flags.
func regexpKey(pattern *String, flags regexpFlags) regexpCacheKey {
	if s, ok := pattern.ASCII(); ok {
		return regexpCacheKey{pattern: s, flags: flags}
	}
	u := pattern.UTF16()
	b := make([]byte, 1+2*len(u))
	b[0] = regexpWideKey
	for i, c := range u {
		b[1+2*i], b[2+2*i] = byte(c), byte(c>>8)
	}
	return regexpCacheKey{pattern: bytesToString(b), flags: flags}
}

// units returns the pattern's code units.
func (k regexpCacheKey) units() []uint16 {
	if len(k.pattern) == 0 || k.pattern[0] != regexpWideKey {
		u := make([]uint16, len(k.pattern))
		for i := range len(k.pattern) {
			u[i] = uint16(k.pattern[i])
		}
		return u
	}
	p := k.pattern[1:]
	u := make([]uint16, len(p)/2)
	for i := range u {
		u[i] = uint16(p[2*i]) | uint16(p[2*i+1])<<8
	}
	return u
}

// regexpCacheLimit bounds the per-realm compiled-pattern cache; when full the
// cache is cleared (plugin hooks use a handful of patterns).
const regexpCacheLimit = 256

// regexpPrograms is the process-wide cache of compiled patterns: the
// translation and the RE2 program of a pattern are immutable and
// *regexp.Regexp is safe for concurrent use, so every realm running the same
// plugin shares one copy instead of compiling and retaining its own (a
// realm's first evaluation of a plugin compiles each of its literals). The
// per-realm map stays as a lock-free front. Bounded by regexpProgramsLimit
// entries; beyond it patterns are compiled per realm as before.
var (
	regexpPrograms      sync.Map // regexpCacheKey -> *compiledRegExp
	regexpProgramCount  atomic.Int32
	regexpProgramsLimit = int32(4096)
)

// regexpState is the per-realm RegExp machinery (Realm.regexps).
type regexpState struct {
	cache     map[regexpCacheKey]*compiledRegExp
	shape     *Shape // RegExp instance shape: lastIndex
	execShape *Shape // exec result: index, input, groups
	subjects  [2]reSubject
	subjNext  int32
	// The last successful match, for the legacy statics (noteMatch,
	// regexp_legacy.go): subject, program and where to match again.
	lastAt int32
	lastS  *String
	lastC  *compiledRegExp
}

func (r *Realm) regexpState() *regexpState {
	if r.regexps == nil {
		r.regexps = &regexpState{cache: make(map[regexpCacheKey]*compiledRegExp, 16)}
	}
	return r.regexps
}

// compileRegExp translates and compiles pattern with flags, through the
// per-realm cache. Errors are JavaScript SyntaxErrors.
func (r *Realm) compileRegExp(pattern *String, flags regexpFlags) (*compiledRegExp, error) {
	st := r.regexpState()
	key := regexpKey(pattern, flags)
	if c, ok := st.cache[key]; ok {
		return c, nil
	}
	var c *compiledRegExp
	if shared, ok := regexpPrograms.Load(key); ok {
		c = shared.(*compiledRegExp)
	} else {
		var err error
		if c, err = compilePattern(key, pattern.UTF16(), flags); err != nil {
			return nil, r.regexpSyntaxError(pattern, flags, err)
		}
		if regexpProgramCount.Load() < regexpProgramsLimit {
			if prev, loaded := regexpPrograms.LoadOrStore(c.key, c); loaded {
				c = prev.(*compiledRegExp)
			} else {
				regexpProgramCount.Add(1)
			}
		}
	}
	if len(st.cache) >= regexpCacheLimit {
		clear(st.cache)
	}
	// The cache keeps compilePattern's copy of the key, as the pattern's
	// bytes may be a slice of a large string, or an atom's, whose bytes are
	// its own: a literal's pattern is one (interp.go), and its next lookup
	// then compares equal pointers.
	k := &c.key
	if pattern.atom != 0 {
		k = &key
	}
	st.cache[*k] = c
	return c, nil
}

// compilePattern parses a pattern and selects its engine: RE2 when reExact
// holds and RE2 accepts the translation, the backtracking VM otherwise.
// Errors are the parser's.
func compilePattern(key regexpCacheKey, units []uint16, flags regexpFlags) (*compiledRegExp, error) {
	ast, err := parseRegExp(units, flags)
	if err != nil {
		return nil, err
	}
	if len(key.pattern) == 0 || key.pattern[0] != regexpWideKey {
		// The caches keep c.key, the realm's unless the pattern is an atom
		// (compileRegExp): its own copy of an ASCII pattern, never the
		// pattern's bytes, which may be a slice of a large string
		// (Substring). A wide key is a copy already (regexpKey).
		key.pattern = strings.Clone(key.pattern)
	}
	c := &compiledRegExp{flags: flags, key: key, simple: compileSimpleClass(units, flags), run: compileRunPattern(ast, flags)}
	if reExact(ast.Root) {
		if tr, re := compileRE2(ast, flags, len(units)); re != nil {
			c.tr, c.re = tr, re
			ast.Root.Walk(func(n *reNode) {
				switch n.Op {
				case reOpBegin, reOpEnd:
					c.btCR = c.btCR || n.Multiline
				case reOpWordB, reOpNotWordB:
					c.btFoldWord = c.btFoldWord || n.Icase && flags.unicode
				}
			})
			return c, nil
		}
	}
	c.tr = &translatedRegExp{names: ast.Names, hasNames: ast.HasNames, dupNames: ast.DupNames, minLen0: ast.Root.CanBeEmpty()}
	c.bt.Store(compileBT(ast, flags))
	return c, nil
}

// compileRE2 translates an exact pattern and compiles it, or returns a nil
// program when RE2 rejects it (a size or nesting limit). The search variants
// (matchAt) wrap the body in a prefix and a group, and a body at RE2's limit
// compiles alone but not wrapped, so the largest variant is parsed first
// (every RE2 limit is enforced by the parser); such a pattern runs on the
// backtracking VM instead of failing on its first exec.
func compileRE2(ast *reAST, flags regexpFlags, sizeHint int) (*translatedRegExp, *regexp.Regexp) {
	tr, err := emitRE2(ast, flags, sizeHint)
	if err != nil {
		return nil, nil
	}
	if _, err := syntax.Parse(variantOpenLargest+tr.body+`)`, syntax.Perl); err != nil {
		return nil, nil
	}
	re, err := regexp.Compile(tr.body)
	if err != nil {
		return nil, nil
	}
	return tr, re
}

func (r *Realm) regexpSyntaxError(pattern *String, flags regexpFlags, err error) error {
	if u, ok := err.(*regexpUnsupportedError); ok {
		return r.SyntaxError("%s", u.Error())
	}
	return r.SyntaxError("Invalid regular expression: /%s/%s: %s", pattern.GoString(), flags.String(), err.Error())
}

// --- subjects ---------------------------------------------------------------

// reSubject is the matching view of a subject string. Positions ("bytes")
// index text; a UTF-16 subject of a pattern without an RE2 program has no
// working copy (units), and its positions are code units.
type reSubject struct {
	s       *String // as given: a rope stays one (Len and At read it)
	text    []byte  // ASCII bytes (aliasing the string) or a UTF-8 working copy
	ascii   bool
	unicode bool // u mode: working copy built with surrogate pairs combined
	units   bool // UTF-16 subject without a working copy
	// Characters for which RE2 differs from JavaScript (useBT): crLS is \r,
	// U+2028 or U+2029 (m-mode ^ and $); foldWord U+017F or U+212A (u-mode
	// \b and \B with i); private a u-mode character U+F0000..U+F07FF, which
	// the working copy cannot tell from a surrogate stand-in.
	crLS, foldWord, private bool
	b2u                     []int32 // UTF-16 subjects: byte offset -> unit offset, len(text)+1
	u2b                     []int32 // UTF-16 subjects: unit offset -> byte offset, len(units)+1
}

// len returns the subject length in positions.
func (sub *reSubject) len() int {
	if sub.units {
		return sub.s.Len()
	}
	return len(sub.text)
}

func (sub *reSubject) toByte(u int) int {
	if sub.ascii || sub.units {
		return u
	}
	return int(sub.u2b[u])
}

func (sub *reSubject) toUnit(b int) int {
	if sub.ascii || sub.units {
		return b
	}
	return int(sub.b2u[b])
}

// advance returns the position one character after b (AdvanceStringIndex).
func (sub *reSubject) advance(b int) int {
	if sub.ascii || b >= sub.len() {
		return b + 1
	}
	if sub.units {
		s := sub.s // At reads a rope's units too
		if sub.unicode && b+1 < s.Len() && isHighSurrogate(rune(s.At(b))) && isLowSurrogate(rune(s.At(b+1))) {
			return b + 2
		}
		return b + 1
	}
	_, w := utf8.DecodeRune(sub.text[b:])
	return b + w
}

// initRegExpSubject fills sub for s and the pattern c, reusing the realm's
// cached working copy for UTF-16 strings. In u mode a surrogate pair is one
// character (its code point); every other surrogate unit is encoded as its
// private-use stand-in rune (see regexp_translate.go).
func (r *Realm) initRegExpSubject(sub *reSubject, s *String, c *compiledRegExp) {
	// sub keeps s, a rope or lazy string as it came (the cache compares
	// pointers), and f is what is read.
	f := s
	if s.needsFlat() {
		f = s.flat(new(String))
	}
	unicode := c.flags.unicode
	if f.kind == strASCII {
		*sub = reSubject{s: s, text: asciiBytes(f.s), ascii: true, unicode: unicode}
		sub.crLS = c.btCR && strings.IndexByte(f.s, '\r') >= 0
		return
	}
	if c.re == nil {
		*sub = reSubject{s: s, units: true, unicode: unicode}
		return
	}
	st := r.regexpState()
	for i := range st.subjects {
		if st.subjects[i].s == s && st.subjects[i].unicode == unicode {
			*sub = st.subjects[i]
			return
		}
	}
	u := f.units()
	text := make([]byte, 0, len(u)+len(u)/2)
	u2b := make([]int32, len(u)+1)
	r.charge(cap(text) + 4*len(u2b))
	var crLS, foldWord, private bool
	for i := 0; i < len(u); i++ {
		u2b[i] = int32(len(text))
		c := u[i]
		switch {
		case c < 0x80:
			text = append(text, byte(c))
			crLS = crLS || c == '\r'
		case c >= 0xD800 && c < 0xE000:
			if unicode && c < 0xDC00 && i+1 < len(u) && u[i+1] >= 0xDC00 && u[i+1] < 0xE000 {
				cp := utf16.DecodeRune(rune(c), rune(u[i+1]))
				text = utf8.AppendRune(text, cp)
				private = private || cp >= surrogateBase && cp < surrogateBase+0x800
				i++
				u2b[i] = int32(len(text)) // the low unit maps past the pair
				continue
			}
			text = utf8.AppendRune(text, surrogateRune(rune(c)))
		default:
			text = utf8.AppendRune(text, rune(c))
			switch c {
			case 0x2028, 0x2029:
				crLS = true
			case 0x17F, 0x212A:
				foldWord = true
			}
		}
	}
	u2b[len(u)] = int32(len(text))
	b2u := make([]int32, len(text)+1)
	for i := range len(u) {
		for b := u2b[i]; b < u2b[i+1]; b++ {
			b2u[b] = int32(i)
		}
	}
	b2u[len(text)] = int32(len(u))
	*sub = reSubject{s: s, text: text, unicode: unicode, crLS: crLS, foldWord: foldWord, private: private, b2u: b2u, u2b: u2b}
	st.subjects[st.subjNext] = *sub
	st.subjNext = (st.subjNext + 1) % int32(len(st.subjects))
}

// --- matching ------------------------------------------------------------------

// matchAt returns the submatch byte indices of the leftmost match starting at
// or after bytePos (exactly at bytePos when sticky), or nil. A pattern that
// never looks left of the match start (no ^, \b or \B) is run on
// text[bytePos:], which keeps RE2's literal-prefix scan; otherwise a search
// from a non-zero position runs a variant of the pattern prefixed with one
// context character on text[prev:], so `^`, `\b` and `\B` see the real
// neighbours (Go's regexp has no start-offset API).
func (c *compiledRegExp) matchAt(r *Realm, sub *reSubject, bytePos int, sticky bool) ([]int, error) {
	if c.run != nil {
		if m, handled, err := c.run.match(r, sub, bytePos, sticky); handled {
			return m, err
		}
	}
	if c.useBT(sub) {
		return c.matchBT(r, sub, bytePos, sticky)
	}
	if bytePos == 0 || !c.tr.leftContext {
		re := c.re
		if sticky {
			var err error
			if re, err = c.anchored(); err != nil {
				return nil, err
			}
		}
		m := re.FindSubmatchIndex(sub.text[bytePos:])
		for i := range m {
			if m[i] >= 0 {
				m[i] += bytePos
			}
		}
		return m, nil
	}
	prev := bytePos - 1
	if !sub.ascii {
		for prev > 0 && !utf8.RuneStart(sub.text[prev]) {
			prev--
		}
	}
	var re *regexp.Regexp
	var err error
	if sticky {
		re, err = c.contextAnchored()
	} else {
		re, err = c.context()
	}
	if err != nil {
		return nil, err
	}
	m := re.FindSubmatchIndex(sub.text[prev:])
	if m == nil {
		return nil, nil
	}
	for i := range m {
		if m[i] >= 0 {
			m[i] += prev
		}
	}
	// m[0] is the start of the consumed context character.
	if sub.ascii {
		m[0]++
	} else {
		_, w := utf8.DecodeRune(sub.text[m[0]:])
		m[0] += w
	}
	return m, nil
}

// matchBT is matchAt on the backtracking VM, which runs on the code units:
// positions are converted when the subject has a working copy.
func (c *compiledRegExp) matchBT(r *Realm, sub *reSubject, bytePos int, sticky bool) ([]int, error) {
	p, err := c.btProgram()
	if err != nil {
		return nil, err
	}
	in := newBTInput(sub.s, c.flags.unicode)
	m, err := p.exec(r, &in, sub.toUnit(bytePos), sticky)
	if m == nil || err != nil {
		return nil, err
	}
	if !sub.ascii && !sub.units {
		for i, u := range m {
			if u >= 0 {
				m[i] = sub.toByte(u)
			}
		}
	}
	return m, nil
}

// regexpUninterruptedLimit is the largest subject (bytes) handed to Go's
// regexp in one FindAll or ReplaceAll call, which cannot observe an
// interrupt; longer subjects go through the match loop, which checks the
// flag every 1024 matches.
const regexpUninterruptedLimit = 1 << 16

// findAll returns every match of the spec's global loop from position 0
// (byte offsets): after a match the search resumes at its end, after an empty
// match one character further. A sticky pattern must match exactly where the
// search resumes, so the loop ends at the first gap. Short subjects with a
// pattern that cannot match empty and is not sticky use Go's FindAll, which
// is identical then.
func (c *compiledRegExp) findAll(r *Realm, sub *reSubject) ([][]int, error) {
	// A pending interrupt is observed before any search: a long subject
	// with few matches is otherwise one uninterruptible RE2 scan.
	if err := r.CheckInterrupt(); err != nil {
		return nil, err
	}
	sticky := c.flags.sticky
	if !c.tr.minLen0 && !sticky && len(sub.text) <= regexpUninterruptedLimit && !c.useBT(sub) {
		return c.re.FindAllSubmatchIndex(sub.text, -1), nil
	}
	var out [][]int
	pos := 0
	for pos <= sub.len() {
		m, err := c.matchAt(r, sub, pos, sticky)
		if err != nil {
			return nil, err
		}
		if m == nil {
			break
		}
		out = append(out, m)
		if m[1] == m[0] {
			pos = sub.advance(m[1])
		} else {
			pos = m[1]
		}
		if len(out)&1023 == 0 {
			if err := r.CheckInterrupt(); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// toUnits converts byte indices to code-unit indices in place.
func (sub *reSubject) toUnits(m []int) {
	if sub.ascii || sub.units {
		return
	}
	for i, b := range m {
		if b >= 0 {
			m[i] = int(sub.b2u[b])
		}
	}
}

// --- lastIndex ----------------------------------------------------------------

var lastIndexKey = StringKey(AtomLastIndex)

// regexpLastIndex implements ToLength(Get(rx, "lastIndex")).
func (r *Realm) regexpLastIndex(rx *Object) (int64, error) {
	var v Value
	if p, attrs, ok := rx.lookupNamed(lastIndexKey); ok && attrs&attrAccessor == 0 {
		v = *p
	} else {
		var err error
		if v, err = rx.GetProp(r, lastIndexKey); err != nil {
			return 0, err
		}
	}
	if v.IsNumber() {
		f := v.AsNumber()
		if f >= 0 && f < maxSafeInteger {
			return int64(f), nil
		}
	}
	return r.ToLength(v)
}

// setRegExpLastIndex implements Set(rx, "lastIndex", v, true).
func (r *Realm) setRegExpLastIndex(rx *Object, v int) error {
	if p, attrs, ok := rx.lookupNamed(lastIndexKey); ok && attrs&(attrAccessor|attrWritable) == attrWritable && rx.flags&flagShared == 0 {
		*p = IntValue(v)
		return nil
	}
	return rx.SetProp(r, lastIndexKey, IntValue(v))
}

// errRecompiled is execLastIndex's report that reading lastIndex recompiled
// the RegExp.
var errRecompiled = errors.New("engine: RegExp recompiled by lastIndex")

// execLastIndex is regexpLastIndex for RegExpBuiltinExec, which reads the
// flags and matcher after lastIndex: when the ToLength of a lastIndex that
// is not a number recompiles rx (RegExp.prototype.compile from valueOf),
// it returns the index it read and errRecompiled, and d.c is the new
// program. A number, the lastIndex of every RegExp that has not been given
// another, runs no user code.
func (r *Realm) execLastIndex(rx *Object, d *RegExpData) (int64, error) {
	if p, attrs, ok := rx.lookupNamed(lastIndexKey); ok && attrs&attrAccessor == 0 && p.IsNumber() {
		if f := p.AsNumber(); f >= 0 && f < maxSafeInteger {
			return int64(f), nil
		}
	}
	c := d.c
	n, err := r.regexpLastIndex(rx)
	if err == nil && d.c != c {
		err = errRecompiled
	}
	return n, err
}

// lastIndexIsNumber reports whether rx's lastIndex is its own data property
// holding a number: coercing it runs no user code, so a path that ignores
// its value may skip it.
func lastIndexIsNumber(rx *Object) bool {
	p, attrs, ok := rx.lookupNamed(lastIndexKey)
	return ok && attrs&attrAccessor == 0 && p.IsNumber()
}

// regexpBuiltinExec implements RegExpBuiltinExec, returning capture indices
// in code units (nil for no match). lastIndex is read and updated per spec,
// then the flags and matcher of d, which reading lastIndex may have changed:
// sub, initialized for the program before, is then initialized again.
func (r *Realm) regexpBuiltinExec(rx *Object, d *RegExpData, s *String, sub *reSubject) ([]int, error) {
	lastIndex, err := r.execLastIndex(rx, d)
	if err != nil {
		if err != errRecompiled {
			return nil, err
		}
		r.initRegExpSubject(sub, s, d.c)
	}
	global, sticky := d.c.flags.global, d.c.flags.sticky
	if !global && !sticky {
		lastIndex = 0
	}
	if lastIndex > int64(s.Len()) {
		if global || sticky {
			if err := r.setRegExpLastIndex(rx, 0); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	m, err := r.regexpMatchFrom(d.c, s, sub, int(lastIndex), sticky)
	if err != nil {
		return nil, err
	}
	if m == nil {
		if global || sticky {
			if err := r.setRegExpLastIndex(rx, 0); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	if global || sticky {
		if err := r.setRegExpLastIndex(rx, m[1]); err != nil {
			return nil, err
		}
	}
	r.noteMatch(d, s, d.c, int32(lastIndex))
	return m, nil
}

// regexpMatchFrom runs the matcher loop of RegExpBuiltinExec from code unit
// lastIndex and returns the capture indices in code units, or nil. In u
// mode a lastIndex inside a surrogate pair designates the pair, so the
// matcher is tried at the pair's start first; the match is then reported at
// lastIndex (the spec's index), and a failure advances past the pair
// (AdvanceStringIndex from a lone trailing surrogate).
func (r *Realm) regexpMatchFrom(c *compiledRegExp, s *String, sub *reSubject, lastIndex int, sticky bool) ([]int, error) {
	if c.flags.unicode && lastIndex > 0 && lastIndex < s.Len() &&
		isLowSurrogate(rune(s.At(lastIndex))) && isHighSurrogate(rune(s.At(lastIndex-1))) {
		m, err := c.matchAt(r, sub, sub.toByte(lastIndex-1), true)
		if err != nil {
			return nil, err
		}
		if m != nil {
			sub.toUnits(m)
			m[0], m[1] = lastIndex, max(m[1], lastIndex)
			return m, nil
		}
		if sticky {
			return nil, nil
		}
		lastIndex++
	}
	m, err := c.matchAt(r, sub, sub.toByte(lastIndex), sticky)
	if m == nil || err != nil {
		return nil, err
	}
	sub.toUnits(m)
	return m, nil
}

// execResultObject co-allocates an exec result array, its length payload
// and its three named slots (index, input, groups).
type execResultObject struct {
	obj   Object
	ad    ArrayData
	slots [3]Value
}

// regexpExecResult builds the exec result array for capture indices m (code
// units): elements, `index`, `input`, `groups` and, with the d flag, `indices`.
func (r *Realm) regexpExecResult(d *RegExpData, s *String, m []int) *Object {
	st := r.regexpState()
	n := d.c.ngroups()
	items := make([]Value, n+1)
	for i := range items {
		if m[2*i] < 0 {
			items[i] = Undefined()
			continue
		}
		items[i] = StringValue(r.substring(s, m[2*i], m[2*i+1]))
	}
	groups := Undefined()
	if d.c.tr.hasNames {
		groups = ObjectValue(r.regexpGroups(d.c, items))
	}
	if st.execShape == nil {
		st.execShape = r.arrayShape().
			addProperty(r, StringKey(AtomIndex), attrDefault).
			addProperty(r, StringKey(AtomInput), attrDefault).
			addProperty(r, StringKey(AtomGroups), attrDefault)
	}
	if !d.c.flags.hasIndices {
		eo := &execResultObject{}
		r.chargeObject(unsafe.Sizeof(execResultObject{}))
		arr := &eo.obj
		arr.shape = st.execShape
		arr.proto = st.execShape.proto
		arr.class = ClassArray
		arr.flags = flagExtensible
		arr.elements = items
		eo.ad = ArrayData{length: uint32(len(items)), lengthWritable: true}
		arr.internal = &eo.ad
		eo.slots = [3]Value{IntValue(m[0]), StringValue(s), groups}
		arr.slots = eo.slots[:]
		return arr
	}
	arr := r.NewArrayFromSlice(items)
	pairs := make([]Value, n+1)
	for i := range pairs {
		if m[2*i] < 0 {
			pairs[i] = Undefined()
			continue
		}
		pairs[i] = ObjectValue(r.NewArray(IntValue(m[2*i]), IntValue(m[2*i+1])))
	}
	indices := r.NewArrayFromSlice(pairs)
	indices.shape = r.arrayShape().addProperty(r, StringKey(AtomGroups), attrDefault)
	indexGroups := Undefined()
	if d.c.tr.hasNames {
		indexGroups = ObjectValue(r.regexpGroups(d.c, pairs))
	}
	indices.slots = []Value{indexGroups}
	arr.shape = st.execShape.addProperty(r, StringKey(AtomIndices), attrDefault)
	arr.slots = []Value{IntValue(m[0]), StringValue(s), groups, ObjectValue(indices)}
	return arr
}

// regexpGroups builds the null-prototype groups object from per-group values.
// A name shared by several groups (only one of which can participate) is one
// property, in the position of its first group, with the value of the group
// that participated.
func (r *Realm) regexpGroups(c *compiledRegExp, values []Value) *Object {
	names := c.tr.names
	g := r.NewObjectWithProto(nil)
	for i := 1; i < len(names); i++ {
		if names[i] == "" {
			continue
		}
		v := values[i]
		if c.tr.dupNames {
			if slices.Contains(names[1:i], names[i]) {
				continue
			}
			for j := i + 1; j < len(names); j++ {
				if names[j] == names[i] && !values[j].IsUndefined() {
					v = values[j]
				}
			}
		}
		g.DefineOwnDataFast(r, r.KeyFromGoString(names[i]), v, attrDefault)
	}
	return g
}

// --- construction -----------------------------------------------------------------

// regexpObject co-allocates a RegExp instance, its payload and its lastIndex slot.
type regexpObject struct {
	obj  Object
	data RegExpData
	slot [1]Value
}

// regexpCreate implements RegExpCreate/RegExpInitialize for an already
// validated flag string.
func (r *Realm) regexpCreate(proto *Object, pattern *String, flagsText *String, flags regexpFlags) (*Object, error) {
	c, err := r.compileRegExp(pattern, flags)
	if err != nil {
		return nil, err
	}
	st := r.regexpState()
	var shape *Shape
	if proto == r.RegExpPrototype {
		if st.shape == nil {
			st.shape = r.rootShapeFor(proto).addProperty(r, lastIndexKey, attrWritable)
		}
		shape = st.shape
	} else {
		r.markPrototype(proto)
		shape = r.rootShapeFor(proto).addProperty(r, lastIndexKey, attrWritable)
	}
	ro := &regexpObject{}
	r.chargeObject(unsafe.Sizeof(regexpObject{}))
	o := &ro.obj
	o.shape = shape
	o.proto = proto
	o.class = ClassRegExp
	o.flags = flagExtensible
	o.slots = ro.slot[:1]
	o.slots[0] = IntValue(0)
	ro.data = RegExpData{source: pattern, flags: flagsText, c: c, legacy: r}
	o.internal = &ro.data
	return o, nil
}

// NewRegExp implements RegExpCreate(pattern, flags) with %RegExp.prototype%
// (used by the interpreter for regular expression literals and by the host
// API). It returns a SyntaxError for invalid flags or patterns.
func (r *Realm) NewRegExp(pattern, flags *String) (*Object, error) {
	return r.regexpFromStrings(r.RegExpPrototype, pattern, flags)
}

func (r *Realm) regexpFromStrings(proto *Object, pattern, flags *String) (*Object, error) {
	f, ok := flags.ASCII()
	fl, valid := parseRegExpFlags(f)
	if !ok || !valid {
		return nil, r.SyntaxError("Invalid flags supplied to RegExp constructor '%s'", flags.GoString())
	}
	canonical := fl.String()
	flagsText := flags
	if canonical != f {
		flagsText = asciiString(canonical)
	}
	return r.regexpCreate(proto, pattern, flagsText, fl)
}

// isRegExpObject reports whether v is a RegExp object (IsRegExp without the
// @@match lookup) and returns its payload.
func isRegExpObject(v Value) (*Object, *RegExpData, bool) {
	if !v.IsObject() {
		return nil, nil, false
	}
	o := v.AsObject()
	d := o.RegExpData()
	return o, d, d != nil
}

func regexpCall(r *Realm, this Value, args []Value) (Value, error) {
	return regexpNew(r, args, nil)
}

func regexpConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	return regexpNew(r, args, newTarget)
}

// regexpNew implements RegExp(pattern, flags); newTarget is nil for a call.
func regexpNew(r *Realm, args []Value, newTarget *Object) (Value, error) {
	pattern, flags := Arg(args, 0), Arg(args, 1)
	patternIsRegExp, err := r.isRegExp(pattern)
	if err != nil {
		return Undefined(), err
	}
	if newTarget == nil {
		newTarget = r.RegExpCtor
		if patternIsRegExp && flags.IsUndefined() {
			ctor, err := pattern.AsObject().GetProp(r, constructorKey)
			if err != nil {
				return Undefined(), err
			}
			if ctor.IsObject() && ctor.AsObject() == newTarget {
				return pattern, nil
			}
		}
	}
	p, f := pattern, flags
	if _, d, ok := isRegExpObject(pattern); ok {
		p = StringValue(d.source)
		if flags.IsUndefined() {
			f = StringValue(d.flags)
		}
	} else if patternIsRegExp {
		po := pattern.AsObject()
		if p, err = po.GetProp(r, StringKey(AtomSource)); err != nil {
			return Undefined(), err
		}
		if flags.IsUndefined() {
			if f, err = po.GetProp(r, flagsKey); err != nil {
				return Undefined(), err
			}
		}
	}
	// RegExpAlloc reads newTarget.prototype before RegExpInitialize
	// coerces the pattern and flags.
	proto, err := r.GetPrototypeFromConstructor(newTarget, r.RegExpCtor, r.RegExpPrototype)
	if err != nil {
		return Undefined(), err
	}
	ps, fs := AtomEmpty, AtomEmpty
	if !p.IsUndefined() {
		if ps, err = r.ToString(p); err != nil {
			return Undefined(), err
		}
	}
	if !f.IsUndefined() {
		if fs, err = r.ToString(f); err != nil {
			return Undefined(), err
		}
	}
	o, err := r.regexpFromStrings(proto, ps, fs)
	if err != nil {
		return Undefined(), err
	}
	if newTarget != r.RegExpCtor {
		o.internal.(*RegExpData).legacy = nil // a subclass instance: no compile
	}
	return ObjectValue(o), nil
}

// thisRegExp validates the receiver of a RegExp.prototype method.
func thisRegExp(r *Realm, this Value, method string) (*Object, *RegExpData, error) {
	o, d, ok := isRegExpObject(this)
	if !ok {
		return nil, nil, r.TypeError("RegExp.prototype.%s requires that 'this' be a RegExp object", method)
	}
	return o, d, nil
}

// --- prototype methods -------------------------------------------------------------

var regexpProtoMethods = []builtinDef{
	{AtomExec, regexpProtoExec, 1},
	{AtomTest, regexpProtoTest, 1},
	{AtomToString, regexpProtoToString, 0},
}

var regexpProtoGetters = []getterDef{
	newGetterDef(AtomSource, regexpGetSource),
	newGetterDef(AtomFlags, regexpGetFlags),
	newGetterDef(AtomGlobal, regexpFlagGetter(func(f regexpFlags) bool { return f.global })),
	newGetterDef(AtomIgnoreCase, regexpFlagGetter(func(f regexpFlags) bool { return f.ignoreCase })),
	newGetterDef(AtomMultiline, regexpFlagGetter(func(f regexpFlags) bool { return f.multiline })),
	newGetterDef(AtomDotAll, regexpFlagGetter(func(f regexpFlags) bool { return f.dotAll })),
	newGetterDef(AtomUnicode, regexpFlagGetter(func(f regexpFlags) bool { return f.unicode && !f.unicodeSets })),
	newGetterDef(AtomUnicodeSets, regexpFlagGetter(func(f regexpFlags) bool { return f.unicodeSets })),
	newGetterDef(AtomSticky, regexpFlagGetter(func(f regexpFlags) bool { return f.sticky })),
	newGetterDef(AtomHasIndices, regexpFlagGetter(func(f regexpFlags) bool { return f.hasIndices })),
}

func installRegExp(r *Realm) {
	fd := r.RegExpCtor.FunctionData()
	fd.SetNative(regexpCall)
	fd.SetConstructor(regexpConstruct)
	if r.buildingShared {
		r.RegExpCtor.ReserveSlots(r, 1+len(regexpStaticDefs)) // @@species (installSpecies), the statics
	} else {
		r.RegExpCtor.ReserveSlots(r, 1) // @@species (installSpecies)
	}
	n := len(regexpProtoMethods) + len(regexpProtoGetters) + len(regexpSymbolMethods)
	if r.buildingShared {
		n++ // compile
	}
	r.RegExpPrototype.ReserveSlots(r, n)
	r.installBuiltins(r.RegExpPrototype, regexpProtoMethods)
	r.installGetters(r.RegExpPrototype, regexpProtoGetters)
	installRegExpProtocol(r)
	installRegExpCompile(r)
}

// RegExpExec runs RegExpBuiltinExec on rx with subject s and returns the
// result array or null (Go-callable entry point).
func (r *Realm) RegExpExec(rx *Object, s *String) (Value, error) {
	d := rx.RegExpData()
	if d == nil {
		return Undefined(), r.TypeError("RegExpExec requires a RegExp object")
	}
	var sub reSubject
	r.initRegExpSubject(&sub, s, d.c)
	m, err := r.regexpBuiltinExec(rx, d, s, &sub)
	if err != nil {
		return Undefined(), err
	}
	if m == nil {
		return Null(), nil
	}
	return ObjectValue(r.regexpExecResult(d, s, m)), nil
}

func regexpProtoExec(r *Realm, this Value, args []Value) (Value, error) {
	rx, _, err := thisRegExp(r, this, "exec")
	if err != nil {
		return Undefined(), err
	}
	s, err := r.ToString(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	return r.RegExpExec(rx, s)
}

// regexpProtoTest implements RegExp.prototype.test: RegExpExec(R, S) !==
// null, answered without a result object when the builtin exec applies.
func regexpProtoTest(r *Realm, this Value, args []Value) (Value, error) {
	if !this.IsObject() {
		return Undefined(), r.TypeError("RegExp.prototype.test called on non-object %s", r.DisplayString(this))
	}
	rx := this.AsObject()
	s, err := r.ToString(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	d := r.pristineRegExp(rx, rxExec)
	if d == nil {
		var exec Value
		if d, exec, err = r.regexpExecMethod(rx); err != nil {
			return Undefined(), err
		}
		if d == nil {
			res, err := r.callExec(rx, exec, s)
			if err != nil {
				return Undefined(), err
			}
			return Bool(!res.IsNull()), nil
		}
	}
	if !d.c.flags.global && !d.c.flags.sticky && lastIndexIsNumber(rx) {
		// lastIndex is neither written nor used, and coercing a number is
		// unobservable (any other value, whose valueOf may throw or even
		// recompile rx, takes RegExpBuiltinExec); a plain match suffices.
		var ok bool
		if d.c.simple != nil {
			_, _, ok = d.c.simple.find(s, 0)
		} else {
			var sub reSubject
			r.initRegExpSubject(&sub, s, d.c)
			if d.c.run != nil && sub.ascii {
				if ok, err = d.c.run.test(r, sub.text); err != nil {
					return Undefined(), err
				}
			} else if !d.c.useBT(&sub) {
				ok = d.c.re.Match(sub.text)
			} else {
				m, err := d.c.matchBT(r, &sub, 0, false)
				if err != nil {
					return Undefined(), err
				}
				ok = m != nil
			}
		}
		if ok {
			r.noteMatch(d, s, d.c, 0)
		}
		return Bool(ok), nil
	}
	var sub reSubject
	r.initRegExpSubject(&sub, s, d.c)
	m, err := r.regexpBuiltinExec(rx, d, s, &sub)
	if err != nil {
		return Undefined(), err
	}
	return Bool(m != nil), nil
}

func regexpProtoToString(r *Realm, this Value, args []Value) (Value, error) {
	if !this.IsObject() {
		return Undefined(), r.TypeError("RegExp.prototype.toString requires that 'this' be an Object")
	}
	o := this.AsObject()
	sv, err := o.GetProp(r, StringKey(AtomSource))
	if err != nil {
		return Undefined(), err
	}
	source, err := r.ToString(sv)
	if err != nil {
		return Undefined(), err
	}
	fv, err := o.GetProp(r, StringKey(AtomFlags))
	if err != nil {
		return Undefined(), err
	}
	flags, err := r.ToString(fv)
	if err != nil {
		return Undefined(), err
	}
	var sb StringBuilder
	sb.Grow(source.Len() + flags.Len() + 2)
	sb.WriteASCII('/')
	sb.WriteString(source)
	sb.WriteASCII('/')
	sb.WriteString(flags)
	return StringValue(r.builtString(&sb)), nil
}

func regexpGetSource(r *Realm, this Value, args []Value) (Value, error) {
	_, d, ok := isRegExpObject(this)
	if !ok {
		if this.IsObject() && this.AsObject() == r.RegExpPrototype {
			return StringValue(asciiString("(?:)")), nil
		}
		return Undefined(), r.TypeError("RegExp.prototype.source getter called on non-RegExp object")
	}
	return StringValue(escapeRegExpPattern(d.source)), nil
}

// escapeRegExpPattern implements EscapeRegExpPattern: "/" and line
// terminators are escaped so that "/" + source + "/" + flags parses back.
// The flags do not matter: a "/" left raw inside a class is valid without
// v, and with v a raw "/" is a syntax error anywhere in a class.
func escapeRegExpPattern(p *String) *String {
	n := p.Len()
	if n == 0 {
		return asciiString("(?:)")
	}
	needs := false
	for i := range n {
		switch p.At(i) {
		case '/', '\n', '\r', 0x2028, 0x2029:
			needs = true
		}
	}
	if !needs {
		return p
	}
	var sb StringBuilder
	sb.Grow(n + 8)
	inClass := false
	for i := 0; i < n; i++ {
		c := p.At(i)
		switch c {
		case '\\':
			// An escaped character is copied as is, except a line
			// terminator: `\` + LF matches the same as `\n`, which the round
			// trip through the constructor needs.
			if i+1 < n && isLineTerminatorUnit(p.At(i+1)) {
				i++
				sb.WriteString(escapeLineTerminator(p.At(i)))
				continue
			}
			sb.WriteUnit(c)
			if i+1 < n {
				i++
				sb.WriteUnit(p.At(i))
			}
			continue
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '/':
			if !inClass {
				sb.WriteGoString(`\/`)
				continue
			}
		case '\n', '\r', 0x2028, 0x2029:
			sb.WriteString(escapeLineTerminator(c))
			continue
		}
		sb.WriteUnit(c)
	}
	return sb.String()
}

func isLineTerminatorUnit(c uint16) bool {
	return c == '\n' || c == '\r' || c == 0x2028 || c == 0x2029
}

// escapeLineTerminator is the source text of a line terminator: a
// terminator never appears raw in RegExp.prototype.source.
func escapeLineTerminator(c uint16) *String {
	switch c {
	case '\n':
		return asciiString(`\n`)
	case '\r':
		return asciiString(`\r`)
	case 0x2028:
		return asciiString(`\u2028`)
	}
	return asciiString(`\u2029`)
}

// regexpFlagProps lists the flag getters get RegExp.prototype.flags reads,
// in the spec's order.
var regexpFlagProps = [...]struct {
	name *String
	ch   byte
}{{AtomHasIndices, 'd'}, {AtomGlobal, 'g'}, {AtomIgnoreCase, 'i'}, {AtomMultiline, 'm'}, {AtomDotAll, 's'}, {AtomUnicode, 'u'}, {AtomUnicodeSets, 'v'}, {AtomSticky, 'y'}}

// regexpGetFlags implements get RegExp.prototype.flags: every flag is read
// with Get, so own properties and replaced getters are observed (the
// matcher itself always uses the flags the RegExp was created with). A
// RegExp whose Gets agree with its creation flags returns its canonical
// flags string instead of a new one.
func regexpGetFlags(r *Realm, this Value, args []Value) (Value, error) {
	if !this.IsObject() {
		return Undefined(), r.TypeError("RegExp.prototype.flags getter called on non-object")
	}
	o := this.AsObject()
	var b [len(regexpFlagProps)]byte
	n := 0
	for _, f := range regexpFlagProps {
		v, err := o.GetProp(r, StringKey(f.name))
		if err != nil {
			return Undefined(), err
		}
		if ToBoolean(v) {
			b[n] = f.ch
			n++
		}
	}
	if d := o.RegExpData(); d != nil {
		if f, _ := d.flags.ASCII(); f == string(b[:n]) {
			return StringValue(d.flags), nil
		}
	}
	return StringValue(asciiString(string(b[:n]))), nil
}

func regexpFlagGetter(get func(regexpFlags) bool) NativeFunc {
	return func(r *Realm, this Value, args []Value) (Value, error) {
		_, d, ok := isRegExpObject(this)
		if !ok {
			if this.IsObject() && this.AsObject() == r.RegExpPrototype {
				return Undefined(), nil
			}
			return Undefined(), r.TypeError("RegExp.prototype flag getter called on non-RegExp object")
		}
		return Bool(get(d.c.flags)), nil
	}
}

// --- hooks used by String.prototype ---------------------------------------------------

// regexpMatch implements RegExp.prototype[@@match].
func regexpMatch(r *Realm, rx *Object, d *RegExpData, s *String) (Value, error) {
	if d.c.flags.global && !d.c.flags.sticky && d.c.simple != nil {
		if err := r.setRegExpLastIndex(rx, 0); err != nil {
			return Undefined(), err
		}
		var items []Value
		last := 0
		for pos := 0; ; {
			start, end, ok := d.c.simple.find(s, pos)
			if !ok {
				break
			}
			items = append(items, StringValue(r.substring(s, start, end)))
			last, pos = start, end
			if len(items)&1023 == 0 {
				if err := r.CheckInterrupt(); err != nil {
					return Undefined(), err
				}
			}
		}
		if items == nil {
			return Null(), nil
		}
		r.noteMatch(d, s, d.c, matchedAt(last))
		return ObjectValue(r.NewArrayFromSlice(items)), nil
	}
	var sub reSubject
	r.initRegExpSubject(&sub, s, d.c)
	if !d.c.flags.global {
		m, err := r.regexpBuiltinExec(rx, d, s, &sub)
		if err != nil {
			return Undefined(), err
		}
		if m == nil {
			return Null(), nil
		}
		return ObjectValue(r.regexpExecResult(d, s, m)), nil
	}
	if err := r.setRegExpLastIndex(rx, 0); err != nil {
		return Undefined(), err
	}
	matches, err := d.c.findAll(r, &sub)
	if err != nil {
		return Undefined(), err
	}
	if len(matches) == 0 {
		return Null(), nil
	}
	r.noteMatch(d, s, d.c, matchedAt(sub.toUnit(matches[len(matches)-1][0])))
	items := make([]Value, len(matches))
	for i, m := range matches {
		items[i] = StringValue(r.substring(s, sub.toUnit(m[0]), sub.toUnit(m[1])))
	}
	return ObjectValue(r.NewArrayFromSlice(items)), nil
}

// regexpSplit implements RegExp.prototype[@@split] for the builtin matcher:
// the spec's sticky probe at every position is equivalent to a leftmost
// search from the current position, skipping an empty match at the start of
// the current piece.
func regexpSplit(r *Realm, d *RegExpData, s *String, lim uint32) (Value, error) {
	if lim == 0 {
		return ObjectValue(r.NewArrayLen(0)), nil
	}
	var sub reSubject
	r.initRegExpSubject(&sub, s, d.c)
	// The spec's splitter is a RegExp of this realm's %RegExp% (the
	// species guard holds), so each of its matches updates the statics,
	// whatever rx's realm or class.
	st := r.regexps
	size := s.Len()
	if size == 0 {
		m, err := d.c.matchAt(r, &sub, 0, true)
		if err != nil {
			return Undefined(), err
		}
		if m != nil {
			st.lastS, st.lastC, st.lastAt = s, d.c, matchedAt(0)
			return ObjectValue(r.NewArrayLen(0)), nil
		}
		return ObjectValue(r.NewArray(StringValue(s))), nil
	}
	ngroups := d.c.ngroups()
	var items []Value
	p := 0 // unit offset of the current piece start
	q := 0 // byte offset of the next search
	pb := 0
	for q < sub.len() {
		m, err := d.c.matchAt(r, &sub, q, false)
		if err != nil {
			return Undefined(), err
		}
		if m == nil || m[0] >= sub.len() {
			break
		}
		if m[1] == pb {
			st.lastS, st.lastC, st.lastAt = s, d.c, matchedAt(sub.toUnit(m[0]))
			q = sub.advance(m[0])
			continue
		}
		sub.toUnits(m)
		st.lastS, st.lastC, st.lastAt = s, d.c, matchedAt(m[0])
		items = append(items, StringValue(r.substring(s, p, m[0])))
		if uint32(len(items)) == lim {
			return ObjectValue(r.NewArrayFromSlice(items)), nil
		}
		for i := 1; i <= ngroups; i++ {
			if m[2*i] < 0 {
				items = append(items, Undefined())
			} else {
				items = append(items, StringValue(r.substring(s, m[2*i], m[2*i+1])))
			}
			if uint32(len(items)) == lim {
				return ObjectValue(r.NewArrayFromSlice(items)), nil
			}
		}
		p = m[1]
		pb = sub.toByte(p)
		q = pb
		if len(items)&1023 == 0 {
			if err := r.CheckInterrupt(); err != nil {
				return Undefined(), err
			}
		}
	}
	items = append(items, StringValue(r.substring(s, p, size)))
	return ObjectValue(r.NewArrayFromSlice(items)), nil
}

// regexpReplace implements RegExp.prototype[@@replace] (String.prototype
// .replace/.replaceAll with a RegExp pattern).
func regexpReplace(r *Realm, rx *Object, d *RegExpData, s *String, replaceValue Value) (Value, error) {
	functional := IsCallable(replaceValue)
	var tmpl *String
	if !functional {
		var err error
		if tmpl, err = r.ToString(replaceValue); err != nil {
			return Undefined(), err
		}
	}
	global := d.c.flags.global
	if d.c.simple != nil && !d.c.flags.sticky && (global || lastIndexIsNumber(rx)) {
		return r.simpleReplace(rx, d, s, replaceValue, tmpl)
	}
	var sub reSubject
	r.initRegExpSubject(&sub, s, d.c)
	var matches [][]int
	if global {
		if err := r.setRegExpLastIndex(rx, 0); err != nil {
			return Undefined(), err
		}
		if !functional && sub.ascii && !d.c.tr.minLen0 && !d.c.flags.sticky && len(sub.text) <= regexpUninterruptedLimit && !d.c.useBT(&sub) {
			if err := r.CheckInterrupt(); err != nil {
				return Undefined(), err
			}
			if res, ok := regexpReplaceFast(d.c, &sub, tmpl); ok {
				if res != s {
					r.noteMatch(d, s, d.c, matchedLast)
				}
				return StringValue(res), nil
			}
		}
		var err error
		if matches, err = d.c.findAll(r, &sub); err != nil {
			return Undefined(), err
		}
		if len(matches) != 0 {
			r.noteMatch(d, s, d.c, matchedAt(sub.toUnit(matches[len(matches)-1][0])))
		}
	} else {
		m, err := r.regexpBuiltinExec(rx, d, s, &sub)
		if err != nil {
			return Undefined(), err
		}
		if m == nil {
			return StringValue(s), nil
		}
		matches = [][]int{m} // already in code units
	}
	if len(matches) == 0 {
		return StringValue(s), nil
	}
	if global {
		for _, m := range matches {
			sub.toUnits(m)
		}
	}
	// The program that found the matches: a replacer may recompile rx.
	c := d.c
	ngroups := c.ngroups()
	var sb StringBuilder
	sb.growFor(s, s.Len()+16)
	next := 0
	held := int64(-1)
	var captures []Value
	if functional || ngroups > 0 || c.tr.hasNames {
		captures = make([]Value, ngroups+1)
	}
	for _, m := range matches {
		position := m[0]
		if captures != nil {
			captures[0] = StringValue(s.Substring(m[0], m[1]))
			for i := 1; i <= ngroups; i++ {
				if m[2*i] < 0 {
					captures[i] = Undefined()
				} else {
					captures[i] = StringValue(s.Substring(m[2*i], m[2*i+1]))
				}
			}
		}
		namedCaptures := Undefined()
		if c.tr.hasNames {
			namedCaptures = ObjectValue(r.regexpGroups(c, captures))
		}
		if position >= next {
			writeSlice(&sb, s, next, position)
		}
		if functional {
			args := make([]Value, 0, ngroups+4)
			args = append(args, captures...)
			args = append(args, IntValue(position), StringValue(s))
			if !namedCaptures.IsUndefined() {
				args = append(args, namedCaptures)
			}
			if held < 0 {
				// The measurements made in the callbacks count the builder,
				// until a return reports nothing again (no defer: the
				// function has too many returns for an open-coded one, and
				// every call would pay).
				held = r.heldBase()
			}
			r.holdBytes(held, sb.heldBytes())
			rv, err := r.Call(replaceValue, Undefined(), args)
			if err != nil {
				r.holdBytes(held, 0)
				return Undefined(), err
			}
			rs, err := r.ToString(rv)
			if err != nil {
				r.holdBytes(held, 0)
				return Undefined(), err
			}
			if position >= next {
				sb.WriteString(rs)
			}
		} else {
			if position >= next {
				var caps []Value
				if captures != nil {
					caps = captures[1:]
				}
				if err := r.appendSubstitution(&sb, s, position, m[1], nil, caps, namedCaptures, tmpl); err != nil {
					return Undefined(), err
				}
			}
		}
		if err := sb.checkLength(r); err != nil {
			r.holdBytes(held, 0)
			return Undefined(), err
		}
		if position >= next {
			next = m[1]
		}
	}
	r.holdBytes(held, 0)
	writeSlice(&sb, s, next, s.Len())
	if err := sb.checkLength(r); err != nil {
		return Undefined(), err
	}
	return StringValue(r.builtString(&sb)), nil
}

// simpleReplace is regexpReplace for a single-class pattern: matches are
// found on the code units and copied into a builder pre-sized in the
// subject's storage kind, so `str.replace(/\s+/g, " ")` allocates the
// result and nothing else.
func (r *Realm) simpleReplace(rx *Object, d *RegExpData, s *String, replaceValue Value, tmpl *String) (Value, error) {
	// A non-global rx comes with a numeric lastIndex (regexpReplace), which
	// RegExpBuiltinExec coerces unobservably and ignores. The spec finds
	// every match before the first replacer call, which may recompile rx:
	// the loop keeps the matcher it started with.
	simple, global := d.c.simple, d.c.flags.global
	if global {
		if err := r.setRegExpLastIndex(rx, 0); err != nil {
			return Undefined(), err
		}
	}
	start, end, ok := simple.find(s, 0)
	if !ok {
		return StringValue(s), nil
	}
	if global {
		r.noteMatch(d, s, d.c, matchedLast)
	} else {
		r.noteMatch(d, s, d.c, 0)
	}
	var sb StringBuilder
	sb.growFor(s, s.Len()+16)
	next := 0
	held := int64(-1)
	if tmpl == nil {
		// The measurements made in the callbacks count the builder, until
		// a return reports nothing again (no defer, which every return
		// would test).
		held = r.heldBase()
	}
	for n := 0; ; n++ {
		writeSlice(&sb, s, next, start)
		if tmpl == nil {
			r.holdBytes(held, sb.heldBytes())
			rv, err := r.Call(replaceValue, Undefined(), []Value{StringValue(s.Substring(start, end)), IntValue(start), StringValue(s)})
			if err != nil {
				r.holdBytes(held, 0)
				return Undefined(), err
			}
			rs, err := r.ToString(rv)
			if err != nil {
				r.holdBytes(held, 0)
				return Undefined(), err
			}
			sb.WriteString(rs)
		} else if err := r.appendSubstitution(&sb, s, start, end, nil, nil, Undefined(), tmpl); err != nil {
			return Undefined(), err
		}
		if err := sb.checkLength(r); err != nil {
			r.holdBytes(held, 0)
			return Undefined(), err
		}
		next = end
		if !global {
			break
		}
		if start, end, ok = simple.find(s, end); !ok {
			break
		}
		if n&1023 == 1023 {
			if err := r.CheckInterrupt(); err != nil {
				r.holdBytes(held, 0)
				return Undefined(), err
			}
		}
	}
	r.holdBytes(held, 0)
	writeSlice(&sb, s, next, s.Len())
	if err := sb.checkLength(r); err != nil {
		return Undefined(), err
	}
	return StringValue(r.builtString(&sb)), nil
}

// regexpReplaceFast handles the hot path `str.replace(/re/g, "template")`
// on ASCII subjects with Go's zero-allocation-per-match ReplaceAll after
// translating the JavaScript template to Go's ${n} syntax. ok is false when
// the template uses $` or $' (no Go equivalent) or is not ASCII, or when
// the result exceeds the string length limit; the generic path then raises
// the RangeError as it builds.
func regexpReplaceFast(c *compiledRegExp, sub *reSubject, tmpl *String) (*String, bool) {
	t, ok := tmpl.ASCII()
	if !ok && tmpl.Len() != 0 {
		return nil, false
	}
	if !c.re.Match(sub.text) {
		return sub.s, true
	}
	var b []byte
	if strings.IndexByte(t, '$') < 0 {
		// No substitution patterns: the template is literal text.
		b = c.re.ReplaceAllLiteral(sub.text, asciiBytes(t))
	} else {
		goTmpl, ok := jsTemplateToGo(t, c)
		if !ok {
			return nil, false
		}
		b = c.re.ReplaceAll(sub.text, goTmpl)
	}
	if len(b) > maxStringLength {
		return nil, false
	}
	return asciiString(bytesToString(b)), true
}

// jsTemplateToGo converts a JavaScript replacement template to Go's Expand
// syntax. It returns nil, true for an empty template.
func jsTemplateToGo(t string, c *compiledRegExp) ([]byte, bool) {
	if len(t) == 0 {
		return nil, true
	}
	m := c.ngroups()
	out := make([]byte, 0, len(t)+8)
	for i := 0; i < len(t); i++ {
		ch := t[i]
		if ch != '$' {
			out = append(out, ch)
			continue
		}
		if i+1 >= len(t) {
			out = append(out, '$', '$')
			continue
		}
		n := t[i+1]
		switch {
		case n == '$':
			out = append(out, '$', '$')
			i++
		case n == '&':
			out = append(out, "${0}"...)
			i++
		case n == '`', n == '\'':
			return nil, false
		case n >= '0' && n <= '9':
			d1 := int(n - '0')
			if i+2 < len(t) && t[i+2] >= '0' && t[i+2] <= '9' {
				d2 := d1*10 + int(t[i+2]-'0')
				if d2 >= 1 && d2 <= m {
					out = append(out, "${"...)
					out = appendIntBytes(out, d2)
					out = append(out, '}')
					i += 2
					continue
				}
			}
			if d1 >= 1 && d1 <= m {
				out = append(out, "${"...)
				out = appendIntBytes(out, d1)
				out = append(out, '}')
				i++
				continue
			}
			out = append(out, '$', '$')
		case n == '<':
			if !c.tr.hasNames {
				out = append(out, '$', '$')
				continue
			}
			if c.tr.dupNames {
				return nil, false // $<a> is whichever group named a participated
			}
			end := -1
			for j := i + 2; j < len(t); j++ {
				if t[j] == '>' {
					end = j
					break
				}
			}
			if end < 0 {
				out = append(out, '$', '$')
				continue
			}
			name := t[i+2 : end]
			idx := 0
			for gi, gn := range c.tr.names {
				if gi > 0 && gn == name {
					idx = gi
					break
				}
			}
			if idx != 0 {
				out = append(out, "${"...)
				out = appendIntBytes(out, idx)
				out = append(out, '}')
			}
			i = end
		default:
			out = append(out, '$', '$')
		}
	}
	return out, true
}

func appendIntBytes(b []byte, v int) []byte {
	if v >= 10 {
		b = append(b, byte('0'+v/10))
	}
	return append(b, byte('0'+v%10))
}

// appendSubstitution implements GetSubstitution, appending the expansion of
// tmpl for the match str[position:matchEnd) to sb. matched is the matched
// string when a user exec reported it (nil: the text at position; matchEnd
// is then position plus its length). captures holds the capture groups
// (strings or undefined) without the whole match.
func (r *Realm) appendSubstitution(sb *StringBuilder, str *String, position, matchEnd int, matched *String, captures []Value, namedCaptures Value, tmpl *String) error {
	n := tmpl.Len()
	if a, ok := tmpl.ASCII(); ok {
		dollar := -1
		for i := range len(a) {
			if a[i] == '$' {
				dollar = i
				break
			}
		}
		if dollar < 0 {
			sb.WriteString(tmpl)
			return nil
		}
	}
	m := len(captures)
	tailPos := min(matchEnd, str.Len())
	for i := 0; i < n; i++ {
		c := tmpl.At(i)
		if c != '$' || i+1 >= n {
			sb.WriteUnit(c)
			continue
		}
		next := tmpl.At(i + 1)
		switch {
		case next == '$':
			sb.WriteASCII('$')
			i++
		case next == '&':
			if matched != nil {
				sb.WriteString(matched)
			} else {
				writeSlice(sb, str, position, tailPos)
			}
			i++
		case next == '`':
			writeSlice(sb, str, 0, position)
			i++
		case next == '\'':
			writeSlice(sb, str, tailPos, str.Len())
			i++
		case next >= '0' && next <= '9':
			d1 := int(next - '0')
			if i+2 < n {
				if c2 := tmpl.At(i + 2); c2 >= '0' && c2 <= '9' {
					d2 := d1*10 + int(c2-'0')
					if d2 >= 1 && d2 <= m {
						if v := captures[d2-1]; !v.IsUndefined() {
							sb.WriteString(v.AsString())
						}
						i += 2
						continue
					}
				}
			}
			if d1 >= 1 && d1 <= m {
				if v := captures[d1-1]; !v.IsUndefined() {
					sb.WriteString(v.AsString())
				}
				i++
				continue
			}
			sb.WriteASCII('$')
		case next == '<':
			if namedCaptures.IsUndefined() {
				sb.WriteASCII('$')
				continue
			}
			end := -1
			for j := i + 2; j < n; j++ {
				if tmpl.At(j) == '>' {
					end = j
					break
				}
			}
			if end < 0 {
				sb.WriteASCII('$')
				continue
			}
			groupName := tmpl.Substring(i+2, end)
			key, err := r.ToPropertyKey(StringValue(groupName))
			if err != nil {
				return err
			}
			capture, err := r.GetV(namedCaptures, key)
			if err != nil {
				return err
			}
			if !capture.IsUndefined() {
				cs, err := r.ToString(capture)
				if err != nil {
					return err
				}
				sb.WriteString(cs)
			}
			i = end
		default:
			sb.WriteASCII('$')
		}
	}
	return nil
}
