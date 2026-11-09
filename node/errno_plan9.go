package node

import (
	"errors"
	"io/fs"
	"strings"
	"syscall"
)

// sysErrno is the type of Plan 9's system errors: strings, as it has no
// errnos.
type sysErrno = syscall.ErrorString

// The errors that this package raises itself and Plan 9's syscall package
// does not name.
const (
	eLOOP     = syscall.ErrorString("too many levels of symbolic links")
	eNOTEMPTY = syscall.ErrorString("directory not empty")
)

// plan9Codes are the codes of syscall's errors and of this package's.
var plan9Codes = map[error]string{
	syscall.EINVAL: "EINVAL", syscall.ENOTDIR: "ENOTDIR", syscall.EISDIR: "EISDIR", syscall.ENOENT: "ENOENT",
	syscall.EEXIST: "EEXIST", syscall.EMFILE: "EMFILE", syscall.EIO: "EIO", syscall.ENAMETOOLONG: "ENAMETOOLONG",
	syscall.EINTR: "EINTR", syscall.EPERM: "EPERM", syscall.EBUSY: "EBUSY", syscall.EACCES: "EACCES",
	syscall.ESPIPE: "ESPIPE", eLOOP: "ELOOP", eNOTEMPTY: "ENOTEMPTY",
}

// errnoName returns the code of e: syscall's errors and this package's by
// their values, the kernel's by the words of their messages.
func errnoName(e syscall.ErrorString) string {
	if c, ok := plan9Codes[e]; ok {
		return c
	}
	switch s := string(e); {
	case strings.Contains(s, "not a directory"):
		return "ENOTDIR"
	case strings.Contains(s, "is a directory"):
		return "EISDIR"
	case strings.Contains(s, "not empty"):
		return "ENOTEMPTY"
	}
	var err error = e
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

// errnoNumber returns libuv's number of code where the system has no
// errno for it, its Windows one, and that of UNKNOWN for one libuv does
// not know.
func errnoNumber(code string) int {
	if n, ok := winErrnoNumbers[code]; ok {
		return n
	}
	return 4094
}
