//go:build unix || wasip1

package node

import "syscall"

// errnosMore are the errnos of errnos that js/wasm's syscall package does
// not name.
var errnosMore = []errnoEntry{{syscall.ETXTBSY, "ETXTBSY"}}
