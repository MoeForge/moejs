package engine

import (
	"math/bits"

	"github.com/MoeForge/moejs/bytecode"
)

// Dynamic code: the code eval, the Function constructors and
// Realm.EvalScript compile from strings. A program can compile any number
// of pieces of it over a realm's life, so the realm must not keep what it
// built for code the program no longer runs. Each piece therefore gets a
// realm-private meta tree (dynMeta) instead of the process-wide one of
// metaFor, and everything the realm holds for the code hangs from it:
//
//   - The inline caches of a function are a range of the realm's table,
//     bound to the tree while the program runs its code. A closure binds
//     it on its first call (bindDynamic) and stays bound; the tree lists
//     its bound closures. Reclaim rounds (reclaimDyn) unbind every listed
//     closure no frame runs, so the next call of each binds again and
//     marks its tree used, and return the ranges of the trees neither used
//     since the last round nor running to free lists by size class. The
//     table thus holds the sites of the code a program runs, not of all it
//     compiled, and a closure whose ranges were reclaimed binds new ones on
//     its next call and warms them again. A free range is reused only for
//     its size class and the table never shrinks (allocDynIC), so the
//     dynamic part of the table is bounded by the sum over the size classes
//     of each class's peak of ranges bound at once.
//   - The template objects of its tagged templates live in its metas'
//     constants (templateObject), not in the realm's list.
//   - The script or module record it belongs to is its dynRoot's.
//
// The realm refers to a tree only from the caches of compiled code
// (compileEval, compileFunction) and from the held trees, and to a
// closure only from the list of its held tree until the next round, all
// bounded, so a tree and all it holds are collected with its last closure.
// Nothing needs a finalizer or a weak pointer.
//
// A closure is assumed to run in its own realm, as with host code: another
// realm that calls one binds its caches in its own table (icBaseFor), where
// they stay, for the call only.

// dynFlag marks the funcMeta.dyn of every meta of a dynamic tree. Its low
// bits are the base+1 of the function's inline caches in the realm's table,
// zero while they are unbound.
const dynFlag = 1 << 31

// dynRoundMin is the number of held trees and bound closures that starts
// the first reclaim round.
const dynRoundMin = 64

// dynRoot is the root meta of a dynamic tree with the realm's state of the
// tree; dynRootOf returns it for the root's funcMeta.
type dynRoot struct {
	funcMeta
	code  *bytecode.Function
	ref   *scriptOrModule // the record of the script or module the code belongs to, or nil
	realm *Realm          // the realm that compiled the code
	bound []dynRange      // the functions whose inline caches are bound
	fds   []*FunctionData // the closures bound to them since the last reclaim round
	used  bool            // a closure bound since the last round, or one ran then
}

// dynRange is the inline-cache range of the function of the meta m: 1<<class
// entries from the base m.dyn holds.
type dynRange struct {
	m     *funcMeta
	class uint8
}

// dynState is the realm's state of its dynamic code (realmLazy.dyn).
type dynState struct {
	evals  map[evalKey]*dynRoot       // compiled eval code (compileEval)
	funcs  map[funcKey]*dynRoot       // compiled dynamic functions (compileFunction)
	held   []*dynRoot                 // the trees with bound inline caches
	nfds   int                        // the closures their fds list
	next   int                        // the len(held)+nfds that starts the next reclaim round
	free   [][]uint32                 // the bases of the free ranges of 1<<class entries, by class
	size   int                        // the entries of the table dynamic code owns, free ones included
	active map[*FunctionData]struct{} // during a round: the closures a frame runs
}

// dynState returns the realm's state of its dynamic code, creating it on
// first use.
func (r *Realm) dynState() *dynState {
	l := r.lazyState()
	if l.dyn == nil {
		l.dyn = &dynState{next: dynRoundMin}
	}
	return l.dyn
}

// dynMeta builds the realm-private meta tree of the template code compiled
// from a string, which belongs to the script or module of the record ref
// (nil: none). A function of the tree that fails bytecode.VerifyRegisters
// is an error (fillMeta).
func (r *Realm) dynMeta(code *bytecode.Function, ref *scriptOrModule) (*dynRoot, error) {
	d := &dynRoot{code: code, ref: ref, realm: r}
	d.dyn = dynFlag
	n := uint32(0)
	if err := r.fillMeta(&d.funcMeta, code, &d.funcMeta, &n); err != nil {
		return nil, err
	}
	d.nfuncs = n
	return d, nil
}

// scriptClosure returns a function object of the dynamic script template of
// d: instantiateTopLevel with its meta.
func (r *Realm) scriptClosure(d *dynRoot) *Object {
	fn, _ := r.newClosure(d.code, &d.funcMeta, NewEnv(nil, len(d.code.CaptureLayout)), Undefined())
	return fn
}

