package engine

import (
	"hash/maphash"
	"math/bits"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"
	"weak"
)

// Interning has three levels:
//
//  1. the static atom table (atoms.go), built at package init and immutable;
//  2. a per-realm cache (plain maps, single goroutine, bounded:
//     internCacheMax);
//  3. the process-wide table (internTable): lock-free reads, sharded
//     inserts, weak entries.
//
// Atom identity is process-wide for every name: realm-local shapes, shared
// intrinsic shapes and the materialized constants of compiled functions
// (funcMeta, interp.go, built once per template and shared by every realm)
// all hold the same pointer for a name, and interned strings compare by
// pointer. The table therefore has no capacity limit: a per-shard cap with a
// realm-local fallback would let the first realm to materialize a template
// publish keys no other realm could produce, so their property reads would
// return undefined. Memory stays bounded because the
// entries are weak: a name that no realm, shape or funcMeta references any
// more is collected with them, and the next rebuild of its shard drops its
// entry.

// staticAtoms maps ASCII builtin names to their process-wide *String. It is
// fully populated during package initialization and read-only afterwards.
var staticAtoms = map[string]*String{}

// staticAtomCount is the number of static atoms; dynamic atom ids start
// above it.
var staticAtomCount uint32

// atomCounter hands out dynamic atom ids.
var atomCounter atomic.Uint32

func init() {
	atomCounter.Store(staticAtomCount)
}

// staticAtom interns a builtin name at package init time.
func staticAtom(name string) *String {
	if s, ok := staticAtoms[name]; ok {
		return s
	}
	staticAtomCount++
	s := &String{s: name, n: int32(len(name)), kind: strASCII, atom: staticAtomCount, numeric: canonicalNumericName(name)}
	if name == "" {
		s = emptyString
		s.atom = staticAtomCount
	}
	s.Hash() // precomputed so concurrent readers never write
	staticAtoms[name] = s
	return s
}

// LookupStaticAtom returns the static atom for an ASCII name, if any.
func LookupStaticAtom(name string) (*String, bool) {
	s, ok := staticAtoms[name]
	return s, ok
}

// nextAtom hands out a dynamic atom id.
func nextAtom() uint32 { return takeAtomID(&atomCounter) }

// takeAtomID increments the atom counter c and returns it. Should c ever
// wrap, it skips 0, which means "not interned", and the static ids, so ids
// 1..staticAtomCount stay unique to the static atoms (isStaticAtom,
// coldGlobalSlot).
func takeAtomID(c *atomic.Uint32) uint32 {
	id := c.Add(1)
	for id <= staticAtomCount {
		c.CompareAndSwap(id, staticAtomCount) // concurrent callers retry
		id = c.Add(1)
	}
	return id
}

// internTable is a process-wide string table: content -> weak pointer to the
// canonical *String. Lookups take no lock; an insert locks one of
// internShards shards, chosen by the content's seeded hash, and returns the
// live entry of the same content if there is one, so identity is unique
// per content for as long as any reference to the string exists.
//
// An entry costs a 32-bit tag in a pointer-free array and a weak pointer:
// no node, boxed key or cleanup of its own, so the table adds little to
// what each GC cycle scans. An entry whose string was collected stays
// until its shard is rebuilt, which drops it; a shard is rebuilt when an
// insert would take it past three quarters full, to twice its live
// entries, so the dead entries of names a program no longer uses never
// outnumber the shard's capacity. The tags come from a per-process seed
// (maphash), so no input can make names collide on purpose.
type internTable struct {
	seed   maphash.Seed
	shards [internShards]internShard
}

// internShards is the number of shards of each internTable: inserts on
// different goroutines rarely wait for each other.
const internShards = 256

// internMinSlots is the size of a shard's first slot arrays.
const internMinSlots = 16

type internShard struct {
	mu   sync.Mutex
	tab  atomic.Pointer[internSlots]
	used int // slots holding an entry, live or dead (mu held)
	_    [64 - 8 - 8 - 8]byte
}

// internSlots is one shard's open-addressing table. tag[i] is 0 for an
// empty slot, else the entry's tag (internTag), stored atomically after
// str[i]: a reader that loads a tag that is not 0 reads the str it was
// stored after. A slot is written once; a rebuild fills a new internSlots
// and publishes it whole, so readers of the old one see it unchanged.
type internSlots struct {
	tag  []uint32
	str  []weak.Pointer[String]
	mask uint32
}

var (
	globalInternASCII = newInternTable()
	globalInternUTF16 = newInternTable()
)

func newInternTable() *internTable { return &internTable{seed: strSeed} }

// hashASCII and hashUnits are the table's seeded hashes of ASCII content
// and of code units; with strSeed, strHash32 of one is the String's hash.
func (t *internTable) hashASCII(key string) uint64 { return strHash64(key) }

