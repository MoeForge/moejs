package node

import (
	"errors"
	"io/fs"
	"math"
	"strings"
	"syscall"

	"github.com/MoeForge/moejs/engine"
)

// sysError is the Go side of a Node.js system error: what a file system
// operation failed with, turned into an Error on the loop (jsError). Its
// message is Node's: `ENOENT: no such file or directory, open 'a.txt'`,
// with ` -> 'dest'` for the operations of two paths.
type sysError struct {
	code    string // "ENOENT"
	syscall string // "open"
	path    string // the path as JavaScript gave it
	dest    string
	hasPath bool
	hasDest bool
}

func (e *sysError) Error() string { return e.message() }

func (e *sysError) message() string {
	var b strings.Builder
	b.WriteString(e.code)
	b.WriteString(": ")
	b.WriteString(errDesc(e.code))
	b.WriteString(", ")
	b.WriteString(e.syscall)
	if e.hasPath {
		b.WriteString(" '")
		b.WriteString(e.path)
		b.WriteByte('\'')
	}
	if e.hasDest {
		b.WriteString(" -> '")
		b.WriteString(e.dest)
		b.WriteByte('\'')
	}
	return b.String()
}

// errEscape is the error of a path that leaves the root, or names a place
// outside the mount: EACCES.
var errEscape = errors.New("path escapes the root")

// errReadOnly is the error of a write to a file system without Write:
// EROFS.
var errReadOnly = errors.New("read-only file system")

// errClosed is the error of an operation after Close: EBADF.
var errClosed = errors.New("file system closed")

// errnoCode returns the Node.js code of err, a Go error of the file
// system or of the Allow hook.
func errnoCode(err error) string {
	var en syscall.Errno
	switch {
	case errors.Is(err, errEscape):
		return "EACCES"
	case errors.Is(err, errReadOnly):
		return "EROFS"
	case errors.Is(err, errClosed), errors.Is(err, fs.ErrClosed):
		return "EBADF"
	case errors.As(err, &en):
		if c := errnoName(en); c != "" {
			return c
		}
		return "UNKNOWN"
	case errors.Is(err, fs.ErrNotExist):
		return "ENOENT"
	case errors.Is(err, fs.ErrExist):
		return "EEXIST"
	case errors.Is(err, fs.ErrPermission):
		return "EACCES"
	}
	// os.Root fails a path that a link or ".." takes out of it with an
	// error of its own.
	return "EACCES"
}

// isEscape reports whether err is os.Root's refusal of a path that leaves
// it, which a link with an absolute target inside the root also gets.
func isEscape(err error) bool {
	if err == nil {
		// Before en, which errors.As makes escape: no allocation on the
		// common path.
		return false
	}
	var en syscall.Errno
	if errors.As(err, &en) || errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrExist) {
		return false
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err != nil && strings.Contains(pe.Err.Error(), "escapes")
	}
	var le *linkError
	if errors.As(err, &le) {
		return isEscape(le.err)
	}
	return strings.Contains(err.Error(), "escapes")
}

type linkError struct{ err error }

func (e *linkError) Error() string { return e.err.Error() }
func (e *linkError) Unwrap() error { return e.err }

// newSysError returns the system error of err for syscall on path.
func newSysError(err error, syscall, path string) *sysError {
	var se *sysError
	if errors.As(err, &se) {
		return se
	}
	return &sysError{code: errnoCode(err), syscall: syscall, path: path, hasPath: true}
}

// newSysError2 returns the system error of err for syscall from path to
// dest.
func newSysError2(err error, syscall, path, dest string) *sysError {
	var se *sysError
	if errors.As(err, &se) {
		return se
	}
	return &sysError{code: errnoCode(err), syscall: syscall, path: path, dest: dest, hasPath: true, hasDest: true}
}

