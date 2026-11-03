package node

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"

	"github.com/MoeForge/moejs/engine"
)

// --- values -------------------------------------------------------------------------

// encodedSize estimates the bytes the string of n bytes in enc takes.
func encodedSize(enc encoding, b []byte) int {
	switch enc {
	case encUTF8, encLatin1:
		if isASCII(b) {
			return len(b)
		}
		return 2 * len(b)
	case encBase64, encBase64URL:
		return (len(b) + 2) / 3 * 4
	case encHex:
		return 2 * len(b)
	}
	return len(b)
}

// maxStringLength is moejs's longest string, in UTF-16 code units.
const maxStringLength = engine.MaxStringLength

// bytesValue converts data, read for JavaScript, to a Buffer (encBuffer)
// or a string of enc, charged to the memory limit first. owned says that
// nothing else holds data.
func (e *env) bytesValue(data []byte, enc encoding, owned bool) (engine.Value, error) {
	if enc == encBuffer {
		if err := e.r.ChargeMemory(len(data)); err != nil {
			return undefined, err
		}
		if !owned {
			data = append([]byte(nil), data...)
		}
		return e.newBuffer(data)
	}
	if enc == encUTF8 {
		// The common case: what ASCII text takes is charged first, and
		// the rest of what other text takes once it is decoded.
		if len(data) > maxStringLength {
			return undefined, e.throwCoded(engine.KindError, "ERR_STRING_TOO_LONG",
				"Cannot create a string longer than 0x3fffffe8 characters")
		}
		if err := e.r.ChargeMemory(len(data)); err != nil {
			return undefined, err
		}
		s := decodeUTF8(data, owned)
		if !s.IsASCII() {
			if err := e.r.ChargeMemory(2*s.Len() - len(data)); err != nil {
				return undefined, err
			}
		}
		return engine.StringValue(s), nil
	}
	// One scan tells ASCII text, which latin1 and ascii keep as it is.
	ascii := (enc == encLatin1 || enc == encASCII) && isASCII(data)
	n := len(data)
	if !ascii {
		n = encodedSize(enc, data)
	}
	if n > 2*maxStringLength || (n > maxStringLength && enc != encUTF8 && enc != encLatin1 && enc != encUTF16LE) {
		return undefined, e.throwCoded(engine.KindError, "ERR_STRING_TOO_LONG",
			"Cannot create a string longer than 0x3fffffe8 characters")
	}
	if err := e.r.ChargeMemory(n); err != nil {
		return undefined, err
	}
	if ascii {
		return engine.StringValue(asciiString(data, owned)), nil
	}
	return engine.StringValue(decode(enc, data, owned)), nil
}

// nameValue converts a file name to a string of enc, or a Buffer.
func (e *env) nameValue(name string, enc encoding) (engine.Value, error) {
	if enc == encUTF8 {
		return str(name), nil
	}
	return e.bytesValue([]byte(name), enc, true)
}

// --- flags --------------------------------------------------------------------------

// parseFlags converts Node's flags, a string such as "r" or "a+" or a
// number, to the open flags; writes reports whether they write.
func (e *env) parseFlags(v engine.Value, def string) (int, bool, error) {
	if v.IsNumber() {
		n := int(v.AsNumber())
		return n, n&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0, nil
	}
	if v.IsNullish() || v.IsString() {
		const (
			rd, wr, rw = os.O_RDONLY, os.O_WRONLY, os.O_RDWR
			cr, tr, ap = os.O_CREATE, os.O_TRUNC, os.O_APPEND
			ex, sy     = os.O_EXCL, os.O_SYNC
		)
		flag := def
		if v.IsString() {
			flag = v.AsString().GoString()
		}
		switch flag {
		case "r":
			return rd, false, nil
		case "rs", "sr":
			return rd | sy, false, nil
		case "r+":
			return rw, true, nil
		case "rs+", "sr+":
			return rw | sy, true, nil
		case "w":
			return tr | cr | wr, true, nil
		case "wx", "xw":
			return tr | cr | wr | ex, true, nil
		case "w+":
			return tr | cr | rw, true, nil
		case "wx+", "xw+":
			return tr | cr | rw | ex, true, nil
		case "a":
			return ap | cr | wr, true, nil
		case "ax", "xa":
			return ap | cr | wr | ex, true, nil
		case "as", "sa":
			return ap | cr | wr | sy, true, nil
		case "a+":
			return ap | cr | rw, true, nil
		case "ax+", "xa+":
			return ap | cr | rw | ex, true, nil
		case "as+", "sa+":
			return ap | cr | rw | sy, true, nil
		}
	}
	return 0, false, e.argValueError("flags", v, "is invalid")
}

