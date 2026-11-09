package engine

import "unsafe"

// Polymorphic inline caches. A GetProp or SetProp site whose entry is
// valid for another shape than the receiver's turns polymorphic: it gets
// polyWays more entries, a polyIC of the realm's polyTable, found by the
// site's index in r.ic. The site's own entry stays the run loop's, for the
// shape the site saw last, so a run of receivers of one shape hits as at
// a monomorphic site. A miss on it swaps in the entry for the receiver's
// shape from the polyIC; when there is none, the site's entry goes first
// into the polyIC, the last one drops out, and the site's entry is filled
// as at a monomorphic site. Only a site that turns polymorphic allocates,
// and only after the sites of one code tree (a script, a module) have
// missed so polyAfter times, counted in the tree's bases (icTrees), so that
// a realm that makes a few such misses, as a plugin's initialization does,
// allocates nothing for them. A site whose receivers keep pushing
// entries out of its polyIC, more than it can serve, turns megamorphic:
// from then on a miss takes the slow path as at a monomorphic site, past
// one lookup of its polyIC. Like r.ic, the table holds entries for the
// sites of the realm's code, which bounds it, and the memory account is
// not charged for it.
//
// Every entry is checked against the receiver's shape and the realm's
// epoch before use, wherever it is, but not against the key: a polyIC
// holds the entries of one site, the one at its index. Dynamic code (eval,
// the Function constructors) has none, as the entries of its functions
// return to a free list once no closure of the code is in use
// (reclaimDyn) and another function's sites reuse them. The sites of
// other code keep their index for the life of the realm.

// polyWays is the number of entries of a polyIC.
const polyWays = 4

// polyIC is the more entries of a polymorphic site, the last pushed first,
// and what they served.
type polyIC struct {
	w      [polyWays]ICEntry
	hits   uint32 // misses of the site's entry an entry of w took
	evicts uint32 // entries pushed out of w
	mega   bool   // the site is megamorphic (polyEvict)
}

// polyMegaEvicts is the evictions after which a site whose polyIC takes
// fewer than polyMegaRatio of its misses per eviction turns megamorphic.
const (
	polyMegaEvicts = 16
	polyMegaRatio  = 4
)

// polyAfter is the number of polymorphic misses the sites of a code tree
// make before the realm gives the first of them a polyIC.
const polyAfter = 64

// polyTable is the polyICs of the realm's polymorphic sites, behind
// realmLazy.poly.
type polyTable struct {
	slots []polySlot // open addressing on the site's index, a power of two of them
	ways  []polyIC
}

// polySlot maps the site at index site-1 of r.ic to ways[way]; site is
// zero in a free slot.
type polySlot struct{ site, way uint32 }

// icSite returns 1 + the index of e in r.ic, or 0 when e is no entry of
// r.ic.
func (r *Realm) icSite(e *ICEntry) uint32 {
	i := (uintptr(unsafe.Pointer(e)) - uintptr(unsafe.Pointer(unsafe.SliceData(r.ic)))) / unsafe.Sizeof(ICEntry{})
	if i >= uintptr(len(r.ic)) {
		return 0
	}
	return uint32(i) + 1
}

// polyTable returns the realm's polyTable, or nil when no site is
// polymorphic.
func (r *Realm) polyTable() *polyTable {
	if l := r.lazy; l != nil {
		return l.poly
	}
	return nil
}

// find returns the polyIC of site, or nil; t may be nil. The slow paths
// inline it and icSite to take an entry of a polymorphic site without a
// call, around which they would spill their operands.
func (t *polyTable) find(site uint32) *polyIC {
	if t == nil {
		return nil
	}
	mask := uint32(len(t.slots) - 1)
	for j := site; ; j++ {
		s := &t.slots[j&mask]
		if s.site == site {
			return &t.ways[s.way]
		}
		if s.site == 0 {
			return nil
		}
	}
}

// findNext is find out of line, for getPropSlow and setPropSlow.
//
//go:noinline
func (t *polyTable) findNext(site uint32) *polyIC { return t.find(site) }

// addPolySite gives the site at index site-1 of r.ic a polyIC, unless site
// is 0, the innermost frame, whose site it is, runs dynamic code, or the
// realm has no polyTable and the sites of the frame's code tree have yet
// to miss polyAfter times.
func (r *Realm) addPolySite(site uint32) *polyIC {
	st := &r.interp
	m := st.frames[st.nframes-1].fn.funcData().meta
	if site == 0 || m.dyn&dynFlag != 0 {
		return nil
	}
	var t *polyTable
	if l := r.lazy; l != nil {
		t = l.poly
	}
	if t == nil {
		bases := r.icTrees[m.root]
		if len(bases) == 0 {
			return nil
		}
		if n := &bases[len(bases)-treePolyMisses]; *n < polyAfter {
			*n++
			return nil
		}
		t = &polyTable{slots: make([]polySlot, 16)}
		r.lazyState().poly = t
	} else if 2*(len(t.ways)+1) > len(t.slots) {
		old := t.slots
		t.slots = make([]polySlot, 2*len(old))
		for _, s := range old {
			if s.site != 0 {
				t.insert(s)
			}
		}
	}
	t.ways = append(t.ways, polyIC{})
	t.insert(polySlot{site, uint32(len(t.ways) - 1)})
	return &t.ways[len(t.ways)-1]
}