func (t *internTable) hashUnits(u []uint16) uint64 {
	return maphash.Bytes(t.seed, unsafe.Slice((*byte)(unsafe.Pointer(unsafe.SliceData(u))), 2*len(u)))
}

// shard and internTag split a hash: its top 8 bits pick the shard, and its
// bits 1-31 are the entry's tag, whose low bits pick the slot (tag & mask);
// the tag's top bit is set, so a tag is never 0.
func (t *internTable) shard(h uint64) *internShard { return &t.shards[h>>56] }

func internTag(h uint64) uint32 { return uint32(h>>1) | 1<<31 }

// findASCII returns the live entry of tab whose tag is tag and whose content
// is key, or nil.
func (tab *internSlots) findASCII(tag uint32, key string) *String {
	for i := tag & tab.mask; ; i = (i + 1) & tab.mask {
		switch atomic.LoadUint32(&tab.tag[i]) {
		case 0:
			return nil
		case tag:
			if s := tab.str[i].Value(); s != nil && s.s == key {
				return s
			}
		}
	}
}

// findUnits is findASCII for UTF-16 content.
func (tab *internSlots) findUnits(tag uint32, u []uint16) *String {
	for i := tag & tab.mask; ; i = (i + 1) & tab.mask {
		switch atomic.LoadUint32(&tab.tag[i]) {
		case 0:
			return nil
		case tag:
			if s := tab.str[i].Value(); s != nil && int(s.n) == len(u) && slices.Equal(s.units(), u) {
				return s
			}
		}
	}
}

// lookupASCII returns the live canonical string for ASCII content key, whose
// hash (hashASCII) is h, or nil.
func (t *internTable) lookupASCII(key string, h uint64) *String {
	if tab := t.shard(h).tab.Load(); tab != nil {
		return tab.findASCII(internTag(h), key)
	}
	return nil
}

// lookupUnits is lookupASCII for UTF-16 content.
func (t *internTable) lookupUnits(u []uint16, h uint64) *String {
	if tab := t.shard(h).tab.Load(); tab != nil {
		return tab.findUnits(internTag(h), u)
	}
	return nil
}

// lookup returns the live canonical string for ASCII content key, or nil.
func (t *internTable) lookup(key string) *String { return t.lookupASCII(key, t.hashASCII(key)) }

// intern publishes s, whose hash is h and into which nothing else points, as
// the canonical string for its content, unless a live entry exists, which
// is returned instead. s must be flat and complete but for its atom id,
// which intern sets: readers on other goroutines see it the moment it is in
// the table and never take a lock.
func (t *internTable) intern(h uint64, s *String) *String {
	sh := t.shard(h)
	tag := internTag(h)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	tab := sh.tab.Load()
	if tab != nil {
		var e *String
		if s.kind == strASCII {
			e = tab.findASCII(tag, s.s)
		} else {
			e = tab.findUnits(tag, s.units())
		}
		if e != nil {
			return e
		}
	}
	if tab == nil || 4*(sh.used+1) > 3*len(tab.tag) {
		tab = sh.rebuild(tab)
	}
	if s.atom == 0 {
		s.atom = nextAtom()
	}
	i := tag & tab.mask
	for tab.tag[i] != 0 {
		i = (i + 1) & tab.mask
	}
	tab.str[i] = weak.Make(s)
	atomic.StoreUint32(&tab.tag[i], tag)
	sh.used++
	return s
}

// rebuild replaces the shard's slots (sh.mu held) with ones holding only
// the entries whose strings are alive, at most half full, and returns them.
func (sh *internShard) rebuild(old *internSlots) *internSlots {
	live := 0
	if old != nil {
		for i, tag := range old.tag {
			if tag != 0 && old.str[i].Value() != nil {
				live++
			}
		}
	}
	n := internMinSlots
	for n < 2*(live+1) {
		n *= 2
	}
	tab := &internSlots{tag: make([]uint32, n), str: make([]weak.Pointer[String], n), mask: uint32(n - 1)}
	used := 0
	if old != nil {
		for i, tag := range old.tag {
			// A string can die between the two passes: it is dropped here.
			if tag == 0 || old.str[i].Value() == nil {
				continue
			}
			j := tag & tab.mask
			for tab.tag[j] != 0 {
				j = (j + 1) & tab.mask
			}
			tab.tag[j], tab.str[j] = tag, old.str[i]
			used++
		}
	}
	sh.used = used
	sh.tab.Store(tab)
	return tab
}