// --- readFile, writeFile, appendFile ------------------------------------------------

func (e *env) readFileOp(c call, args []engine.Value) (engine.Value, error) {
	p, err := e.fsPath(engine.Arg(args, 0), "path")
	if err != nil {
		return undefined, err
	}
	opts, enc, err := e.getOptions(engine.Arg(args, 1), encBuffer)
	if err != nil {
		return undefined, err
	}
	fv, err := e.optValue(opts, e.keys.flag)
	if err != nil {
		return undefined, err
	}
	flags, writes, err := e.parseFlags(fv, "r")
	if err != nil {
		return undefined, err
	}
	limit := e.r.MemoryLimit()
	f := e.fsys
	if c.mode == modeSync {
		// run's synchronous form, without its closures and the holding,
		// which only the work on a worker uses.
		if e.isClosed() {
			return undefined, e.throw(closedError())
		}
		rel, err := f.rel(p)
		if err != nil {
			return undefined, e.throw(newSysError(err, "open", p))
		}
		data, err := f.readFile(rel, p, flags, writes, limit, nil)
		if err != nil {
			return undefined, e.throw(err)
		}
		return e.bytesValue(data, enc, true)
	}
	// A worker takes the file's size of the budget before it reads it,
	// so that the reads in flight hold at most the limit in all.
	hold := &holding{e: e, sync: c.mode == modeSync, what: "file"}
	var reserve func(int64) error
	if !hold.sync && e.budget != nil {
		reserve = func(n int64) error {
			if n == 0 {
				n = limit // a file that reports no size reads up to the limit
			}
			return hold.grow(int(min(n, limit)))
		}
	}
	c.hold = hold
	return run(e, c, func() ([]byte, error) {
		rel, err := f.rel(p)
		if err != nil {
			return nil, newSysError(err, "open", p)
		}
		return f.readFile(rel, p, flags, writes, limit, reserve)
	}, func(data []byte) (engine.Value, error) {
		return e.bytesValue(data, enc, true)
	})
}

// dataArg reads the data of writeFile: a string in enc, or the bytes of a
// TypedArray or DataView, copied when the work runs off the loop.
func (e *env) dataArg(v engine.Value, enc encoding, c call) ([]byte, error) {
	if v.IsString() {
		return encode(enc, v.AsString()), nil
	}
	if v.IsObject() {
		o := v.AsObject()
		if cls := o.Class(); cls == engine.ClassTypedArray || cls == engine.ClassDataView {
			b, _ := o.BufferData()
			if c.mode != modeSync {
				b = append([]byte(nil), b...)
			}
			return b, nil
		}
	}
	return nil, e.argTypeError("data", "of type string or an instance of Buffer, TypedArray, or DataView", v)
}

func (e *env) writeFileOp(c call, args []engine.Value) (engine.Value, error) {
	return e.writeOp(c, args, "w")
}

func (e *env) appendFileOp(c call, args []engine.Value) (engine.Value, error) {
	return e.writeOp(c, args, "a")
}

// writeOp implements writeFile and appendFile, whose default flag is
// defFlag.
func (e *env) writeOp(c call, args []engine.Value, defFlag string) (engine.Value, error) {
	p, err := e.fsPath(engine.Arg(args, 0), "path")
	if err != nil {
		return undefined, err
	}
	opts, enc, err := e.getOptions(engine.Arg(args, 2), encUTF8)
	if err != nil {
		return undefined, err
	}
	if enc == encBuffer {
		enc = encUTF8
	}
	data, err := e.dataArg(engine.Arg(args, 1), enc, c)
	if err != nil {
		return undefined, err
	}
	fv, err := e.optValue(opts, e.keys.flag)
	if err != nil {
		return undefined, err
	}
	flags, _, err := e.parseFlags(fv, defFlag)
	if err != nil {
		return undefined, err
	}
	mv, err := e.optValue(opts, e.keys.mode)
	if err != nil {
		return undefined, err
	}
	mode, err := e.modeArg(mv, "mode", 0o666)
	if err != nil {
		return undefined, err
	}
	f := e.fsys
	return run(e, c, func() (struct{}, error) {
		rel, err := f.rel(p)
		if err == nil {
			err = f.writeFile(rel, data, flags, fs.FileMode(mode&0o777))
		}
		if err != nil {
			return struct{}{}, newSysError(err, "open", p)
		}
		return struct{}{}, nil
	}, func(struct{}) (engine.Value, error) { return undefined, nil })
}

