package engine

import (
	"unsafe"

	"github.com/MoeForge/moejs/bytecode"
)

// The funcMeta of a dynamic root is the first field of its dynRoot, so a
// pointer to the one is a pointer to the other.
var _ [0]struct{} = [unsafe.Offsetof(dynRoot{}.funcMeta)]struct{}{}

// dynRootOf returns the dynRoot whose meta m is, or nil when m is not the
// root of a dynamic tree. dynMeta allocates every such root as a dynRoot,
// so the conversion stays within the allocation.
func dynRootOf(m *funcMeta) *dynRoot {
	if m.dyn&dynFlag == 0 || m.root != m {
		return nil
	}
	return (*dynRoot)(unsafe.Pointer(m))
}

// constIndex returns the index of k in consts, which must hold it: the
// offset of k from the first element, which needs no search.
func constIndex(consts []bytecode.Const, k *bytecode.Const) int {
	off := uintptr(unsafe.Pointer(k)) - uintptr(unsafe.Pointer(unsafe.SliceData(consts)))
	return int(off / unsafe.Sizeof(bytecode.Const{}))
}
