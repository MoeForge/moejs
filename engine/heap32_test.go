package engine

import "runtime/debug"

// On a 32-bit platform the heap cannot grow past the address space, which
// the collector's pacing does not know: a cycle that marks while a test
// holds gigabytes (TestAuditB_ConcatRopeFlattenPanics holds 2.4 GB on 386)
// sets the next goal at twice that, past 4 GB, and the tests after it then
// allocate without another cycle until the address space runs out, in
// whichever test comes next. A soft memory limit below the address space
// makes the collector run first. 64-bit platforms keep the default pacing.
func init() {
	if !is64 {
		debug.SetMemoryLimit(2560 << 20)
	}
}