// --- readdir ------------------------------------------------------------------------

// listed is an entry of a recursive listing: dir is its directory,
// relative to the listed one ("" for that one).
type listed struct {
	dir string
	dirent
}

func (e *env) readdirOp(c call, args []engine.Value) (engine.Value, error) {
	p, err := e.fsPath(engine.Arg(args, 0), "path")
	if err != nil {
		return undefined, err
	}
	opts, enc, err := e.getOptions(engine.Arg(args, 1), encUTF8)
	if err != nil {
		return undefined, err
	}
	var recursive bool
	if c.mode == modePromise {
		recursive, err = e.optTruthy(opts, e.keys.recursive)
	} else {
		recursive, err = e.optBool(opts, e.keys.recursive, "recursive", false)
	}
	if err != nil {
		return undefined, err
	}
	withTypes, err := e.optTruthy(opts, e.keys.withFileTypes)
	if err != nil {
		return undefined, err
	}
	f := e.fsys
	lifo := c.mode == modePromise
	// The listing is charged, or held of the budget, as it grows.
	hold := &holding{e: e, sync: c.mode == modeSync, what: "listing"}
	c.hold = hold
	return run(e, c, func() ([]listed, error) {
		rel, err := f.rel(p)
		if err != nil {
			return nil, newSysError(err, "scandir", p)
		}
		var out []listed
		if !recursive {
			var ents []dirent
			if ents, err = f.readdirTypes(rel, withTypes); err != nil {
				return nil, newSysError(err, "scandir", p)
			}
			out = make([]listed, len(ents))
			for i, d := range ents {
				out[i] = listed{dirent: d}
			}
			err = hold.grow(listedSize(out))
		} else {
			out, err = f.readdirRecursive(rel, p, lifo, withTypes, hold.grow)
		}
		if err != nil {
			return nil, err
		}
		return out, nil
	}, func(ents []listed) (engine.Value, error) {
		return e.listingValue(p, ents, enc, withTypes, hold.sync)
	})
}

// readdirRecursive lists rel and the directories below it in Node's
// order: breadth first for the synchronous and callback forms, where a
// link to a directory is listed too; for the promise form (lifo), each
// directory's listing in turn as a stack pops them, where withTypes
// follows no link, as Node's own code does.
func (f *fsys) readdirRecursive(rel, p string, lifo, withTypes bool, grow func(int) error) ([]listed, error) {
	read := func(dir string) ([]dirent, error) {
		r := rel
		if dir != "" {
			r = rel + "/" + dir
		}
		ents, err := f.readdir(r)
		if err != nil {
			return nil, newSysError(err, "scandir", path.Join(p, dir))
		}
		// Each directory's part of the listing is charged, or held, as
		// it is read, and the runtime's interrupt and Close stop the
		// walk: links to directories are followed, as in Node.js, so
		// two links back to their own directory list 2^40 paths before
		// the system's link limit ends it.
		n := 0
		for _, d := range ents {
			n += len(d.name) + len(dir) + listedOverhead
		}
		if err := grow(n); err != nil {
			return nil, err
		}
		return ents, nil
	}
	isDir := func(dir string, d dirent) bool {
		if d.typ.IsDir() {
			return true
		}
		// ReadDir gave the type of each entry, lstat's when the system
		// did not: only a link may still lead to a directory.
		if d.typ&fs.ModeSymlink == 0 || lifo && withTypes {
			return false
		}
		return f.isDir(rel + "/" + path.Join(dir, d.name))
	}
	var out []listed
	if !lifo {
		queue := []string{""}
		for i := 0; i < len(queue); i++ {
			dir := queue[i]
			ents, err := read(dir)
			if err != nil {
				return nil, err
			}
			for _, d := range ents {
				out = append(out, listed{dir, d})
				if isDir(dir, d) {
					queue = append(queue, path.Join(dir, d.name))
				}
			}
		}
		return out, nil
	}
	type pending struct {
		dir  string
		ents []dirent
	}
	ents, err := read("")
	if err != nil {
		return nil, err
	}
	stack := []pending{{"", ents}}
	for len(stack) > 0 {
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, d := range top.ents {
			out = append(out, listed{top.dir, d})
			if isDir(top.dir, d) {
				sub := path.Join(top.dir, d.name)
				ents, err := read(sub)
				if err != nil {
					return nil, err
				}
				stack = append(stack, pending{sub, ents})
			}
		}
	}
	return out, nil
}

