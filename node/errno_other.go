//go:build !unix

package node

import (
	"errors"
	"io/fs"
	"syscall"
)

// errnoNumbers are libuv's codes on Windows, negated.
var errnoNumbers = map[string]int{
	"E2BIG": 4093, "EACCES": 4092, "EAGAIN": 4088, "EBADF": 4083, "EBUSY": 4082, "ECANCELED": 4081,
	"EEXIST": 4075, "EFAULT": 4074, "EFBIG": 4036, "EINTR": 4072, "EINVAL": 4071, "EIO": 4070,
	"EISDIR": 4068, "ELOOP": 4067, "EMFILE": 4066, "EMLINK": 4032, "ENAMETOOLONG": 4064, "ENFILE": 4060,
	"ENODEV": 4055, "ENOENT": 4058, "ENOMEM": 4057, "ENOSPC": 4055, "ENOSYS": 4054, "ENOTDIR": 4052,
	"ENOTEMPTY": 4051, "ENOTSUP": 4049, "ENXIO": 4033, "EPERM": 4048, "EROFS": 4034, "ESPIPE": 4035,
	"ETXTBSY": 4031, "EXDEV": 4037,
}

func errnoName(n syscall.Errno) string {
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

func errnoNumber(code string) int {
	if n, ok := errnoNumbers[code]; ok {
		return n
	}
	return 4094
}
