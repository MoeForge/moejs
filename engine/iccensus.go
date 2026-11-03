//go:build iccensus

package engine

import (
	"cmp"
	"slices"
	"sync"

	"github.com/MoeForge/moejs/bytecode"
)

// The inline-cache census (build tag iccensus) counts, per IC site, the
// hits and the misses of the interpreter's inline caches, to show where
// polymorphic caches would pay. Normal builds compile its
// hooks out (iccensus_off.go).

const icCensus = true

// ICSiteCensus is the census of one inline-cache site.
type ICSiteCensus struct {
	Code *bytecode.Function
	Site uint32
	Op   bytecode.Op
	Key  string // the property name
	PC   int    // the instruction's pc in Code (Code.Position)
	Hits uint64
	// Misses by the state of the entry: Cold (empty), Stale (it held the
	// receiver's shape but is invalid: a prototype changed, or for
	// DefineField the object cannot take the transition), Poly (it held
	// another shape).
	Cold, Stale, Poly uint64
	// Poly misses a polymorphic cache of 2 and 4 entries would have hit:
	// the receiver's shape was among the 2 (4) most recently cached ones.
	Saved2, Saved4 uint64
	Shapes         int // distinct receiver shapes (at most maxCensusShapes)
	shapes         map[*Shape]struct{}
	recent         [4]*Shape // the shapes cached most recently, newest first
}

// Misses returns the site's misses.
func (s *ICSiteCensus) Misses() uint64 { return s.Cold + s.Stale + s.Poly }

const maxCensusShapes = 64

type icSiteKey struct {
	code *bytecode.Function
	site uint32
}

var census struct {
	sync.Mutex
	sites map[icSiteKey]*ICSiteCensus
}

// censusSite returns the census of the site of the instruction at pc, whose
// extra word x holds the site and the constant index of the key.
func censusSite(code *bytecode.Function, op bytecode.Op, x uint32, pc int) *ICSiteCensus {
	if census.sites == nil {
		census.sites = make(map[icSiteKey]*ICSiteCensus)
	}
	k := icSiteKey{code, x >> 16}
	s := census.sites[k]
	if s == nil {
		key := "length"
		if op != bytecode.GetLen {
			key = code.Consts[uint16(x)].Str
		}
		s = &ICSiteCensus{Code: code, Site: x >> 16, Op: op, Key: key, PC: pc, shapes: make(map[*Shape]struct{})}
		census.sites[k] = s
	}
	return s
}

func censusHit(code *bytecode.Function, op bytecode.Op, x uint32, pc int, shape *Shape) {
	census.Lock()
	s := censusSite(code, op, x, pc)
	s.Hits++
	s.see(shape)
	census.Unlock()
}

// see records shape as the site's newest cached shape.
func (s *ICSiteCensus) see(shape *Shape) {
	i := 0
	for i < len(s.recent)-1 && s.recent[i] != shape {
		i++
	}
	copy(s.recent[1:i+1], s.recent[:i])
	s.recent[0] = shape
	if len(s.shapes) < maxCensusShapes {
		s.shapes[shape] = struct{}{}
		s.Shapes = len(s.shapes)
	}
}

// censusMiss records a miss of entry e at a site whose receiver has shape.
// A DefineField entry caches the transition, taken from e.Shape.parent.
func censusMiss(code *bytecode.Function, op bytecode.Op, x uint32, pc int, e *ICEntry, shape *Shape) {
	census.Lock()
	s := censusSite(code, op, x, pc)
	cached := e.Shape
	if op == bytecode.DefineField && cached != nil {
		cached = cached.parent
	}
	switch cached {
	case nil:
		s.Cold++
	case shape:
		s.Stale++
	default:
		s.Poly++
		for i, r := range s.recent {
			if r == shape {
				if i < 2 {
					s.Saved2++
				}
				s.Saved4++
				break
			}
		}
		s.see(shape)
	}
	census.Unlock()
}

// ICCensus returns the sites counted since the last ResetICCensus, most
// polymorphic misses first.
func ICCensus() []ICSiteCensus {
	census.Lock()
	defer census.Unlock()
	out := make([]ICSiteCensus, 0, len(census.sites))
	for _, s := range census.sites {
		out = append(out, *s)
	}
	slices.SortFunc(out, func(a, b ICSiteCensus) int {
		if c := cmp.Compare(b.Poly, a.Poly); c != 0 {
			return c
		}
		if c := cmp.Compare(b.Misses(), a.Misses()); c != 0 {
			return c
		}
		return cmp.Compare(b.Hits, a.Hits)
	})
	return out
}

// ResetICCensus clears the census.
func ResetICCensus() {
	census.Lock()
	census.sites = nil
	census.Unlock()
}
