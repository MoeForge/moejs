package node

import (
	"errors"
	"net/url"
	"strings"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
)

// callMode is the form a file system function was called in.
type callMode uint8

const (
	modeSync     callMode = iota // readFileSync: on the loop, the result returned
	modePromise                  // fs.promises.readFile: a promise
	modeCallback                 // fs.readFile: the callback called
)

// call is how a file system function was called: its form, and the
// callback of the callback form.
type call struct {
	mode callMode
	cb   engine.Value
	// hold, when set, is what the work takes of the memory budget (or
	// charges, in the synchronous form) for what it hands over: run
	// releases it on every path, once the result is charged on the loop,
	// when the work fails, and when the loop drops the result.
	hold *holding
}

// run does work as the call's form does it: at once on the loop for the
// synchronous form, on a worker of the pool for the others. done converts
// the result on the loop: the synchronous form returns its value, the
// promise form settles with it, and the callback form passes it to the
// callback after a null error. An error of work becomes the JavaScript
// error (jsError) that the form throws, rejects with, or passes. Only
// done may call JavaScript and allocate.
func run[T any](e *env, c call, work func() (T, error), done func(T) (engine.Value, error)) (engine.Value, error) {
	if c.mode == modeSync {
		if e.isClosed() {
			return undefined, e.throw(closedError())
		}
		res, err := work()
		if err != nil {
			return undefined, e.throw(err)
		}
		return done(res)
	}
	var settle func(err error, v engine.Value) error
	var result engine.Value
	if c.mode == modePromise {
		p := e.r.NewHostPromise()
		result = engine.ObjectValue(p)
		settle = func(err error, v engine.Value) error {
			if err != nil {
				reason, jerr := e.jsError(err)
				if jerr != nil {
					// An interrupt leaves the promise pending.
					return jerr
				}
				return e.r.SettlePromise(p, reason, true)
			}
			return e.r.SettlePromise(p, v, false)
		}
	} else {
		result = undefined
		settle = func(err error, v engine.Value) error { return e.callback(c.cb, err, v) }
	}
	finish := e.start(c.cb, result, func() {
		// The loop is over: it settles nothing more.
		c.hold.done()
		e.budget.dropReady()
	})
	epoch := e.epoch.Load()
	e.pool.submit(job{
		run: func() {
			if e.halted.Load() {
				// Close ran since: the operation fails as a dropped
				// one does.
				finish(func() error {
					defer c.hold.done()
					return settle(closedError(), undefined)
				})
				return
			}
			if e.r.CheckInterrupt() != nil {
				e.epoch.Add(1)
			}
			if e.epoch.Load() != epoch {
				// The program was interrupted since it asked: its
				// work is dropped, not done, and its promise stays
				// pending, as the interrupt leaves promises.
				finish(func() error { return nil })
				return
			}
			res, err := work()
			if err != nil {
				c.hold.done()
			} else {
				c.hold.finished()
			}
			finish(func() error {
				defer c.hold.done()
				if err != nil {
					var ie *moejs.InterruptedError
					if errors.As(err, &ie) && e.r.CheckInterrupt() == nil {
						// The work stopped at an interrupt that the host
						// cleared since: the program it was for was
						// stopped, and its result is dropped, as the
						// interrupt leaves a promise pending.
						return nil
					}
					return settle(err, undefined)
				}
				v, derr := done(res)
				if derr != nil {
					if _, ok := e.r.ThrownValue(derr); !ok {
						return derr
					}
					return settle(derr, undefined)
				}
				return settle(nil, v)
			})
		},
		drop: func() {
			finish(func() error { return settle(closedError(), undefined) })
		},
	})
	return result, nil
}

// start begins an operation in flight, on the loop: it holds the loop and
// lists what JavaScript waits on, the callback or the promise. The
// function it returns posts the task that ends the operation, from any
// goroutine, and releases the hold; dropped runs when the loop is over
// and will not run the task.
func (e *env) start(cb, p engine.Value, dropped func()) func(task func() error) {
	keep := p
	if isFunction(cb) {
		keep = cb
	}
	op := e.track(keep)
	release := e.loop.Hold()
	return func(task func() error) {
		posted := e.post(func() error {
			// What the task charges may measure: what it settles is
			// still listed then.
			err := task()
			e.untrack(op)
			return err
		})
		if !posted && dropped != nil {
			dropped()
		}
		release()
	}
}

