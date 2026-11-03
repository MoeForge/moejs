package engine

// Layout: the linker places functions in file order, each aligned to 32
// bytes by default, and the interpreter's speed depends on the 64-byte
// phase of (*Realm).run (propGetMono moves by about 8 %). This file's name
// and ReleaseCallData's size, and the places of a few functions elsewhere
// in the package (escapeByte, proxy, sweepWeak, JobsPending, ownCap), were
// tuned to keep the hot functions' phase in the test binaries. That tuning
// is over: the gate's binaries link with -funcalign=64 (docs/DESIGN.md
// §14.2), where every function starts a 64-byte line whatever comes before
// it, so none of these sizes and places needs keeping.

// Past these sizes ReleaseCallData lets a deep recursion's register stack
// and frame table go instead of clearing them; the next call allocates the
// initial sizes again.
const (
	maxRetainedStack  = 4096 // 64 KiB of registers
	maxRetainedFrames = 256  // 4 KiB
)

// ReleaseCallData drops the realm's references to the host values of
// finished calls, so a pooled realm does not keep the last request alive:
// the host conversion's chunks, whose placeholders hold the Go maps, slices
// and strings of the arguments (hostHeap); the registers and frame records
// the finished calls left above the live ones, and the RegExp working
// copies of their subjects (ReleaseScratch), which keep what the calls
// made and dropped reachable for Go's collector until later calls
// overwrite them; and the last match, which the legacy RegExp statics
// (RegExp.$1, RegExp.input and the rest) describe: they read "" again, as
// in a new realm. Values already returned stay valid: their nodes keep
// their own storage. It also starts a new memory budget (ResetAllocation).
//
// It ends the host conversion's period, which otherwise ends at the next
// top-level FromGo, so a host calls it where a request ends, after the
// request's last use of the realm (CallObject, ToGo, GetV, JSONStringify:
// each can run JavaScript): a request that runs several hooks on one realm
// is one period, whose usage sizes the next request's chunks. Calls do not
// do it themselves: clearing the register stack after each would tax every
// call, hosts that do not pool included, and split a request into periods.
//
// Between calls only; a no-op inside one (from a native function).
func (r *Realm) ReleaseCallData() {
	if r.callDepth != 0 {
		return
	}
	if h := r.fromGo.heap; h != nil {
		h.seal()
	}
	r.releaseScratch()
	if rs := r.regexps; rs != nil {
		rs.lastS, rs.lastC = nil, nil
	}
	if lz := r.lazy; lz != nil {
		lz.statics = nil
		if lz.mem != nil {
			r.ResetAllocation()
		}
	}
}

// ReleaseScratch drops what finished calls left in the interpreter's
// working state, the part of ReleaseCallData that neither the host nor
// JavaScript can observe: the registers and frame records above the live
// frames, which keep the values the calls used, and the functions they ran
// with their scopes, reachable for Go's collector until later calls
// overwrite them, and the RegExp working copies of their subjects. Calls
// do not do it themselves, which would tax every call: a host that keeps
// a realm after the code it ran is done calls it, as package eventloop
// does at the end of a run of its loop, or ReleaseCallData, which does it
// too.
//
// Between calls only; a no-op inside one (from a native function).
func (r *Realm) ReleaseScratch() {
	if r.callDepth == 0 {
		r.releaseScratch()
	}
}

// releaseScratch is ReleaseScratch, at depth 0. No frame is live there, so
// sp and nframes are 0; the registers and records below them would be kept
// all the same. Of the register stack only the extent the calls used, its
// length (growStack), holds anything to clear, and of the frame table the
// records the calls wrote, a prefix: records are written from the bottom
// up, every one with its function, and releaseScratch leaves the rest
// zero. So the release costs what the calls used, not the capacity the
// stack and the table keep.
func (r *Realm) releaseScratch() {
	st := &r.interp
	if st.sp == 0 && cap(st.stack) > maxRetainedStack {
		st.stack = nil
	} else {
		clear(st.stack[st.sp:])
		st.stack = st.stack[:st.sp]
	}
	if st.nframes == 0 && len(st.frames) > maxRetainedFrames {
		st.frames = nil
	} else {
		for i := st.nframes; i < len(st.frames) && st.frames[i].fn != nil; i++ {
			st.frames[i] = frameInfo{}
		}
	}
	if rs := r.regexps; rs != nil {
		rs.subjNext = 0
		clear(rs.subjects[:])
	}
}
