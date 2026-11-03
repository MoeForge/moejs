package node

import (
	"errors"
	"io/fs"
	"path"
	"runtime"
	"strings"

	"github.com/MoeForge/moejs/engine"
)

// cpOptions are the options of cp.
type cpOptions struct {
	recursive, force, errorOnExist bool
}

// cpOp implements cp and cpSync: a file, or with recursive a directory,
// copied. A link is copied as what it leads to (dereference), since the
// package creates no links; one that leads out of the root fails.
func (e *env) cpOp(c call, args []engine.Value) (engine.Value, error) {
	from, err := e.fsPath(engine.Arg(args, 0), "src")
	if err != nil {
		return undefined, err
	}
	to, err := e.fsPath(engine.Arg(args, 1), "dest")
	if err != nil {
		return undefined, err
	}
	opts := engine.Arg(args, 2)
	if isFunction(opts) {
		opts = undefined
	}
	o := cpOptions{force: true}
	if !opts.IsNullish() {
		if !opts.IsObject() {
			return undefined, e.argTypeError("options", "of type object", opts)
		}
		if o.recursive, err = e.optBool(opts, e.keys.recursive, "recursive", false); err != nil {
			return undefined, err
		}
		if o.force, err = e.optBool(opts, e.keys.force, "force", true); err != nil {
			return undefined, err
		}
		if o.errorOnExist, err = e.optBool(opts, e.keys.errorOnExist, "errorOnExist", false); err != nil {
			return undefined, err
		}
		if fv, err := e.optValue(opts, e.keys.filter); err != nil {
			return undefined, err
		} else if !fv.IsUndefined() {
			return undefined, e.throwCoded(engine.KindError, "ERR_METHOD_NOT_IMPLEMENTED",
				"The filter option of cp() is not implemented")
		}
	}
	f := e.fsys
	return run(e, c, func() (struct{}, error) {
		return struct{}{}, f.cp(from, to, o)
	}, func(struct{}) (engine.Value, error) { return undefined, nil })
}

func (f *fsys) cp(from, to string, o cpOptions) error {
	if !f.write {
		return newSysError(errReadOnly, "cp", to)
	}
	a, err := f.rel(from)
	if err != nil {
		return newSysError(err, "lstat", from)
	}
	b, err := f.rel(to)
	if err != nil {
		return newSysError(err, "cp", to)
	}
	// Node.js makes the destination's missing parents.
	if dir := path.Dir(b); dir != "." && !f.exists(dir) {
		if err := f.mkdirAll(dir, 0o777); err != nil {
			return newSysError(err, "mkdir", path.Dir(to))
		}
	}
	info, err := f.stat(a, false)
	if err != nil {
		if _, lerr := f.stat(a, true); lerr == nil && !errors.Is(err, fs.ErrNotExist) {
			return newSysError(err, "stat", from)
		}
		return newSysError(err, "lstat", from)
	}
	if info.IsDir() {
		if !o.recursive {
			return &codedError{kind: engine.KindError, code: "ERR_FS_EISDIR",
				msg: "Recursive option not enabled, cannot copy a directory: " + from + "/"}
		}
		ra, err1 := f.resolve(a, true, false)
		rb, err2 := f.resolve(b, true, true)
		if err1 == nil && err2 == nil && (ra == rb || len(rb) > len(ra) && rb[:len(ra)+1] == ra+"/" || ra == ".") {
			msg := "Cannot copy " + strings.TrimSuffix(from, "/") + "/ to a subdirectory of self " + to
			if ra == rb {
				msg = "src and dest cannot be the same"
			}
			return &codedError{kind: engine.KindError, code: "ERR_FS_CP_EINVAL", msg: msg}
		}
		return f.cpDir(a, b, from, to, o)
	}
	return f.cpFile(a, b, from, to, o)
}

func (f *fsys) cpFile(a, b, from, to string, o cpOptions) error {
	if _, err := f.stat(b, true); err == nil {
		if !o.force {
			if o.errorOnExist {
				return &systemError{code: "ERR_FS_CP_EEXIST", prefix: "Target already exists",
					errCode: "EEXIST", desc: to + " already exists", syscall: "cp", path: to}
			}
			return nil
		}
		if info, err := f.stat(b, false); err == nil && info.IsDir() {
			return &codedError{kind: engine.KindError, code: "ERR_FS_CP_NON_DIR_TO_DIR",
				msg: "Cannot overwrite non-directory " + from + " with directory " + to}
		}
	}
	if err := f.copyFile(a, b, false); err != nil {
		return newSysError2(err, "copyfile", from, to)
	}
	return nil
}

func (f *fsys) cpDir(a, b, from, to string, o cpOptions) error {
	if info, err := f.stat(b, false); err == nil {
		if !info.IsDir() {
			return &codedError{kind: engine.KindError, code: "ERR_FS_CP_DIR_TO_NON_DIR",
				msg: "Cannot overwrite directory " + from + " with non-directory " + to}
		}
	} else if errors.Is(err, fs.ErrNotExist) {
		if err := f.mkdir(b, 0o777); err != nil {
			return newSysError(err, "mkdir", to)
		}
	} else {
		return newSysError(err, "stat", to)
	}
	ents, err := f.readdir(a)
	if err != nil {
		return newSysError(err, "opendir", from)
	}
	for _, d := range ents {
		// Links are copied as what they lead to: one back to its own
		// directory recurses to the system's link limit, which the
		// runtime's interrupt and Close cut short.
		if err := f.halt(); err != nil {
			return err
		}
		ca, cb := a+"/"+d.name, b+"/"+d.name
		cf, ct := path.Join(from, d.name), path.Join(to, d.name)
		isDir := d.typ.IsDir()
		if d.typ&fs.ModeSymlink != 0 {
			info, err := f.stat(ca, false)
			if err != nil {
				return newSysError(err, "stat", cf)
			}
			isDir = info.IsDir()
		}
		if isDir {
			err = f.cpDir(ca, cb, cf, ct, o)
		} else {
			err = f.cpFile(ca, cb, cf, ct, o)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// platform is process.platform.
func platform() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}
