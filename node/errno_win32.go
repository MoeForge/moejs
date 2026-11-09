package node

// winSysErrorCode returns the code of err for Windows, through libuv's
// uv_translate_sys_error (v1.52.1 src/win/error.c). This is the mapping
// from the Windows codes that os and os.Root return and that the package
// raises itself. OS-neutral, no build tag.
func winSysErrorCode(err uint32) string {
	// libuv v1.52.1 uv_translate_sys_error, extracted by parsing the
	// switch cases and joining Go's ERROR_* constants with their numeric
	// values.
	switch err {
	case 10013, 740, 1920:
		// 10013 = WSAEACCES
		// 740 = ERROR_ELEVATION_REQUIRED
		// 1920 = ERROR_CANT_ACCESS_FILE
		return "EACCES"
	case 1227, 10048:
		// 1227 = ERROR_ADDRESS_ALREADY_ASSOCIATED
		// 10048 = WSAEADDRINUSE
		return "EADDRINUSE"
	case 10049:
		// 10049 = WSAEADDRNOTAVAIL
		return "EADDRNOTAVAIL"
	case 10047:
		// 10047 = WSAEAFNOSUPPORT
		return "EAFNOSUPPORT"
	case 10035, 232:
		// 10035 = WSAEWOULDBLOCK
		// 232 = ERROR_NO_DATA
		return "EAGAIN"
	case 10037:
		// 10037 = WSAEALREADY
		return "EALREADY"
	case 1004, 6:
		// 1004 = ERROR_INVALID_FLAGS
		// 6 = ERROR_INVALID_HANDLE
		return "EBADF"
	case 33, 231, 32:
		// 33 = ERROR_LOCK_VIOLATION
		// 231 = ERROR_PIPE_BUSY
		// 32 = ERROR_SHARING_VIOLATION
		return "EBUSY"
	case 995, 10004:
		// 995 = ERROR_OPERATION_ABORTED
		// 10004 = WSAEINTR
		return "ECANCELED"
	case 1113:
		// 1113 = ERROR_NO_UNICODE_TRANSLATION
		return "ECHARSET"
	case 1236, 10053:
		// 1236 = ERROR_CONNECTION_ABORTED
		// 10053 = WSAECONNABORTED
		return "ECONNABORTED"
	case 1225, 10061:
		// 1225 = ERROR_CONNECTION_REFUSED
		// 10061 = WSAECONNREFUSED
		return "ECONNREFUSED"
	case 64, 10054:
		// 64 = ERROR_NETNAME_DELETED
		// 10054 = WSAECONNRESET
		return "ECONNRESET"
	case 183, 80:
		// 183 = ERROR_ALREADY_EXISTS
		// 80 = ERROR_FILE_EXISTS
		return "EEXIST"
	case 998, 10014:
		// 998 = ERROR_NOACCESS
		// 10014 = WSAEFAULT
		return "EFAULT"
	case 1232, 10065:
		// 1232 = ERROR_HOST_UNREACHABLE
		// 10065 = WSAEHOSTUNREACH
		return "EHOSTUNREACH"
	case 122, 13, 87, 1464, 10022, 10046:
		// 122 = ERROR_INSUFFICIENT_BUFFER
		// 13 = ERROR_INVALID_DATA
		// 87 = ERROR_INVALID_PARAMETER
		// 1464 = ERROR_SYMLINK_NOT_SUPPORTED
		// 10022 = WSAEINVAL
		// 10046 = WSAEPFNOSUPPORT
		return "EINVAL"
	case 1102, 1111, 23, 1166, 1165, 1393, 1129, 1101, 31, 1106, 1117, 1104, 205, 110, 1103, 156:
		// 1102 = ERROR_BEGINNING_OF_MEDIA
		// 1111 = ERROR_BUS_RESET
		// 23 = ERROR_CRC
		// 1166 = ERROR_DEVICE_DOOR_OPEN
		// 1165 = ERROR_DEVICE_REQUIRES_CLEANING
		// 1393 = ERROR_DISK_CORRUPT
		// 1129 = ERROR_EOM_OVERFLOW
		// 1101 = ERROR_FILEMARK_DETECTED
		// 31 = ERROR_GEN_FAILURE
		// 1106 = ERROR_INVALID_BLOCK_LENGTH
		// 1117 = ERROR_IO_DEVICE
		// 1104 = ERROR_NO_DATA_DETECTED
		// 205 = ERROR_NO_SIGNAL_SENT
		// 110 = ERROR_OPEN_FAILED
		// 1103 = ERROR_SETMARK_DETECTED
		// 156 = ERROR_SIGNAL_REFUSED
		return "EIO"
	case 10056:
		// 10056 = WSAEISCONN
		return "EISCONN"
	case 1921:
		// 1921 = ERROR_CANT_RESOLVE_FILENAME
		return "ELOOP"
	case 4, 10024:
		// 4 = ERROR_TOO_MANY_OPEN_FILES
		// 10024 = WSAEMFILE
		return "EMFILE"
	case 10040:
		// 10040 = WSAEMSGSIZE
		return "EMSGSIZE"
	case 111, 206:
		// 111 = ERROR_BUFFER_OVERFLOW
		// 206 = ERROR_FILENAME_EXCED_RANGE
		return "ENAMETOOLONG"
	case 1231, 10051:
		// 1231 = ERROR_NETWORK_UNREACHABLE
		// 10051 = WSAENETUNREACH
		return "ENETUNREACH"
	case 10055:
		// 10055 = WSAENOBUFS
		return "ENOBUFS"
	case 161, 267, 203, 2, 123, 15, 4392, 126, 3, 11001, 11004:
		// 161 = ERROR_BAD_PATHNAME
		// 267 = ERROR_DIRECTORY
		// 203 = ERROR_ENVVAR_NOT_FOUND
		// 2 = ERROR_FILE_NOT_FOUND
		// 123 = ERROR_INVALID_NAME
		// 15 = ERROR_INVALID_DRIVE
		// 4392 = ERROR_INVALID_REPARSE_DATA
		// 126 = ERROR_MOD_NOT_FOUND
		// 3 = ERROR_PATH_NOT_FOUND
		// 11001 = WSAHOST_NOT_FOUND
		// 11004 = WSANO_DATA
		return "ENOENT"
	case 8, 14:
		// 8 = ERROR_NOT_ENOUGH_MEMORY
		// 14 = ERROR_OUTOFMEMORY
		return "ENOMEM"
	case 82, 112, 277, 1100, 39:
		// 82 = ERROR_CANNOT_MAKE
		// 112 = ERROR_DISK_FULL
		// 277 = ERROR_EA_TABLE_FULL
		// 1100 = ERROR_END_OF_MEDIA
		// 39 = ERROR_HANDLE_DISK_FULL
		return "ENOSPC"
	case 2250, 10057:
		// 2250 = ERROR_NOT_CONNECTED
		// 10057 = WSAENOTCONN
		return "ENOTCONN"
	case 145:
		// 145 = ERROR_DIR_NOT_EMPTY
		return "ENOTEMPTY"
	case 10038:
		// 10038 = WSAENOTSOCK
		return "ENOTSOCK"
	case 50:
		// 50 = ERROR_NOT_SUPPORTED
		return "ENOTSUP"
	case 109:
		// 109 = ERROR_BROKEN_PIPE
		return "EOF"
	case 5, 1314:
		// 5 = ERROR_ACCESS_DENIED
		// 1314 = ERROR_PRIVILEGE_NOT_HELD
		return "EPERM"
	case 230, 233, 10058:
		// 230 = ERROR_BAD_PIPE
		// 233 = ERROR_PIPE_NOT_CONNECTED
		// 10058 = WSAESHUTDOWN
		return "EPIPE"
	case 10043:
		// 10043 = WSAEPROTONOSUPPORT
		return "EPROTONOSUPPORT"
	case 19:
		// 19 = ERROR_WRITE_PROTECT
		return "EROFS"
	case 121, 10060:
		// 121 = ERROR_SEM_TIMEOUT
		// 10060 = WSAETIMEDOUT
		return "ETIMEDOUT"
	case 17:
		// 17 = ERROR_NOT_SAME_DEVICE
		return "EXDEV"
	case 1:
		// 1 = ERROR_INVALID_FUNCTION
		return "EISDIR"
	case 208:
		// 208 = ERROR_META_EXPANSION_TOO_LONG
		return "E2BIG"
	case 10044:
		// 10044 = WSAESOCKTNOSUPPORT
		return "ESOCKTNOSUPPORT"
	case 193:
		// 193 = ERROR_BAD_EXE_FORMAT
		return "EFTYPE"
	default:
		return "UNKNOWN"
	}
}