// listedOverhead is what an entry of a listing takes besides its names,
// for the charges of a listing as it grows.
const listedOverhead = 32

// listedSize is what a listing takes, as its walk charges it.
func listedSize(ents []listed) int {
	n := 0
	for _, d := range ents {
		n += len(d.name) + len(d.dir) + listedOverhead
	}
	return n
}

// listingValue converts a listing of the directory p: names, relative to
// p for a recursive one, or Dirents whose parentPath is p joined with
// their directory. charged says that the walk charged it already.
func (e *env) listingValue(p string, ents []listed, enc encoding, withTypes, charged bool) (engine.Value, error) {
	r := e.r
	if !charged {
		if err := r.ChargeMemory(listedSize(ents)); err != nil {
			return undefined, err
		}
	}
	items := make([]engine.Value, len(ents))
	pv := str(p)
	var lastDir string
	lastParent := pv
	for i, d := range ents {
		if withTypes {
			name, err := e.nameValue(d.name, enc)
			if err != nil {
				return undefined, err
			}
			parent := pv
			if d.dir != "" {
				if d.dir != lastDir {
					lastDir, lastParent = d.dir, str(path.Join(p, d.dir))
				}
				parent = lastParent
			}
			items[i] = e.newDirent(name, parent, direntType(d.typ))
			continue
		}
		name := d.name
		if d.dir != "" {
			name = d.dir + "/" + d.name
		}
		v, err := e.nameValue(name, enc)
		if err != nil {
			return undefined, err
		}
		items[i] = v
	}
	return engine.ObjectValue(r.NewArrayFromSlice(items)), nil
}

// --- stat, lstat, access, exists ----------------------------------------------------

func (e *env) statOp(c call, args []engine.Value) (engine.Value, error) {
	return e.statImpl(c, args, false)
}

func (e *env) lstatOp(c call, args []engine.Value) (engine.Value, error) {
	return e.statImpl(c, args, true)
}

func (e *env) statImpl(c call, args []engine.Value, lstat bool) (engine.Value, error) {
	p, err := e.fsPath(engine.Arg(args, 0), "path")
	if err != nil {
		return undefined, err
	}
	opts := engine.Arg(args, 1)
	if isFunction(opts) {
		opts = undefined
	}
	if big, err := e.optTruthy(opts, e.keys.bigint); err != nil {
		return undefined, err
	} else if big {
		return undefined, e.throwCoded(engine.KindError, "ERR_METHOD_NOT_IMPLEMENTED",
			"The bigint option of stat() is not implemented")
	}
	throwIfNoEntry := true
	if c.mode == modeSync {
		v, err := e.optValue(opts, e.keys.throwIfNoEntry)
		if err != nil {
			return undefined, err
		}
		throwIfNoEntry = !v.IsFalse()
	}
	syscallName := "stat"
	if lstat {
		syscallName = "lstat"
	}
	f := e.fsys
	if c.mode == modeSync {
		// run's synchronous form, without its closures and the
		// fs.FileInfo.
		if e.isClosed() {
			return undefined, e.throw(closedError())
		}
		rel, err := f.rel(p)
		var st statData
		if err == nil {
			st, err = f.statData(rel, lstat)
		}
		if err != nil {
			if !throwIfNoEntry && (errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)) {
				return undefined, nil
			}
			return undefined, e.throw(newSysError(err, syscallName, p))
		}
		return e.newStats(st), nil
	}
	return run(e, c, func() (fs.FileInfo, error) {
		rel, err := f.rel(p)
		var info fs.FileInfo
		if err == nil {
			info, err = f.stat(rel, lstat)
		}
		if err != nil {
			if !throwIfNoEntry && (errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)) {
				return nil, nil
			}
			return nil, newSysError(err, syscallName, p)
		}
		return info, nil
	}, func(info fs.FileInfo) (engine.Value, error) {
		if info == nil {
			return undefined, nil
		}
		return e.newStats(statOf(info)), nil
	})
}