// errDescs are libuv's descriptions of the codes (uv_strerror).
var errDescs = map[string]string{
	"E2BIG":        "argument list too long",
	"EACCES":       "permission denied",
	"EAGAIN":       "resource temporarily unavailable",
	"EBADF":        "bad file descriptor",
	"EBUSY":        "resource busy or locked",
	"ECANCELED":    "operation canceled",
	"EEXIST":       "file already exists",
	"EFAULT":       "bad address in system call argument",
	"EFBIG":        "file too large",
	"EINTR":        "interrupted system call",
	"EINVAL":       "invalid argument",
	"EIO":          "i/o error",
	"EISDIR":       "illegal operation on a directory",
	"ELOOP":        "too many symbolic links encountered",
	"EMFILE":       "too many open files",
	"EMLINK":       "too many links",
	"ENAMETOOLONG": "name too long",
	"ENFILE":       "file table overflow",
	"ENODEV":       "no such device",
	"ENOENT":       "no such file or directory",
	"ENOMEM":       "not enough memory",
	"ENOSPC":       "no space left on device",
	"ENOSYS":       "function not implemented",
	"ENOTDIR":      "not a directory",
	"ENOTEMPTY":    "directory not empty",
	"ENOTSUP":      "operation not supported on socket",
	"ENXIO":        "no such device or address",
	"EPERM":        "operation not permitted",
	"EROFS":        "read-only file system",
	"ESPIPE":       "invalid seek",
	"ETXTBSY":      "text file is busy",
	"EXDEV":        "cross-device link not permitted",
	"EDQUOT":       "disk quota exceeded",
}

func errDesc(code string) string {
	if d, ok := errDescs[code]; ok {
		return d
	}
	return "unknown error"
}

// --- JavaScript errors ------------------------------------------------------------

// jsError returns the JavaScript value of a Go error of an operation: a
// Node.js Error for a *sysError, a RangeError for a *tooLargeError, and
// what JavaScript would catch for any other.
func (e *env) jsError(err error) (engine.Value, error) {
	var se *sysError
	if errors.As(err, &se) {
		return e.sysErrorValue(se), nil
	}
	var tl *tooLargeError
	if errors.As(err, &tl) {
		return e.codedError(engine.KindRangeError, "ERR_FS_FILE_TOO_LARGE", tl.Error()), nil
	}
	var ta *tooLargeAlone
	if errors.As(err, &ta) {
		return e.r.NewErrorValue(engine.KindRangeError, engine.FromGoString(ta.Error())), nil
	}
	var ge *globError
	if errors.As(err, &ge) {
		return e.r.NewErrorValue(engine.KindTypeError, engine.FromGoString(ge.msg)), nil
	}
	var sy *systemError
	if errors.As(err, &sy) {
		return e.systemErrorValue(sy), nil
	}
	var ce *codedError
	if errors.As(err, &ce) {
		return e.codedError(ce.kind, ce.code, ce.msg), nil
	}
	if v, ok := e.r.ThrownValue(err); ok {
		return v, nil
	}
	return undefined, err
}

// throw returns the error of a synchronous function that failed with err.
func (e *env) throw(err error) error {
	v, err2 := e.jsError(err)
	if err2 != nil {
		return err2
	}
	return e.r.Throw(v)
}

// sysErrorValue creates the Error of se, with Node's own enumerable
// properties errno, code, syscall, path and dest, in that order.
func (e *env) sysErrorValue(se *sysError) engine.Value {
	r := e.r
	o := r.NewErrorValue(engine.KindError, engine.FromGoString(se.message())).AsObject()
	k := e.keys
	o.DefineOwnDataFast(r, k.errno, engine.IntValue(-errnoNumber(se.code)), attrMethod)
	o.DefineOwnDataFast(r, k.code, str(se.code), attrMethod)
	o.DefineOwnDataFast(r, k.syscall, str(se.syscall), attrMethod)
	if se.hasPath {
		o.DefineOwnDataFast(r, k.path, str(se.path), attrMethod)
	}
	if se.hasDest {
		o.DefineOwnDataFast(r, k.dest, str(se.dest), attrMethod)
	}
	return engine.ObjectValue(o)
}

// codedError is an error of Node.js's own, with a code such as
// ERR_INVALID_ARG_TYPE: a TypeError, a RangeError or an Error.
type codedError struct {
	kind engine.ErrorKind
	code string
	msg  string
}

func (c *codedError) Error() string { return c.msg }

