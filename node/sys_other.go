//go:build !unix

package node

import (
	"io/fs"
	"syscall"
)

func rawRead(fd uintptr, p []byte) (int, error) {
	n, err := syscall.Read(syscall.Handle(fd), p)
	if n < 0 {
		n = 0
	}
	return n, err
}

// checkAccess checks mode against the permission bits of info.
func checkAccess(info fs.FileInfo, mode int) error {
	if mode&wOK != 0 && info.Mode().Perm()&0o200 == 0 {
		return syscall.EACCES
	}
	return nil
}
