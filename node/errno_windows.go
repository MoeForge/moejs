package node

import (
	"errors"
	"io/fs"
	"runtime"
	"syscall"
)

// errnoNames are the names of the errnos, libuv's. On Windows, these are
// the values Go invents (above APPLICATION_ERROR), which the os package
// and this package raise themselves.
var errnoNames = map[syscall.Errno]string{
	syscall.E2BIG: "E2BIG", syscall.EACCES: "EACCES", syscall.EAGAIN: "EAGAIN", syscall.EBADF: "EBADF",
	syscall.EBUSY: "EBUSY", syscall.ECANCELED: "ECANCELED", syscall.EEXIST: "EEXIST", syscall.EFAULT: "EFAULT",
	syscall.EFBIG: "EFBIG", syscall.EINTR: "EINTR", syscall.EINVAL: "EINVAL", syscall.EIO: "EIO",
	syscall.EISDIR: "EISDIR", syscall.ELOOP: "ELOOP", syscall.EMFILE: "EMFILE", syscall.EMLINK: "EMLINK",
	syscall.ENAMETOOLONG: "ENAMETOOLONG", syscall.ENFILE: "ENFILE", syscall.ENODEV: "ENODEV",
	syscall.ENOMEM: "ENOMEM", syscall.ENOSPC: "ENOSPC", syscall.ENOSYS: "ENOSYS",
	syscall.ENOTEMPTY: "ENOTEMPTY", syscall.ENOTSUP: "ENOTSUP", syscall.ENXIO: "ENXIO", syscall.EPERM: "EPERM",
	syscall.EROFS: "EROFS", syscall.ESPIPE: "ESPIPE", syscall.ETXTBSY: "ETXTBSY", syscall.EXDEV: "EXDEV",
}

// errnoName returns the code of n. On Windows, a system error code goes
// through libuv's uv_translate_sys_error, so that ERROR_PATH_NOT_FOUND
// (Go's ENOTDIR) is ENOENT and ERROR_ACCESS_DENIED is EPERM, as Node.js
// reports them; Go's invented values are named.
func errnoName(n syscall.Errno) string {
	if runtime.GOOS == "windows" && uint32(n) < 1<<29 {
		return winSysErrorCode(uint32(n))
	}
	if c, ok := errnoNames[n]; ok {
		return c
	}
	switch n {
	case syscall.ENOENT:
		return "ENOENT"
	case syscall.ENOTDIR:
		return "ENOTDIR"
	}
	var err error = n
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "ENOENT"
	case errors.Is(err, fs.ErrExist):
		return "EEXIST"
	case errors.Is(err, fs.ErrPermission):
		return "EACCES"
	}
	return ""
}

// errnoNumber returns libuv's code of code on Windows, negated, and that
// of UNKNOWN for one libuv does not know.
func errnoNumber(code string) int {
	if n, ok := winErrnoNumbers[code]; ok {
		return n
	}
	return 4094
}