// codedError creates the error of kind with Node's code and message.
func (e *env) codedError(kind engine.ErrorKind, code, msg string) engine.Value {
	r := e.r
	o := r.NewErrorValue(kind, engine.FromGoString(msg)).AsObject()
	o.DefineOwnDataFast(r, e.keys.code, str(code), attrMethod)
	return engine.ObjectValue(o)
}

// throwCoded throws the error of kind with Node's code and message.
func (e *env) throwCoded(kind engine.ErrorKind, code, msg string) error {
	return e.r.Throw(e.codedError(kind, code, msg))
}

// argTypeError throws Node's ERR_INVALID_ARG_TYPE: `The "name" argument
// must be <expected>. Received <v>`.
func (e *env) argTypeError(name, expected string, v engine.Value) error {
	subject := `"` + name + `" argument`
	switch {
	case strings.HasSuffix(name, " argument"):
		subject = name
	case strings.Contains(name, "."):
		subject = `"` + name + `" property`
	}
	return e.throwCoded(engine.KindTypeError, "ERR_INVALID_ARG_TYPE",
		"The "+subject+" must be "+expected+". Received "+e.received(v))
}

// argValueError throws Node's ERR_INVALID_ARG_VALUE.
func (e *env) argValueError(name string, v engine.Value, reason string) error {
	what := "argument"
	if strings.Contains(name, ".") {
		what = "property"
	}
	return e.throwCoded(engine.KindTypeError, "ERR_INVALID_ARG_VALUE",
		"The "+what+" '"+name+"' "+reason+". Received "+e.inspectShort(v))
}

// outOfRange throws Node's ERR_OUT_OF_RANGE.
func (e *env) outOfRange(name, rng string, v engine.Value) error {
	return e.throwCoded(engine.KindRangeError, "ERR_OUT_OF_RANGE",
		"The value of \""+name+"\" is out of range. It must be "+rng+". Received "+e.inspectNumber(v))
}

// received describes v as Node's errors do (determineSpecificType).
func (e *env) received(v engine.Value) string {
	r := e.r
	switch {
	case v.IsUndefined():
		return "undefined"
	case v.IsNull():
		return "null"
	case v.IsString():
		s := v.AsString().GoString()
		if v.AsString().Len() > 28 {
			s = v.AsString().Substring(0, 25).GoString() + "..."
		}
		if !strings.Contains(s, "'") {
			return "type string ('" + s + "')"
		}
		return "type string (" + quoteJSON(s) + ")"
	case v.IsNumber():
		return "type number (" + numberString(v.AsNumber()) + ")"
	case v.IsBool():
		if v.AsBool() {
			return "type boolean (true)"
		}
		return "type boolean (false)"
	case v.IsBigInt():
		return "type bigint (" + r.DisplayString(v) + "n)"
	case v.IsSymbol():
		return "type symbol (" + r.DisplayString(v) + ")"
	case v.IsObject():
		o := v.AsObject()
		if o.IsCallable() {
			name, _ := o.GetProp(r, e.keys.name)
			if name.IsString() {
				return "function " + name.AsString().GoString()
			}
			return "function "
		}
		if c, err := o.GetProp(r, e.keys.constructor); err == nil && c.IsObject() {
			if name, err := c.AsObject().GetProp(r, e.keys.name); err == nil && name.IsString() {
				return "an instance of " + name.AsString().GoString()
			}
		}
		return r.DisplayString(v)
	}
	return r.DisplayString(v)
}

// inspectShort formats v as util.inspect does for the values of
// ERR_INVALID_ARG_VALUE: a string in single quotes.
func (e *env) inspectShort(v engine.Value) string {
	if v.IsString() {
		return quoteInspect(v.AsString().GoString())
	}
	if v.IsObject() && e.bufferCls != nil && v.AsObject().Class() == engine.ClassTypedArray {
		if ok, _ := e.r.InstanceOf(v, engine.ObjectValue(e.bufferCls.ctor)); ok {
			if s, err := e.bufferInspect(e.r, v, nil); err == nil && s.IsString() {
				return s.AsString().GoString()
			}
		}
	}
	if v.IsNumber() {
		return numberString(v.AsNumber())
	}
	return e.r.DisplayString(v)
}