// insert adds s to the slots, which have a free one.
func (t *polyTable) insert(s polySlot) {
	mask := uint32(len(t.slots) - 1)
	j := s.site & mask
	for t.slots[j].site != 0 {
		j = (j + 1) & mask
	}
	t.slots[j] = s
}

// push makes t the first entry of p and drops the last, of polyWays = 4.
// The entries move one by one: copy would call typedslicecopy for the
// shapes.
func (p *polyIC) push(t ICEntry) {
	p.w[3], p.w[2], p.w[1], p.w[0] = p.w[2], p.w[1], p.w[0], t
}

// polyEvict makes room for the receiver's entry at the site of e, which
// missed on e and on p, its polyIC or nil when it has none: it pushes e into
// p, giving the site a polyIC first, empties e and returns its entry. ok is
// false, and e left as it is, at a site of dynamic code, which gets no
// polyIC, and at a megamorphic site: one that has pushed polyMegaEvicts
// entries out of p and more than one per polyMegaRatio misses p took turns
// so here.
func (r *Realm) polyEvict(e *ICEntry, p *polyIC) (old ICEntry, ok bool) {
	if p == nil {
		if p = r.addPolySite(r.icSite(e)); p == nil {
			return ICEntry{}, false
		}
	} else if p.w[polyWays-1].Shape != nil {
		if p.evicts++; p.evicts >= polyMegaEvicts && p.evicts*polyMegaRatio > p.hits {
			p.mega = true
			return ICEntry{}, false
		}
	}
	old = *e
	p.push(old)
	*e = ICEntry{}
	return old, true
}

// polyMiss reports whether e, a site's entry on which a receiver of shape
// s missed, is valid for another shape, so that the site is or turns
// polymorphic.
func (r *Realm) polyMiss(e *ICEntry, s *Shape) bool {
	return e.Shape != nil && e.Shape != s && e.Epoch|1 == r.protoEpoch|1
}

// getPropSlow is getNamedSlow for a GetProp site, which may be
// polymorphic. An entry of its polyIC for o's shape becomes the site's;
// read at once when it is of a data property of the realm's epoch, else
// used or filled again as at a monomorphic site. It probes the first slot
// of the polyTable for the site's polyIC itself, which holds it unless
// another site's is there, and leaves the rest of a polymorphic miss to
// getPropMiss: their calls would make it spill its operands on the way to
// the hit.
//
//go:noinline
func (r *Realm) getPropSlow(o *Object, receiver Value, key PropertyKey, e *ICEntry) (Value, error) {
	if r.polyMiss(e, o.shape) {
		var p *polyIC
		if t := r.polyTable(); t != nil {
			if site := r.icSite(e); site != 0 {
				if s := t.slots[site&uint32(len(t.slots)-1)]; s.site == site {
					p = &t.ways[s.way]
				} else if s.site != 0 {
					p = t.findNext(site)
				}
			}
		}
		if p != nil {
			if p.mega {
				return r.getNamedSlow(o, receiver, key, e)
			}
			for i := range p.w {
				if w := &p.w[i]; w.Shape == o.shape {
					*e, *w = *w, *e
					p.hits++
					if e.Epoch == r.protoEpoch {
						return e.Holder(o).slots[e.Slot()], nil
					}
					break
				}
			}
		}
		return r.getPropMiss(o, receiver, key, e, p)
	}
	return r.getNamedSlow(o, receiver, key, e)
}

// getPropMiss is getPropSlow at a polymorphic site whose polyIC, p, or nil,
// has no entry for o's shape, or had one of another epoch, which is now the
// site's and filled again.
func (r *Realm) getPropMiss(o *Object, receiver Value, key PropertyKey, e *ICEntry, p *polyIC) (Value, error) {
	if e.Shape != o.shape {
		return r.getPropPoly(o, receiver, key, e, p)
	}
	return r.getNamedSlow(o, receiver, key, e)
}

// getPropPoly is getPropSlow on a site that is or turns polymorphic, with
// no entry for o's shape: the site's entry is filled from empty, so that an
// absent property gets one (fillGetIC), and keeps the old one when it gets
// none, as a monomorphic site's does.
func (r *Realm) getPropPoly(o *Object, receiver Value, key PropertyKey, e *ICEntry, p *polyIC) (Value, error) {
	old, ok := r.polyEvict(e, p)
	v, err := r.getNamedSlow(o, receiver, key, e)
	if ok && e.Shape == nil {
		*e = old
	}
	return v, err
}

// getStringPoly is getPropPoly for a string primitive, whose entries are
// String.prototype's (getStringProp).
func (r *Realm) getStringPoly(v Value, key PropertyKey, e *ICEntry, p *polyIC) (Value, error) {
	old, ok := r.polyEvict(e, p)
	res, err := r.getStringMono(v, key, e)
	if ok && e.Shape == nil {
		*e = old
	}
	return res, err
}

// setPropPoly is getPropPoly for setPropSlow.
func (r *Realm) setPropPoly(o *Object, key PropertyKey, v Value, e *ICEntry, p *polyIC) error {
	old, ok := r.polyEvict(e, p)
	err := r.setPropMono(o, key, v, e)
	if ok && e.Shape == nil {
		*e = old
	}
	return err
}
