//go:build !moejsregcheck

package engine

import "unsafe"

// codeWords is a function's code as run reads it: at reads the word at pc
// with no bounds check. Every pc run reads at lies inside the code, by
// what bytecode.VerifyRegisters checks of every function the engine runs
// (fillMeta, window):
//
//   - run starts a frame at pc 0, at a module body's pc, at a handler's or
//     at the pc a suspension or a rerun saved, the one after its
//     instruction, and each of them starts an instruction;
//   - an instruction's extra words lie inside the code;
//   - every jump lands on the start of an instruction, and the last
//     instruction never continues at the next pc, so no instruction
//     continues past the code.
//
// The checked build of run (regcheck.go) reads the code through a slice,
// with Go's bounds checks.
type codeWords struct{ p unsafe.Pointer }

func codeOf(code []uint32) codeWords { return codeWords{unsafe.Pointer(unsafe.SliceData(code))} }

func (c codeWords) at(pc int) uint32 { return *(*uint32)(unsafe.Add(c.p, pc*4)) }

// ic returns the frame's inline cache entry i, which an instruction names
// in the high 16 bits of its X word, with no bounds check. i is below
// the function's ICCount (bytecode.VerifyRegisters), and the entries from
// icBase to icBase+ICCount are in r.ic: whoever runs a frame binds its
// caches first (callValue and enterFrame, enterDynamic, resumeFrame and
// resumeDynamic, module bodies), a reclaim round unbinds and frees only
// the caches of closures no frame runs (dynamic.go), and run checks the
// range against r.ic when the frame starts, which no frame shrinks. r.ic
// may move as it grows, so ic reads it each time. The checked build
// indexes it.
func (ev *frameEnv) ic(r *Realm, i int) *ICEntry {
	return (*ICEntry)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(r.ic)), (ev.icBase+i)*int(unsafe.Sizeof(ICEntry{}))))
}