// inspectNumber formats a number as Node's ERR_OUT_OF_RANGE does: an
// integer beyond 2**32 with underscores between its thousands, by Node's
// own addNumericalSeparator, which works on the string ("1e_+21").
func (e *env) inspectNumber(v engine.Value) string {
	if !v.IsNumber() {
		return e.inspectShort(v)
	}
	f := v.AsNumber()
	s := numberString(f)
	if f == math.Trunc(f) && !math.IsInf(f, 0) && math.Abs(f) > 1<<32 {
		return addNumericalSeparator(s)
	}
	return s
}

func addNumericalSeparator(val string) string {
	res := ""
	i := len(val)
	start := 0
	if strings.HasPrefix(val, "-") {
		start = 1
	}
	for ; i >= start+4; i -= 3 {
		res = "_" + val[i-3:i] + res
	}
	return val[:i] + res
}

func numberString(f float64) string {
	if f == 0 && 1/f < 0 {
		return "-0"
	}
	return engine.NumberToGoString(f)
}

// quoteInspect quotes s as util.inspect quotes a string.
func quoteInspect(s string) string {
	q := byte('\'')
	if strings.Contains(s, "'") {
		q = '"'
		if strings.Contains(s, "\"") {
			q = '`'
			if strings.Contains(s, "`") || strings.Contains(s, "${") {
				q = '\''
			}
		}
	}
	var b strings.Builder
	b.WriteByte(q)
	for _, c := range s {
		switch {
		case c == rune(q) || c == '\\':
			b.WriteByte('\\')
			b.WriteRune(c)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\t':
			b.WriteString(`\t`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\b':
			b.WriteString(`\b`)
		case c == '\f':
			b.WriteString(`\f`)
		case c == '\v':
			b.WriteString(`\v`)
		case c < 0x20 || c == 0x7f:
			const hex = "0123456789ABCDEF"
			b.WriteString(`\x`)
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		default:
			b.WriteRune(c)
		}
	}
	b.WriteByte(q)
	return b.String()
}

func quoteJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, c := range s {
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteRune(c)
		case c == '\n':
			b.WriteString(`\n`)
		case c < 0x20:
			const hex = "0123456789abcdef"
			b.WriteString(`\u00`)
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		default:
			b.WriteRune(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// systemError is one of Node's SystemErrors: ERR_FS_EISDIR of rm and
// ERR_FS_CP_EEXIST of cp.
type systemError struct {
	code, prefix  string // "ERR_FS_EISDIR", "Path is a directory"
	errCode, desc string // "EISDIR", "is a directory"
	syscall, path string
}

func (x *systemError) Error() string {
	return x.prefix + ": " + x.syscall + " returned " + x.errCode + " (" + x.desc + ") " + x.path
}

// systemErrorValue creates the SystemError of x.
func (e *env) systemErrorValue(x *systemError) engine.Value {
	r := e.r
	o := r.NewErrorValue(engine.KindError, engine.FromGoString(x.Error())).AsObject()
	k := e.keys
	errno := engine.IntValue(errnoNumber(x.errCode))
	o.DefineOwnDataFast(r, k.name, str("SystemError"), attrW|attrC)
	info := r.NewObject()
	info.DefineOwnDataFast(r, k.code, str(x.errCode), attrMethod)
	info.DefineOwnDataFast(r, k.message, str(x.desc), attrMethod)
	info.DefineOwnDataFast(r, k.path, str(x.path), attrMethod)
	info.DefineOwnDataFast(r, k.syscall, str(x.syscall), attrMethod)
	info.DefineOwnDataFast(r, k.errno, errno, attrMethod)
	o.DefineOwnDataFast(r, k.code, str(x.code), attrMethod)
	o.DefineOwnDataFast(r, k.info, engine.ObjectValue(info), attrMethod)
	o.DefineOwnDataFast(r, k.errno, errno, attrMethod)
	o.DefineOwnDataFast(r, k.syscall, str(x.syscall), attrMethod)
	o.DefineOwnDataFast(r, k.path, str(x.path), attrMethod)
	return engine.ObjectValue(o)
}