func (e *env) accessOp(c call, args []engine.Value) (engine.Value, error) {
	p, err := e.fsPath(engine.Arg(args, 0), "path")
	if err != nil {
		return undefined, err
	}
	mode, err := e.modeBits(engine.Arg(args, 1))
	if err != nil {
		return undefined, err
	}
	f := e.fsys
	return run(e, c, func() (struct{}, error) {
		rel, err := f.rel(p)
		if err == nil {
			err = f.access(rel, mode)
		}
		if err != nil {
			return struct{}{}, newSysError(err, "access", p)
		}
		return struct{}{}, nil
	}, func(struct{}) (engine.Value, error) { return undefined, nil })
}

// modeBits reads the mode of access and copyFile, an int32 in [0, 7], as
// Node's getValidMode checks it.
func (e *env) modeBits(v engine.Value) (int, error) {
	if v.IsNullish() {
		return 0, nil
	}
	if !v.IsNumber() || v.AsNumber() != float64(int32(v.AsNumber())) {
		return 0, e.throwCoded(engine.KindTypeError, "ERR_INVALID_ARG_TYPE", "mode must be int32 or null/undefined")
	}
	if m := v.AsNumber(); m < 0 || m > 7 {
		return 0, e.throwCoded(engine.KindRangeError, "ERR_OUT_OF_RANGE", "mode is out of range: >= 0 && <= 7")
	}
	return int(v.AsNumber()), nil
}

// exists reports whether the path v names exists; a bad path does not.
func (e *env) exists(p string) bool {
	if e.isClosed() {
		return false
	}
	rel, err := e.fsys.rel(p)
	return err == nil && e.fsys.exists(rel)
}

func (e *env) existsSync(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
	p, err := e.fsPath(engine.Arg(args, 0), "path")
	if err != nil {
		if _, ok := r.ThrownValue(err); ok {
			return engine.Bool(false), nil
		}
		return undefined, err
	}
	return engine.Bool(e.exists(p)), nil
}

// existsCallback implements fs.exists(path, callback), whose callback
// gets only a boolean.
func (e *env) existsCallback(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
	cb := engine.Arg(args, 1)
	if !isFunction(cb) {
		return undefined, e.argTypeError("cb", "of type function", cb)
	}
	p, err := e.fsPath(engine.Arg(args, 0), "path")
	valid := true
	if err != nil {
		if _, ok := r.ThrownValue(err); !ok {
			return undefined, err
		}
		valid = false
	}
	finish := e.start(cb, undefined, nil)
	call := func(ok bool) func() error {
		return func() error {
			_, err := r.Call(cb, undefined, []engine.Value{engine.Bool(ok)})
			return err
		}
	}
	e.pool.submit(job{
		run:  func() { finish(call(valid && e.exists(p))) },
		drop: func() { finish(call(false)) },
	})
	return undefined, nil
}

// --- realpath, readlink -------------------------------------------------------------

func (e *env) realpathOp(c call, args []engine.Value) (engine.Value, error) {
	p, err := e.fsPath(engine.Arg(args, 0), "path")
	if err != nil {
		return undefined, err
	}
	_, enc, err := e.getOptions(engine.Arg(args, 1), encUTF8)
	if err != nil {
		return undefined, err
	}
	f := e.fsys
	return run(e, c, func() (string, error) {
		rel, err := f.rel(p)
		if err == nil {
			var res string
			if res, err = f.realpath(rel); err == nil {
				return res, nil
			}
		}
		// Node's realpathSync and realpath walk the path with lstat,
		// and fail with the absolute path of the component that
		// failed; fs.promises.realpath calls realpath(3).
		var re *resolveError
		if c.mode != modePromise && errors.As(err, &re) {
			sc := "lstat"
			if re.viaLink {
				sc = "stat"
			}
			return "", newSysError(re.err, sc, f.jsPath(re.at))
		}
		return "", newSysError(err, "realpath", p)
	}, func(res string) (engine.Value, error) {
		return e.nameValue(res, enc)
	})
}

func (e *env) readlinkOp(c call, args []engine.Value) (engine.Value, error) {
	p, err := e.fsPath(engine.Arg(args, 0), "path")
	if err != nil {
		return undefined, err
	}
	_, enc, err := e.getOptions(engine.Arg(args, 1), encUTF8)
	if err != nil {
		return undefined, err
	}
	f := e.fsys
	return run(e, c, func() (string, error) {
		rel, err := f.rel(p)
		var target string
		if err == nil {
			target, err = f.readlink(rel)
		}
		if err != nil {
			return "", newSysError(err, "readlink", p)
		}
		return target, nil
	}, func(target string) (engine.Value, error) {
		return e.nameValue(target, enc)
	})
}

