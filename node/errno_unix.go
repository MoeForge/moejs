//go:build unix || js || wasip1

package node

import "syscall"

// errnoEntry is an errno and its code, the name libuv gives it.
type errnoEntry struct {
	n    syscall.Errno
	code string
}

// errnos are the errnos and their codes, with errnosMore, those that the
// syscall package of js/wasm does not name.
var errnos = append([]errnoEntry{
	{syscall.E2BIG, "E2BIG"}, {syscall.EACCES, "EACCES"}, {syscall.EAGAIN, "EAGAIN"}, {syscall.EBADF, "EBADF"},
	{syscall.EBUSY, "EBUSY"}, {syscall.ECANCELED, "ECANCELED"}, {syscall.EEXIST, "EEXIST"}, {syscall.EFAULT, "EFAULT"},
	{syscall.EFBIG, "EFBIG"}, {syscall.EINTR, "EINTR"}, {syscall.EINVAL, "EINVAL"}, {syscall.EIO, "EIO"},
	{syscall.EISDIR, "EISDIR"}, {syscall.ELOOP, "ELOOP"}, {syscall.EMFILE, "EMFILE"}, {syscall.EMLINK, "EMLINK"},
	{syscall.ENAMETOOLONG, "ENAMETOOLONG"}, {syscall.ENFILE, "ENFILE"}, {syscall.ENODEV, "ENODEV"},
	{syscall.ENOENT, "ENOENT"}, {syscall.ENOMEM, "ENOMEM"}, {syscall.ENOSPC, "ENOSPC"}, {syscall.ENOSYS, "ENOSYS"},
	{syscall.ENOTDIR, "ENOTDIR"}, {syscall.ENOTEMPTY, "ENOTEMPTY"}, {syscall.ENOTSUP, "ENOTSUP"},
	{syscall.ENXIO, "ENXIO"}, {syscall.EPERM, "EPERM"}, {syscall.EROFS, "EROFS"}, {syscall.ESPIPE, "ESPIPE"},
	{syscall.EXDEV, "EXDEV"}, {syscall.EDQUOT, "EDQUOT"},
}, errnosMore...)

// errnoCodes are the codes of the errnos. Where two codes share an errno,
// as EEXIST and ENOTEMPTY do on AIX, the first in errnos names it: EEXIST
// there, which libuv's map lists first too.
var errnoCodes = func() map[syscall.Errno]string {
	m := make(map[syscall.Errno]string, len(errnos))
	for _, e := range errnos {
		if _, ok := m[e.n]; !ok {
			m[e.n] = e.code
		}
	}
	return m
}()

// errnoNumbers are the errnos of the codes; Node's errno property is the
// negated errno, as libuv's codes are on Unix.
var errnoNumbers = func() map[string]int {
	m := make(map[string]int, len(errnos))
	for _, e := range errnos {
		m[e.code] = int(e.n)
	}
	return m
}()

func errnoName(n syscall.Errno) string { return errnoCodes[n] }

// errnoNumber returns the errno of code, and that of EIO for one this
// package does not know (libuv's UNKNOWN is -4094).
func errnoNumber(code string) int {
	if n, ok := errnoNumbers[code]; ok {
		return n
	}
	return 4094
}