// Intern returns the process-wide canonical *String for the content of s.
// Interned strings compare by pointer, which is what PropertyKey equality
// relies on.
func (r *Realm) Intern(s *String) *String {
	if s.atom != 0 {
		return s
	}
	f := s // s is returned above, f never: a view stays on the stack
	if s.needsFlat() {
		f = s.flat(new(String))
	}
	if f.kind == strASCII {
		return r.internASCII(f.s)
	}
	return r.internUTF16(f)
}

// InternGoString interns a Go string.
func (r *Realm) InternGoString(g string) *String {
	if a, ok := staticAtoms[g]; ok {
		return a
	}
	if a, ok := r.internCacheASCII[g]; ok {
		return a
	}
	if !isASCII(g) {
		return r.internUTF16(FromGoString(g))
	}
	return r.internASCII(g)
}

// internASCII interns ASCII content.
func (r *Realm) internASCII(key string) *String {
	if a, ok := staticAtoms[key]; ok {
		return a
	}
	if a, ok := r.internCacheASCII[key]; ok {
		return a
	}
	a := globalASCIIAtom(key)
	if len(key) > maxSharedDataKey {
		return a // not kept by the cache (internCacheMax)
	}
	if r.internCacheASCII == nil {
		if !r.cacheInterning() {
			return a
		}
		r.internCacheASCII = make(map[string]*String)
	} else if len(r.internCacheASCII) >= internCacheMax {
		clear(r.internCacheASCII)
	}
	// Keyed by the atom's own content, never by key, which may be a slice
	// of a JSON text or of a large string (builtin_json.go, Substring).
	r.internCacheASCII[a.s] = a
	return a
}

// globalASCIIAtom interns ASCII content in the process-wide table.
func globalASCIIAtom(key string) *String {
	h := globalInternASCII.hashASCII(key)
	a := globalInternASCII.lookupASCII(key, h)
	if a == nil {
		// The table gets its own String header and its own copy of the
		// content, never the caller's: a header carved from a FromGo slab
		// would otherwise pin the whole slab, and content sharing a JSON text
		// or a large string would pin that, for as long as the name is in use
		// (hostconv.go, builtin_json.go). Once per new atom.
		key = strings.Clone(key)
		s := asciiString(key)
		s.hash = strHash32(h)
		s.numeric = canonicalNumericName(key)
		a = globalInternASCII.intern(h, s)
	}
	return a
}

// constASCIIAtom returns the atom for the ASCII string constant str of shared
// compiled code, publishing it marked strConst, or nil where the constant
// keeps its own String: past maxSharedDataKey bytes, or when the content's
// atom exists without the mark (a static atom, or one interned from a
// realm's data), which the live measurement counts where a realm reaches
// it while it skips constants. A marked atom is skipped as the constant
// is, in every realm that reaches it: the template holds it, as it held
// the constant.
func constASCIIAtom(str string) *String {
	if len(str) > maxSharedDataKey {
		return nil
	}
	if _, ok := staticAtoms[str]; ok {
		return nil
	}
	h := globalInternASCII.hashASCII(str)
	a := globalInternASCII.lookupASCII(str, h)
	if a == nil {
		key := strings.Clone(str)
		s := asciiString(key)
		s.hash = strHash32(h)
		s.numeric = canonicalNumericName(key)
		s.flags |= strConst
		a = globalInternASCII.intern(h, s)
	}
	if a.flags&strConst == 0 {
		return nil
	}
	return a
}

// globalUTF16Atom interns the content of s, a flat UTF-16 string, in the
// process-wide table.
func globalUTF16Atom(s *String) *String {
	u := s.units()
	h := globalInternUTF16.hashUnits(u)
	a := globalInternUTF16.lookupUnits(u, h)
	if a == nil {
		// Fresh header and code units for the table, as in globalASCIIAtom:
		// a substring shares its string's units (Substring).
		// Hashed here, not through s: a caller's string is not written (it
		// may be shared, or a rope's view).
		c := make([]uint16, s.n)
		copy(c, u)
		x := &String{p: unitsPtr(c), n: s.n, kind: strUTF16, hash: strHash32(h)}
		a = globalInternUTF16.intern(h, x)
	}
	return a
}

// InternKey is Realm.KeyFromGoString without a realm: the key comes from
// the process-wide tables, so it is valid in every realm. A host that
// resolves property names once (a hook path, a struct field) keeps the key
// and with it the atom alive.
func InternKey(g string) PropertyKey {
	if i, ok := parseArrayIndex(g); ok {
		return IndexKey(i)
	}
	if a, ok := staticAtoms[g]; ok {
		return StringKey(a)
	}
	if !isASCII(g) {
		s := FromGoString(g)
		return StringKey(globalUTF16Atom(s))
	}
	return StringKey(globalASCIIAtom(g))
}