// callback calls cb with err's JavaScript error, or with null and v.
func (e *env) callback(cb engine.Value, err error, v engine.Value) error {
	if err != nil {
		reason, jerr := e.jsError(err)
		if jerr != nil {
			return jerr
		}
		_, err = e.r.Call(cb, undefined, []engine.Value{reason})
		return err
	}
	_, err = e.r.Call(cb, undefined, []engine.Value{engine.Null(), v})
	return err
}

// post runs task on the loop with the jobs held until it returns, as
// webapi's post does, so that the reactions of the promises it settles
// run after it. A Go panic in it is an *moejs.InternalError. It reports
// false once the loop is over.
func (e *env) post(task func() error) bool {
	return e.loop.Post(func(*moejs.Runtime) (err error) {
		r := e.r
		saved := r.CallState()
		defer func() {
			if x := recover(); x != nil {
				r.RestoreCallState(saved)
				err = &moejs.InternalError{Value: x}
				r.DropJobs(err)
			}
		}()
		r.HoldJobs()
		return r.ReleaseJobs(task())
	})
}

// isClosed reports whether Close ran: halted, which Close sets with
// closed, without the lock.
func (e *env) isClosed() bool { return e.halted.Load() }

// closedError is the error of an operation after Close.
func closedError() error { return &sysError{code: "EBADF", syscall: "open"} }

// --- arguments ----------------------------------------------------------------------

// pathArg returns the path v names: a string, the UTF-8 of a Buffer or
// Uint8Array, or the path of a file: URL, as Node's getValidatedPath
// reads it.
func (e *env) pathArg(v engine.Value, name string) (string, error) {
	var p string
	switch {
	case v.IsString():
		p = v.AsString().GoString()
	case v.IsObject():
		o := v.AsObject()
		if b, ok := o.BufferData(); ok && o.Class() == engine.ClassTypedArray && e.isUint8Array(o) {
			p = string(b)
			break
		}
		var ok bool
		var err error
		if p, ok, err = e.fileURLPath(v, name); err != nil {
			return "", err
		} else if !ok {
			return "", e.argTypeError(name, "of type string or an instance of Buffer or URL", v)
		}
	default:
		return "", e.argTypeError(name, "of type string or an instance of Buffer or URL", v)
	}
	if strings.IndexByte(p, 0) >= 0 {
		return "", e.throwCoded(engine.KindTypeError, "ERR_INVALID_ARG_VALUE",
			"The argument '"+name+"' must be a string, Uint8Array, or URL without null bytes. Received "+e.inspectShort(v))
	}
	return p, nil
}

// isUint8Array reports whether the typed array o is a Uint8Array, a Buffer
// included.
func (e *env) isUint8Array(o *engine.Object) bool {
	elems, ok := o.TypedArrayElements()
	if !ok {
		return false
	}
	if _, ok = elems.([]uint8); !ok {
		return false
	}
	// A Uint8ClampedArray has []uint8 elements too: its prototype chain
	// tells it apart, as Node's isUint8Array does by its class.
	if c := e.intrinsics().uint8ClampedProto; c != nil {
		for p := o.Proto(); p != nil; p = p.Proto() {
			if p == c {
				return false
			}
		}
	}
	return true
}

// fileURLPath returns the path of v when it is a URL object (one with
// href and protocol, as Node's isURL tests), checked as
// fileURLToPath checks it.
func (e *env) fileURLPath(v engine.Value, name string) (string, bool, error) {
	r := e.r
	o := v.AsObject()
	href, err := o.GetProp(r, e.keys.href)
	if err != nil || !href.IsString() {
		return "", false, err
	}
	proto, err := o.GetProp(r, e.keys.protocol)
	if err != nil || proto.IsUndefined() {
		return "", false, err
	}
	if !proto.IsString() || proto.AsString().GoString() != "file:" {
		return "", true, e.throwCoded(engine.KindTypeError, "ERR_INVALID_URL_SCHEME", "The URL must be of scheme file")
	}
	u, perr := url.Parse(href.AsString().GoString())
	if perr != nil {
		return "", true, e.throwCoded(engine.KindTypeError, "ERR_INVALID_URL", "Invalid URL")
	}
	if u.Host != "" && u.Host != "localhost" {
		return "", true, e.throwCoded(engine.KindTypeError, "ERR_INVALID_FILE_URL_HOST",
			`File URL host must be "localhost" or empty on `+platform())
	}
	raw := u.EscapedPath()
	if strings.Contains(strings.ToLower(raw), "%2f") {
		return "", true, e.throwCoded(engine.KindTypeError, "ERR_INVALID_FILE_URL_PATH",
			"File URL path must not include encoded / characters")
	}
	return u.Path, true, nil
}

