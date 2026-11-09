//go:build !plan9

package node

import "syscall"

// sysErrno is the type of the system's error numbers; errno_plan9.go has
// Plan 9's, which has none.
type sysErrno = syscall.Errno

// The errnos that this package raises itself and Plan 9's syscall package
// does not name.
const (
	eLOOP     = syscall.ELOOP
	eNOTEMPTY = syscall.ENOTEMPTY
)
