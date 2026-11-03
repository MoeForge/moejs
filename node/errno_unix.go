//go:build unix

package node

import "syscall"

// errnoCodes are the codes of the errnos, the names libuv gives them.
var errnoCodes = map[syscall.Errno]string{
	syscall.E2BIG: "E2BIG", syscall.EACCES: "EACCES", syscall.EAGAIN: "EAGAIN", syscall.EBADF: "EBADF",
	syscall.EBUSY: "EBUSY", syscall.ECANCELED: "ECANCELED", syscall.EEXIST: "EEXIST", syscall.EFAULT: "EFAULT",
	syscall.EFBIG: "EFBIG", syscall.EINTR: "EINTR", syscall.EINVAL: "EINVAL", syscall.EIO: "EIO",
	syscall.EISDIR: "EISDIR", syscall.ELOOP: "ELOOP", syscall.EMFILE: "EMFILE", syscall.EMLINK: "EMLINK",
	syscall.ENAMETOOLONG: "ENAMETOOLONG", syscall.ENFILE: "ENFILE", syscall.ENODEV: "ENODEV",
	syscall.ENOENT: "ENOENT", syscall.ENOMEM: "ENOMEM", syscall.ENOSPC: "ENOSPC", syscall.ENOSYS: "ENOSYS",
	syscall.ENOTDIR: "ENOTDIR", syscall.ENOTEMPTY: "ENOTEMPTY", syscall.ENOTSUP: "ENOTSUP",
	syscall.ENXIO: "ENXIO", syscall.EPERM: "EPERM", syscall.EROFS: "EROFS", syscall.ESPIPE: "ESPIPE",
	syscall.ETXTBSY: "ETXTBSY", syscall.EXDEV: "EXDEV", syscall.EDQUOT: "EDQUOT",
}

// errnoNumbers are the errnos of the codes; Node's errno property is the
// negated errno, as libuv's codes are on Unix.
var errnoNumbers = func() map[string]int {
	m := make(map[string]int, len(errnoCodes))
	for n, c := range errnoCodes {
		m[c] = int(n)
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