// fsPath reads the path argument v.
func (e *env) fsPath(v engine.Value, name string) (string, error) { return e.pathArg(v, name) }

// isFunction reports whether v is callable.
func isFunction(v engine.Value) bool { return engine.IsCallable(v) }

// getOptions implements Node's getOptions: options may be undefined,
// null, a function (the callback: the defaults), a string (the encoding)
// or an object. It returns the object (undefined when there is none) and
// the encoding, def when it gives none.
func (e *env) getOptions(v engine.Value, def encoding) (engine.Value, encoding, error) {
	switch {
	case v.IsNullish() || isFunction(v):
		return undefined, def, nil
	case v.IsString():
		enc, err := e.encodingArg(v, def)
		return undefined, enc, err
	case !v.IsObject():
		return undefined, def, e.throwCoded(engine.KindTypeError, "ERR_INVALID_ARG_TYPE",
			`The "options" argument must be one of type string or object. Received `+e.received(v))
	}
	ev, err := v.AsObject().GetProp(e.r, e.keys.encoding)
	if err != nil {
		return undefined, def, err
	}
	enc, err := e.encodingArg(ev, def)
	return v, enc, err
}

// encodingArg reads an encoding: undefined, null or "" is def, "buffer"
// is a Buffer.
func (e *env) encodingArg(v engine.Value, def encoding) (encoding, error) {
	if v.IsNullish() || (v.IsString() && v.AsString().Len() == 0) || (v.IsBool() && !v.AsBool()) ||
		(v.IsNumber() && (v.AsNumber() == 0 || v.AsNumber() != v.AsNumber())) {
		return def, nil
	}
	if v.IsString() {
		name := v.AsString().GoString()
		if name == "buffer" {
			return encBuffer, nil
		}
		if enc, ok := parseEncoding(name); ok {
			return enc, nil
		}
	}
	return def, e.argValueError("encoding", v, "is invalid encoding")
}

// optValue reads options[key]; undefined without options.
func (e *env) optValue(opts engine.Value, key engine.PropertyKey) (engine.Value, error) {
	if !opts.IsObject() {
		return undefined, nil
	}
	return opts.AsObject().GetProp(e.r, key)
}

// optBool reads a boolean option, which must be a boolean when present.
func (e *env) optBool(opts engine.Value, key engine.PropertyKey, name string, def bool) (bool, error) {
	v, err := e.optValue(opts, key)
	if err != nil || v.IsNullish() {
		return def, err
	}
	if !v.IsBool() {
		return def, e.argTypeError("options."+name, "of type boolean", v)
	}
	return v.AsBool(), nil
}

// optTruthy reads an option as Node's code reads one it does not
// validate: by its truthiness.
func (e *env) optTruthy(opts engine.Value, key engine.PropertyKey) (bool, error) {
	v, err := e.optValue(opts, key)
	if err != nil {
		return false, err
	}
	return engine.ToBoolean(v), nil
}

// modeArg parses a file mode: a 32-bit unsigned integer or an octal
// string; undefined is def.
func (e *env) modeArg(v engine.Value, name string, def uint32) (uint32, error) {
	if v.IsUndefined() {
		return def, nil
	}
	if v.IsString() {
		s := v.AsString().GoString()
		var m uint32
		ok := s != ""
		for _, c := range s {
			if c < '0' || c > '7' || m > 0x1fffffff {
				ok = false
				break
			}
			m = m<<3 | uint32(c-'0')
		}
		if !ok {
			return 0, e.argValueError(name, v, "must be a 32-bit unsigned integer or an octal string")
		}
		return m, nil
	}
	if !v.IsNumber() {
		return 0, e.argTypeError(name, "of type number", v)
	}
	f := v.AsNumber()
	if f != float64(int64(f)) {
		return 0, e.outOfRangeInt(name, "an integer", v)
	}
	if f < 0 || f > 0xffffffff {
		return 0, e.outOfRange(name, ">= 0 && <= 4294967295", v)
	}
	return uint32(f), nil
}

func (e *env) outOfRangeInt(name, rng string, v engine.Value) error {
	return e.outOfRange(name, rng, v)
}