// enterDynamic is enterFrame for a closure of dynamic code whose inline
// caches are unbound. Caches bound in another realm's table are unbound
// when the call returns, a panic a host recovers included.
func (r *Realm) enterDynamic(fn *Object, fd *FunctionData, this Value, args []Value) (Value, error) {
	if r.bindDynamic(fd) {
		defer func() { fd.icBase = icUnbound }()
	}
	return r.enterFrame(fn, fd, this, args)
}

// resumeDynamic is resumeFrame for a generator or async function of dynamic
// code whose inline caches are unbound.
func (r *Realm) resumeDynamic(g *genFrame, v, mode Value) (Value, error) {
	fd := g.fd
	if r.bindDynamic(fd) {
		defer func() { fd.icBase = icUnbound }()
	}
	return r.resumeFrame(g, v, mode)
}

// bindDynamic binds the inline caches of the closure fd of dynamic code,
// binding a range of the table to its function if it has none: a free one
// of its size class, else new entries. A tree that binds its first range
// joins the held trees, and a reclaim round runs first when the held trees
// and bound closures have doubled since the last. It reports whether the
// caches are another realm's, which the caller unbinds when the frame ends.
func (r *Realm) bindDynamic(fd *FunctionData) bool {
	m, count := fd.meta, fd.code.ICCount
	if count == 0 {
		fd.icBase = 0 // nothing to reclaim
		return false
	}
	d := dynRootOf(m.root)
	if d == nil || d.realm != r {
		fd.icBase = r.icBaseFor(m, count)
		return true
	}
	ds := r.dynState()
	if len(ds.held)+ds.nfds >= ds.next {
		r.reclaimDyn(ds)
	}
	d.used = true
	if m.dyn&^dynFlag == 0 {
		if len(d.bound) == 0 {
			ds.held = append(ds.held, d)
		}
		class := uint8(bits.Len32(count - 1))
		b := r.allocDynIC(ds, class)
		d.bound = append(d.bound, dynRange{m, class})
		m.dyn = dynFlag | (b + 1)
	}
	d.fds = append(d.fds, fd)
	ds.nfds++
	fd.icBase = m.dyn&^dynFlag - 1
	return false
}

// allocDynIC returns the base of a free range of 1<<class entries, growing
// the table by doubling when there is none.
func (r *Realm) allocDynIC(ds *dynState, class uint8) uint32 {
	if int(class) < len(ds.free) {
		if f := ds.free[class]; len(f) > 0 {
			ds.free[class] = f[:len(f)-1]
			return f[len(f)-1]
		}
	}
	n, size := len(r.ic), 1<<class
	if n+size > cap(r.ic) {
		r.resizeIC(max(2*cap(r.ic), n+size))
	}
	r.ic = r.ic[:n+size]
	ds.size += size
	return uint32(n)
}

// reclaimDyn runs a reclaim round. It unbinds the listed closures no frame
// runs (a running one keeps its caches: the frame reads them), and the
// held trees neither used since the last round nor running return their
// ranges to the free lists. A tree still running, or with a closure still
// bound, is used for the next round too. A tree is thus released at most
// two rounds after it last ran, and a round runs once the held trees and
// bound closures have doubled, so its cost is amortized over the binds
// since the last.
func (r *Realm) reclaimDyn(ds *dynState) {
	st := &r.interp
	for _, f := range st.frames[:st.nframes] {
		if f.fn == nil {
			continue
		}
		if fd, ok := f.fn.internal.(*FunctionData); ok && fd.meta != nil {
			if d := dynRootOf(fd.meta.root); d != nil && d.realm == r {
				d.used = true
				if ds.active == nil {
					ds.active = make(map[*FunctionData]struct{})
				}
				ds.active[fd] = struct{}{}
			}
		}
	}
	keep, nfds := ds.held[:0], 0
	for _, d := range ds.held {
		fds := d.fds[:0]
		for _, fd := range d.fds {
			if _, ok := ds.active[fd]; ok {
				fds = append(fds, fd)
			} else {
				fd.icBase = icUnbound
			}
		}
		clear(d.fds[len(fds):])
		d.fds = fds
		nfds += len(fds)
		if d.used {
			d.used = len(fds) > 0
			keep = append(keep, d)
			continue
		}
		for _, b := range d.bound {
			base := b.m.dyn&^dynFlag - 1
			clear(r.ic[base : base+1<<b.class])
			for len(ds.free) <= int(b.class) {
				ds.free = append(ds.free, nil)
			}
			ds.free[b.class] = append(ds.free[b.class], base)
			b.m.dyn = dynFlag
		}
		clear(d.bound)
		d.bound = d.bound[:0]
	}
	clear(ds.held[len(keep):])
	ds.held = keep
	ds.nfds = nfds
	ds.next = max(dynRoundMin, 2*(len(keep)+nfds))
	clear(ds.active)
}