// --- mkdir, rm, rmdir, unlink, rename, copyFile -------------------------------------

func (e *env) mkdirOp(c call, args []engine.Value) (engine.Value, error) {
	p, err := e.fsPath(engine.Arg(args, 0), "path")
	if err != nil {
		return undefined, err
	}
	opts := engine.Arg(args, 1)
	mode := uint32(0o777)
	recursive := false
	switch {
	case opts.IsNumber() || opts.IsString():
		if mode, err = e.modeArg(opts, "mode", 0o777); err != nil {
			return undefined, err
		}
	case opts.IsObject() && !isFunction(opts):
		if recursive, err = e.optBool(opts, e.keys.recursive, "recursive", false); err != nil {
			return undefined, err
		}
		mv, err := e.optValue(opts, e.keys.mode)
		if err != nil {
			return undefined, err
		}
		if mode, err = e.modeArg(mv, "mode", 0o777); err != nil {
			return undefined, err
		}
	}
	f := e.fsys
	perm := fs.FileMode(mode & 0o777)
	return run(e, c, func() (string, error) {
		rel, err := f.rel(p)
		if err != nil {
			return "", newSysError(err, "mkdir", p)
		}
		if !recursive {
			if err := f.mkdir(rel, perm); err != nil {
				return "", newSysError(err, "mkdir", p)
			}
			return "", nil
		}
		first := f.firstMissing(p)
		if err := f.mkdirAll(rel, perm); err != nil {
			return "", newSysError(err, "mkdir", p)
		}
		return first, nil
	}, func(first string) (engine.Value, error) {
		if first == "" {
			return undefined, nil
		}
		return str(first), nil
	})
}

// firstMissing returns the first directory of the path p, as JavaScript
// gave it, that does not exist: what mkdir with recursive creates first.
func (f *fsys) firstMissing(p string) string {
	for i := 1; i <= len(p); i++ {
		if i < len(p) && p[i] != '/' {
			continue
		}
		prefix := p[:i]
		if strings.HasSuffix(prefix, "/") && i < len(p) {
			continue
		}
		rel, err := f.rel(prefix)
		if err != nil {
			return ""
		}
		if !f.exists(rel) {
			return prefix
		}
	}
	return ""
}

// rmOptions are the options of rm.
type rmOptions struct{ force, recursive bool }

func (e *env) rmOptionsArg(opts engine.Value) (rmOptions, error) {
	var o rmOptions
	if opts.IsNullish() || isFunction(opts) {
		return o, nil
	}
	if !opts.IsObject() {
		return o, e.argTypeError("options", "of type object", opts)
	}
	var err error
	if o.force, err = e.optBool(opts, e.keys.force, "force", false); err != nil {
		return o, err
	}
	if o.recursive, err = e.optBool(opts, e.keys.recursive, "recursive", false); err != nil {
		return o, err
	}
	for _, k := range []struct {
		key  engine.PropertyKey
		name string
	}{{e.keys.maxRetries, "maxRetries"}, {e.keys.retryDelay, "retryDelay"}} {
		v, err := e.optValue(opts, k.key)
		if err != nil {
			return o, err
		}
		if !v.IsUndefined() && !v.IsNumber() {
			return o, e.argTypeError("options."+k.name, "of type number", v)
		}
	}
	return o, nil
}

func (e *env) rmOp(c call, args []engine.Value) (engine.Value, error) {
	p, err := e.fsPath(engine.Arg(args, 0), "path")
	if err != nil {
		return undefined, err
	}
	o, err := e.rmOptionsArg(engine.Arg(args, 1))
	if err != nil {
		return undefined, err
	}
	f := e.fsys
	return run(e, c, func() (struct{}, error) {
		return struct{}{}, f.rm(p, o)
	}, func(struct{}) (engine.Value, error) { return undefined, nil })
}