// cbArgs splits the arguments of a callback form: the callback is the
// first function at or after index fixed, and at most opt arguments come
// before it after the fixed ones. It fails as Node's makeCallback does
// when there is none.
func (e *env) cbArgs(args []engine.Value, fixed, opt int) ([]engine.Value, engine.Value, error) {
	for i := fixed; i <= fixed+opt && i < len(args); i++ {
		if isFunction(args[i]) {
			return args[:i], args[i], nil
		}
	}
	last := undefined
	if n := min(len(args), fixed+opt+1); n > 0 {
		last = args[n-1]
	}
	return nil, undefined, e.argTypeError("cb", "of type function", last)
}

// --- the modules --------------------------------------------------------------------

// fsFunc is a function of node:fs: impl does it for each form; fixed and
// opt are the counts of required and optional arguments before the
// callback of the callback form.
type fsFunc struct {
	name       string
	length     int // of the synchronous and promise forms
	fixed, opt int
	impl       func(e *env, c call, args []engine.Value) (engine.Value, error)
	noSync     bool // exists has its own synchronous form
}

// fsFuncs are the functions of node:fs, each with its three forms.
var fsFuncs = []fsFunc{
	{name: "access", length: 2, fixed: 1, opt: 1, impl: (*env).accessOp},
	{name: "appendFile", length: 3, fixed: 2, opt: 1, impl: (*env).appendFileOp},
	{name: "copyFile", length: 3, fixed: 2, opt: 1, impl: (*env).copyFileOp},
	{name: "cp", length: 3, fixed: 2, opt: 1, impl: (*env).cpOp},
	{name: "glob", length: 2, fixed: 1, opt: 1, impl: (*env).globOp},
	{name: "lstat", length: 2, fixed: 1, opt: 1, impl: (*env).lstatOp},
	{name: "mkdir", length: 2, fixed: 1, opt: 1, impl: (*env).mkdirOp},
	{name: "readFile", length: 2, fixed: 1, opt: 1, impl: (*env).readFileOp},
	{name: "readdir", length: 2, fixed: 1, opt: 1, impl: (*env).readdirOp},
	{name: "readlink", length: 2, fixed: 1, opt: 1, impl: (*env).readlinkOp},
	{name: "realpath", length: 2, fixed: 1, opt: 1, impl: (*env).realpathOp},
	{name: "rename", length: 2, fixed: 2, opt: 0, impl: (*env).renameOp},
	{name: "rm", length: 2, fixed: 1, opt: 1, impl: (*env).rmOp},
	{name: "rmdir", length: 2, fixed: 1, opt: 1, impl: (*env).rmdirOp},
	{name: "stat", length: 2, fixed: 1, opt: 1, impl: (*env).statOp},
	{name: "unlink", length: 1, fixed: 1, opt: 0, impl: (*env).unlinkOp},
	{name: "writeFile", length: 3, fixed: 2, opt: 1, impl: (*env).writeFileOp},
}

// fsModule returns the binding of node:fs, its default export: the
// functions, promises, constants, Stats and Dirent, and the functions it
// does not implement, which throw.
func (e *env) fsModule() *engine.Object {
	if e.fsObj != nil {
		return e.fsObj
	}
	r := e.r
	o := r.NewObject()
	// Its hundred functions keep a shape, so that fs.name() keeps its
	// inline cache.
	o.KeepShape()
	e.fsObj = o
	for _, f := range fsFuncs {
		f := f
		e.method(o, f.name, f.length+1, attrMethod, func(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
			args, cb, err := e.cbArgs(args, f.fixed, f.opt)
			if err != nil {
				return undefined, err
			}
			return f.impl(e, call{mode: modeCallback, cb: cb}, args)
		})
		e.method(o, f.name+"Sync", f.length, attrMethod, func(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
			return f.impl(e, call{mode: modeSync}, args)
		})
	}
	e.method(o, "exists", 2, attrMethod, e.existsCallback)
	e.method(o, "existsSync", 1, attrMethod, e.existsSync)
	e.set(o, "promises", engine.ObjectValue(e.promisesModule()), attrMethod)
	e.set(o, "constants", engine.ObjectValue(e.fsConstants()), attrMethod)
	e.statsClasses()
	e.direntClass()
	e.set(o, "Stats", engine.ObjectValue(e.statsCtor), attrMethod)
	e.set(o, "Dirent", engine.ObjectValue(e.direntCtor), attrMethod)
	for _, name := range fsMissing {
		e.set(o, name, engine.ObjectValue(e.notImplemented("fs", name)), attrMethod)
	}
	return o
}