// winErrnoNumbers are libuv v1.52.1's Windows codes, negated.
var winErrnoNumbers = map[string]int{
	"E2BIG":           4093,
	"EACCES":          4092,
	"EADDRINUSE":      4091,
	"EADDRNOTAVAIL":   4090,
	"EAFNOSUPPORT":    4089,
	"EAGAIN":          4088,
	"EALREADY":        4084,
	"EBADF":           4083,
	"EBUSY":           4082,
	"ECANCELED":       4081,
	"ECHARSET":        4080,
	"ECONNABORTED":    4079,
	"ECONNREFUSED":    4078,
	"ECONNRESET":      4077,
	"EDESTADDRREQ":    4076,
	"EEXIST":          4075,
	"EFAULT":          4074,
	"EHOSTUNREACH":    4073,
	"EINTR":           4072,
	"EINVAL":          4071,
	"EIO":             4070,
	"EISCONN":         4069,
	"EISDIR":          4068,
	"ELOOP":           4067,
	"EMFILE":          4066,
	"EMSGSIZE":        4065,
	"ENAMETOOLONG":    4064,
	"ENETDOWN":        4063,
	"ENETUNREACH":     4062,
	"ENFILE":          4061,
	"ENOBUFS":         4060,
	"ENODEV":          4059,
	"ENOENT":          4058,
	"ENOMEM":          4057,
	"ENONET":          4056,
	"ENOSPC":          4055,
	"ENOSYS":          4054,
	"ENOTCONN":        4053,
	"ENOTDIR":         4052,
	"ENOTEMPTY":       4051,
	"ENOTSOCK":        4050,
	"ENOTSUP":         4049,
	"EPERM":           4048,
	"EPIPE":           4047,
	"EPROTO":          4046,
	"EPROTONOSUPPORT": 4045,
	"EPROTOTYPE":      4044,
	"EROFS":           4043,
	"ESHUTDOWN":       4042,
	"ESPIPE":          4041,
	"ESRCH":           4040,
	"ETIMEDOUT":       4039,
	"ETXTBSY":         4038,
	"EXDEV":           4037,
	"EFBIG":           4036,
	"ENOPROTOOPT":     4035,
	"ERANGE":          4034,
	"ENXIO":           4033,
	"EMLINK":          4032,
	"EHOSTDOWN":       4031,
	"EREMOTEIO":       4030,
	"ENOTTY":          4029,
	"EFTYPE":          4028,
	"EILSEQ":          4027,
	"EOVERFLOW":       4026,
	"ESOCKTNOSUPPORT": 4025,
	"ENODATA":         4024,
	"EUNATCH":         4023,
	"ENOEXEC":         4022,
	"EOF":             4095,
	"UNKNOWN":         4094,
}