// rm removes the path p as rm does.
func (f *fsys) rm(p string, o rmOptions) error {
	rel, err := f.rel(p)
	if err != nil {
		return newSysError(err, "rm", p)
	}
	if !f.write {
		return newSysError(errReadOnly, "rm", p)
	}
	info, err := f.stat(rel, true)
	if err != nil {
		if o.force && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return newSysError(err, "lstat", p)
	}
	if info.IsDir() {
		if !o.recursive {
			return &systemError{code: "ERR_FS_EISDIR", prefix: "Path is a directory",
				errCode: "EISDIR", desc: "is a directory", syscall: "rm", path: p}
		}
		err = f.removeAll(rel)
	} else {
		err = f.remove(rel)
	}
	if err != nil {
		if o.force && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return newSysError(err, "rm", p)
	}
	return nil
}

func (e *env) rmdirOp(c call, args []engine.Value) (engine.Value, error) {
	p, err := e.fsPath(engine.Arg(args, 0), "path")
	if err != nil {
		return undefined, err
	}
	opts := engine.Arg(args, 1)
	recursive := false
	if opts.IsObject() && !isFunction(opts) {
		if recursive, err = e.optBool(opts, e.keys.recursive, "recursive", false); err != nil {
			return undefined, err
		}
	}
	f := e.fsys
	return run(e, c, func() (struct{}, error) {
		rel, err := f.rel(p)
		if err != nil {
			return struct{}{}, newSysError(err, "rmdir", p)
		}
		if !f.write {
			return struct{}{}, newSysError(errReadOnly, "rmdir", p)
		}
		info, err := f.stat(rel, true)
		if err != nil {
			return struct{}{}, newSysError(err, "rmdir", p)
		}
		if !info.IsDir() {
			return struct{}{}, newSysError(syscall.ENOTDIR, "rmdir", p)
		}
		if recursive {
			err = f.removeAll(rel)
		} else {
			err = f.remove(rel)
		}
		if err != nil {
			return struct{}{}, newSysError(err, "rmdir", p)
		}
		return struct{}{}, nil
	}, func(struct{}) (engine.Value, error) { return undefined, nil })
}

func (e *env) unlinkOp(c call, args []engine.Value) (engine.Value, error) {
	p, err := e.fsPath(engine.Arg(args, 0), "path")
	if err != nil {
		return undefined, err
	}
	f := e.fsys
	return run(e, c, func() (struct{}, error) {
		rel, err := f.rel(p)
		if err != nil {
			return struct{}{}, newSysError(err, "unlink", p)
		}
		if !f.write {
			return struct{}{}, newSysError(errReadOnly, "unlink", p)
		}
		info, err := f.stat(rel, true)
		if err == nil && info.IsDir() {
			err = syscall.EISDIR
		}
		if err == nil {
			err = f.remove(rel)
		}
		if err != nil {
			return struct{}{}, newSysError(err, "unlink", p)
		}
		return struct{}{}, nil
	}, func(struct{}) (engine.Value, error) { return undefined, nil })
}

func (e *env) renameOp(c call, args []engine.Value) (engine.Value, error) {
	from, err := e.fsPath(engine.Arg(args, 0), "oldPath")
	if err != nil {
		return undefined, err
	}
	to, err := e.fsPath(engine.Arg(args, 1), "newPath")
	if err != nil {
		return undefined, err
	}
	f := e.fsys
	return run(e, c, func() (struct{}, error) {
		a, err := f.rel(from)
		var b string
		if err == nil {
			b, err = f.rel(to)
		}
		if err == nil {
			err = f.rename(a, b)
		}
		if err != nil {
			return struct{}{}, newSysError2(err, "rename", from, to)
		}
		return struct{}{}, nil
	}, func(struct{}) (engine.Value, error) { return undefined, nil })
}

func (e *env) copyFileOp(c call, args []engine.Value) (engine.Value, error) {
	from, err := e.fsPath(engine.Arg(args, 0), "src")
	if err != nil {
		return undefined, err
	}
	to, err := e.fsPath(engine.Arg(args, 1), "dest")
	if err != nil {
		return undefined, err
	}
	mv := engine.Arg(args, 2)
	if isFunction(mv) {
		mv = undefined
	}
	mode, err := e.modeBits(mv)
	if err != nil {
		return undefined, err
	}
	f := e.fsys
	return run(e, c, func() (struct{}, error) {
		a, err := f.rel(from)
		var b string
		if err == nil {
			b, err = f.rel(to)
		}
		if err == nil {
			err = f.copyFile(a, b, mode&1 != 0)
		}
		if err != nil {
			return struct{}{}, newSysError2(err, "copyfile", from, to)
		}
		return struct{}{}, nil
	}, func(struct{}) (engine.Value, error) { return undefined, nil })
}