// promisesModule returns the binding of node:fs/promises.
func (e *env) promisesModule() *engine.Object {
	if e.promisesObj != nil {
		return e.promisesObj
	}
	r := e.r
	o := r.NewObject()
	e.promisesObj = o
	for _, f := range fsFuncs {
		f := f
		e.method(o, f.name, f.length, attrMethod, func(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
			v, err := f.impl(e, call{mode: modePromise}, args)
			if err != nil {
				// Node's promise functions reject for a bad argument too.
				if reason, ok := r.ThrownValue(err); ok {
					p := r.NewHostPromise()
					cerr := r.SettlePromise(p, reason, true)
					if cerr == nil && f.name == "glob" {
						// Node's glob is an async generator: what it
						// throws, its next rejects with, and nothing
						// rejects until next is called.
						if cerr = e.handled(engine.ObjectValue(p)); cerr == nil {
							return e.asyncIterator(engine.ObjectValue(p)), nil
						}
					}
					return engine.ObjectValue(p), cerr
				}
				return undefined, err
			}
			return v, nil
		})
	}
	e.set(o, "constants", engine.ObjectValue(e.fsConstants()), attrMethod)
	for _, name := range promisesMissing {
		e.set(o, name, engine.ObjectValue(e.notImplemented("fsPromises", name)), attrMethod)
	}
	return o
}

// fsConstants returns fs.constants, Linux's values.
func (e *env) fsConstants() *engine.Object {
	if e.constants != nil {
		return e.constants
	}
	r := e.r
	o := r.NewObjectWithProto(nil)
	for _, c := range fsConstantValues {
		e.set(o, c.name, engine.IntValue(c.value), attrE)
	}
	e.constants = o
	return o
}

var fsConstantValues = []struct {
	name  string
	value int
}{
	{"UV_FS_SYMLINK_DIR", 1}, {"UV_FS_SYMLINK_JUNCTION", 2}, {"O_RDONLY", 0}, {"O_WRONLY", 1}, {"O_RDWR", 2},
	{"UV_DIRENT_UNKNOWN", 0}, {"UV_DIRENT_FILE", 1}, {"UV_DIRENT_DIR", 2}, {"UV_DIRENT_LINK", 3},
	{"UV_DIRENT_FIFO", 4}, {"UV_DIRENT_SOCKET", 5}, {"UV_DIRENT_CHAR", 6}, {"UV_DIRENT_BLOCK", 7},
	{"S_IFMT", 61440}, {"S_IFREG", 32768}, {"S_IFDIR", 16384}, {"S_IFCHR", 8192}, {"S_IFBLK", 24576},
	{"S_IFIFO", 4096}, {"S_IFLNK", 40960}, {"S_IFSOCK", 49152}, {"O_CREAT", 64}, {"O_EXCL", 128},
	{"UV_FS_O_FILEMAP", 0}, {"O_NOCTTY", 256}, {"O_TRUNC", 512}, {"O_APPEND", 1024}, {"O_DIRECTORY", 65536},
	{"O_NOATIME", 262144}, {"O_NOFOLLOW", 131072}, {"O_SYNC", 1052672}, {"O_DSYNC", 4096},
	{"O_DIRECT", 16384}, {"O_NONBLOCK", 2048}, {"S_IRWXU", 448}, {"S_IRUSR", 256}, {"S_IWUSR", 128},
	{"S_IXUSR", 64}, {"S_IRWXG", 56}, {"S_IRGRP", 32}, {"S_IWGRP", 16}, {"S_IXGRP", 8}, {"S_IRWXO", 7},
	{"S_IROTH", 4}, {"S_IWOTH", 2}, {"S_IXOTH", 1}, {"F_OK", 0}, {"R_OK", 4}, {"W_OK", 2}, {"X_OK", 1},
	{"UV_FS_COPYFILE_EXCL", 1}, {"COPYFILE_EXCL", 1}, {"UV_FS_COPYFILE_FICLONE", 2}, {"COPYFILE_FICLONE", 2},
	{"UV_FS_COPYFILE_FICLONE_FORCE", 4}, {"COPYFILE_FICLONE_FORCE", 4},
}
