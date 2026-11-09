//go:build !unix && !windows

package node

import (
	"io/fs"
	"syscall"
)

// oNonblock is no flag on js/wasm, WASI and Plan 9, which have no
// non-blocking open (Plan 9's O_NONBLOCK is 0): opening a FIFO without a
// writer may block there.
const oNonblock = 0

func rawRead(fd uintptr, p []byte) (int, error) {
	n, err := syscall.Read(int(fd), p)
	if n < 0 {
		n = 0
	}
	return n, err
}

// checkAccess checks mode against the owner's permission bits of info, as
// if the process owned every file: there are no users to compare here, or
// none this package knows of.
func checkAccess(info fs.FileInfo, mode int) error {
	if uint32(mode)&^(uint32(info.Mode().Perm()>>6)&7) != 0 {
		return syscall.EACCES
	}
	return nil
}