// internCacheAfter is the number of names a realm interns through the
// process-wide tables alone before it creates its caches: a runtime that
// only binds a few host globals never pays for the maps (256 bytes), and one
// that runs code gets them within its first lookups.
const internCacheAfter = 8

// internCacheMax bounds each of a realm's intern caches. They only front the
// process-wide tables, which keep atom identity on their own, so a cache that
// reaches it is emptied and refills with the names in use: a pooled runtime
// that interns the ids and user values of every request (host map keys,
// obj[key], JSON.parse) holds at most this many, and the names it no longer
// uses can be collected. The bound is checked on the miss path only. A
// name longer than maxSharedDataKey bytes is not cached: the caches would
// keep up to this many of them alive, and the live measurement (memlive.go)
// does not count what they keep.
const internCacheMax = 4096

// cacheInterning reports whether the realm, which has no cache for the kind
// of name at hand, should create it now.
func (r *Realm) cacheInterning() bool {
	if r.internLookups < internCacheAfter {
		r.internLookups++
		return false
	}
	return true
}

func (r *Realm) internUTF16(s *String) *String {
	key := utf16Key(s.units())
	if a, ok := r.internCacheUTF16[key]; ok {
		return a
	}
	a := globalUTF16Atom(s)
	if len(key) > maxSharedDataKey {
		return a // not kept by the cache (internCacheMax)
	}
	if r.internCacheUTF16 == nil {
		if !r.cacheInterning() {
			return a
		}
		r.internCacheUTF16 = make(map[string]*String, 8)
	} else if len(r.internCacheUTF16) >= internCacheMax {
		clear(r.internCacheUTF16)
	}
	r.internCacheUTF16[key] = a
	return a
}

// strSeed keys the hash of string content, process-wide: String.Hash, the
// intern tables and the keyed collections (collHash) hash with it, so
// strings from untrusted input cannot be chosen to collide in a shape's
// table, a dictionary or a Map, and an atom's table hash gives its String
// hash.
var strSeed = maphash.MakeSeed()

// strHash32 folds a seeded content hash to String.Hash's 32 bits (never 0).
func strHash32(h uint64) uint32 {
	if x := uint32(h ^ h>>32); x != 0 {
		return x
	}
	return 1
}

// strHash64 is the seeded hash of ASCII content, the intern table's: past
// 16 bytes maphash with strSeed; up to 16 as Go's runtime hashes them where
// it has no AES (memhashFallback, from wyhash): the bytes as two words,
// which together hold all of them, each with a key, multiplied 64 by 64
// bits into 128, whose halves are combined; then again with the length.
// Most property names and JSON keys are short, and a JSON.parse hashes
// each key's bytes once per object (childNamed), where maphash's calls cost
// more than the hashing; the short path is in this function so that a
// hash is one call. strHash32 of it is String.Hash for ASCII content.
func strHash64(s string) uint64 {
	n := len(s)
	var a, b uint64
	switch {
	case n == 0:
	case n < 4:
		a = uint64(s[0]) | uint64(s[n>>1])<<8 | uint64(s[n-1])<<16
	case n <= 8:
		a, b = le32(s), le32(s[n-4:])
	case n <= 16:
		a, b = le64(s), le64(s[n-8:])
	default:
		return maphash.String(strSeed, s)
	}
	return hashMix(strKeys[2]^uint64(n), hashMix(a^strKeys[1], b^strKeys[0]))
}

// strKeys key strHash64's short path, drawn from strSeed.
var strKeys = [3]uint64{maphash.String(strSeed, "0"), maphash.String(strSeed, "1"), maphash.String(strSeed, "2")}

func hashMix(a, b uint64) uint64 {
	hi, lo := bits.Mul64(a, b)
	return hi ^ lo
}

// le32 and le64 read the first 4 or 8 bytes of s, little-endian.
func le32(s string) uint64 {
	_ = s[3]
	return uint64(s[0]) | uint64(s[1])<<8 | uint64(s[2])<<16 | uint64(s[3])<<24
}

func le64(s string) uint64 {
	_ = s[7]
	return uint64(s[0]) | uint64(s[1])<<8 | uint64(s[2])<<16 | uint64(s[3])<<24 |
		uint64(s[4])<<32 | uint64(s[5])<<40 | uint64(s[6])<<48 | uint64(s[7])<<56
}

// utf16Key encodes code units as a map key. UTF-16 strings are kept in a
// separate table from ASCII strings, so the little-endian byte encoding cannot
// collide with an ASCII key and lone surrogates stay distinguishable (a
// UTF-8 key would fold them into U+FFFD).
func utf16Key(u []uint16) string {
	b := make([]byte, 2*len(u))
	for i, c := range u {
		b[2*i] = byte(c)
		b[2*i+1] = byte(c >> 8)
	}
	return bytesToString(b)
}
