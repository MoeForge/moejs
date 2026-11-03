package node

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// fsys is the file system of an installation: the root directory, the
// mount and the working directory JavaScript sees, and the policy. Its
// methods take paths relative to the root, as rel returns them, and are
// safe for concurrent use.
type fsys struct {
	root *os.Root
	fast *fastRoot // nil without openat2
	// hostRoot is Root, absolute and clean, with forward slashes: the
	// absolute targets of links under it are followed.
	hostRoot string
	mount    string // the absolute, clean path JavaScript sees the root at
	cwdRel   string // the working directory relative to the mount; "" for the mount
	write    bool
	allow    func(op, path string) error
	// halt reports the error that stops a long walk: the runtime's
	// interrupt, or Close's. It is safe on any goroutine.
	halt func() error
}

func openFS(o *FSOptions) (*fsys, error) {
	if o.Root == "" {
		return nil, errors.New("node: FSOptions.Root is empty")
	}
	abs, err := filepath.Abs(o.Root)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	f := &fsys{root: root, hostRoot: filepath.ToSlash(abs), write: o.Write, allow: o.Allow,
		halt: func() error { return nil }}
	if !noFast {
		f.fast = newFastRoot(abs)
		if f.fast != nil {
			f.fast.noCache = o.NoDescriptorCache
		}
	}
	f.mount = f.hostRoot
	if o.Mount != "" {
		if !strings.HasPrefix(o.Mount, "/") {
			root.Close()
			return nil, errors.New("node: FSOptions.Mount " + o.Mount + " is not an absolute path")
		}
		f.mount = path.Clean(o.Mount)
	}
	cwd := f.mount
	if o.Cwd != "" {
		cwd = path.Clean(o.Cwd)
		if !strings.HasPrefix(o.Cwd, "/") {
			cwd = path.Join(f.mount, o.Cwd)
		}
	}
	rest, ok := under(f.mount, cwd)
	if !ok {
		root.Close()
		return nil, errors.New("node: FSOptions.Cwd " + o.Cwd + " is not under the mount " + f.mount)
	}
	f.cwdRel = rest
	return f, nil
}

// noFast turns the openat2 path off, for the tests of the os.Root path.
var noFast bool

func (f *fsys) close() error {
	f.fast.close()
	return f.root.Close()
}

// under returns the part of the absolute path p below the directory dir,
// "" for dir itself, and whether p is dir or below it.
func under(dir, p string) (string, bool) {
	if dir == "/" {
		return strings.TrimLeft(p, "/"), true
	}
	if !strings.HasPrefix(p, dir) {
		return "", false
	}
	rest := p[len(dir):]
	if rest == "" {
		return "", true
	}
	if rest[0] != '/' {
		return "", false
	}
	for rest != "" && rest[0] == '/' {
		rest = rest[1:]
	}
	return rest, true
}

// rel returns the path relative to the root of p, a path JavaScript gave:
// a relative one joined to the working directory, an absolute one below
// the mount. It is not cleaned: the root resolves ".." and links as the
// system does, after the links before them. A path outside the mount is
// errEscape.
func (f *fsys) rel(p string) (string, error) {
	if p == "" {
		return "", syscall.ENOENT
	}
	if filepath.Separator == '\\' {
		// Node.js on Windows takes either separator.
		p = strings.ReplaceAll(p, "\\", "/")
	}
	if strings.HasPrefix(p, "/") {
		rest, ok := under(f.mount, p)
		if ok && (rest == ".." || strings.HasPrefix(rest, "../")) {
			// A ".." right after the mount leaves JavaScript's view of
			// the root; what follows is read lexically, so that
			// /w/../w/a.txt is /w/a.txt.
			ok = false
		}
		if !ok {
			if rest, ok = under(f.mount, path.Clean(p)); !ok {
				return "", errEscape
			}
		}
		if rest == "" {
			return ".", nil
		}
		return rest, nil
	}
	if f.cwdRel == "" {
		return p, nil
	}
	return f.cwdRel + "/" + p, nil
}

// jsPath returns the absolute path JavaScript sees for rel, a clean path
// relative to the root.
func (f *fsys) jsPath(rel string) string {
	if rel == "." || rel == "" {
		return f.mount
	}
	if f.mount == "/" {
		return "/" + rel
	}
	return f.mount + "/" + rel
}

// native converts a path relative to the root to the system's separators.
func native(rel string) string {
	if filepath.Separator == '/' {
		return rel
	}
	return filepath.FromSlash(rel)
}

// maxSymlinks bounds the links one resolution follows, as Linux's
// MAXSYMLINKS does.
const maxSymlinks = 40

// What resolve does with a path whose end does not exist.
const (
	missingNone = iota // an error
	missingLast        // the last component may be missing, for a path an operation creates
	missingTail        // any number of the last components, for mkdir -p
)

// resolveOpts are the options of resolveWith.
type resolveOpts struct {
	followLast bool
	missing    int
	// ask, when set, is asked about each component the walk reaches
	// before it looks at it: last is set for the last component of the
	// path (and of a link's target in its place), and for the missing
	// components of a path missingTail creates. Its error ends the walk
	// as it is.
	ask func(cand string, last bool) error
}

// resolve returns rel with every link resolved and ".." applied, a clean
// path relative to the root: the path resolution of the system, confined
// to the root. The last component's link is followed when followLast is
// set, or when rel ends in a slash. With missingOK, a last component that
// does not exist is kept as it is, for a path an operation creates. A
// path that leaves the root, through ".." or a link, is errEscape; a
// link's absolute target is followed when it lies under the root.
func (f *fsys) resolve(rel string, followLast, missingOK bool) (string, error) {
	o := resolveOpts{followLast: followLast}
	if missingOK {
		o.missing = missingLast
	}
	return f.resolveWith(rel, o)
}

func (f *fsys) resolveWith(rel string, o resolveOpts) (string, error) {
	parts := splitPath(rel, nil)
	trailing := strings.HasSuffix(rel, "/")
	stack := make([]string, 0, len(parts))
	links := 0
	var lastInfo fs.FileInfo
	// link is the outermost link whose target the walk is in, and rest
	// the count of components after it.
	link, rest := "", 0
	fail := func(err error, at string) error {
		if link != "" && len(parts) >= rest {
			return &resolveError{err: err, at: link, viaLink: true}
		}
		return &resolveError{err: err, at: at}
	}
	for len(parts) > 0 {
		if link != "" && len(parts) <= rest {
			link = ""
		}
		part := parts[0]
		parts = parts[1:]
		switch part {
		case ".":
			continue
		case "..":
			if len(stack) == 0 {
				return "", errEscape
			}
			stack = stack[:len(stack)-1]
			lastInfo = nil
			continue
		}
		cand := strings.Join(append(stack, part), "/")
		last := len(parts) == 0
		if o.ask != nil {
			if err := o.ask(cand, last); err != nil {
				return "", err
			}
		}
		fi, err := f.root.Lstat(native(cand))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				switch {
				case last && o.missing != missingNone:
					return cand, nil
				case o.missing == missingTail:
					return f.missingTail(append(stack, part), parts, o)
				}
			}
			return "", fail(err, cand)
		}
		if fi.Mode()&fs.ModeSymlink != 0 && (!last || o.followLast || trailing) {
			if links++; links > maxSymlinks {
				return "", fail(syscall.ELOOP, cand)
			}
			target, err := f.root.Readlink(native(cand))
			if err != nil {
				return "", fail(err, cand)
			}
			if link == "" {
				link, rest = cand, len(parts)
			}
			target = filepath.ToSlash(target)
			if filepath.IsAbs(target) || strings.HasPrefix(target, "/") {
				rest, ok := under(f.hostRoot, path.Clean(target))
				if !ok {
					return "", errEscape
				}
				stack = stack[:0]
				parts = splitPath(rest, parts)
			} else {
				parts = splitPath(target, parts)
			}
			continue
		}
		if !last && !fi.IsDir() {
			return "", fail(syscall.ENOTDIR, cand)
		}
		stack = append(stack, part)
		lastInfo = fi
	}
	if trailing && lastInfo != nil && !lastInfo.IsDir() {
		return "", syscall.ENOTDIR
	}
	if len(stack) == 0 {
		return ".", nil
	}
	return strings.Join(stack, "/"), nil
}

// missingTail returns the path of the components stack, the first of
// which does not exist, and parts after them, for mkdir -p: what does not
// exist has no links, and ".." in it is an error, as for the system.
func (f *fsys) missingTail(stack, parts []string, o resolveOpts) (string, error) {
	cand := strings.Join(stack, "/")
	if o.ask != nil {
		if err := o.ask(cand, true); err != nil {
			return "", err
		}
	}
	for _, part := range parts {
		switch part {
		case ".":
			continue
		case "..":
			return "", &resolveError{err: syscall.ENOENT, at: cand}
		}
		cand += "/" + part
		if o.ask != nil {
			if err := o.ask(cand, true); err != nil {
				return "", err
			}
		}
	}
	return cand, nil
}

// resolveError is resolve's error at the component at, a clean path
// relative to the root.
type resolveError struct {
	err error
	at  string
	// viaLink is set when the error came up in the target of the link
	// at, as Node's realpath stats a link before it reads it.
	viaLink bool
}

func (e *resolveError) Error() string { return e.err.Error() }
func (e *resolveError) Unwrap() error { return e.err }

// splitPath returns the components of p followed by rest.
func splitPath(p string, rest []string) []string {
	var parts []string
	for _, c := range strings.Split(p, "/") {
		if c != "" {
			parts = append(parts, c)
		}
	}
	return append(parts, rest...)
}

// allowError is the Allow hook's refusal.
type allowError struct{ err error }

func (e *allowError) Error() string { return e.err.Error() }
func (e *allowError) Unwrap() error { return e.err }

// Access kinds of the Allow hook.
const (
	accessRead  = "read"
	accessWrite = "write"
)

// do runs op on rel for an access of kind access: with the Allow hook,
// on rel resolved (see resolve for followLast and missingOK) once the
// hook approved it; without, on rel, and again on rel resolved when the
// root refused it as leaving the root, which a link with an absolute
// target under the root gets too.
func (f *fsys) do(access, rel string, followLast, missingOK bool, op func(rel string) error) error {
	if access == accessWrite && !f.write {
		return errReadOnly
	}
	if f.allow != nil {
		res, err := f.check(access, rel, followLast, missingOK)
		if err != nil {
			return err
		}
		return op(res)
	}
	err := op(rel)
	if isEscape(err) {
		res, rerr := f.resolve(rel, followLast, missingOK)
		if rerr != nil {
			return rerr
		}
		return op(res)
	}
	return err
}

// check resolves rel and asks the Allow hook about it, as the hook's
// documentation promises: each directory the path names, lexically, and
// each component the resolution reaches, before anything about it is
// looked up, so that a denied directory tells nothing of what it holds
// or of how its links lead; the directories as "read", the path itself
// (and what a missingTail path creates) as access.
func (f *fsys) check(access, rel string, followLast, missingOK bool) (string, error) {
	missing := missingNone
	if missingOK {
		missing = missingLast
	}
	return f.checkWith(access, rel, followLast, missing)
}

func (f *fsys) checkWith(access, rel string, followLast bool, missing int) (string, error) {
	var asked map[string]string
	ask := func(p string, last bool) error {
		op := accessRead
		if last {
			op = access
		}
		if asked[p] == op || asked[p] == accessWrite {
			return nil
		}
		if asked == nil {
			asked = make(map[string]string, 8)
		}
		asked[p] = op
		if err := f.allow(op, f.jsPath(p)); err != nil {
			return &allowError{err}
		}
		return nil
	}
	clean := path.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errEscape
	}
	if clean != "." {
		for i := 0; i <= len(clean); i++ {
			if i == len(clean) || clean[i] == '/' {
				if err := ask(clean[:i], i == len(clean)); err != nil {
					return "", err
				}
			}
		}
	}
	res, err := f.resolveWith(rel, resolveOpts{followLast: followLast, missing: missing, ask: ask})
	if err != nil {
		return "", err
	}
	if err := ask(res, true); err != nil {
		return "", err
	}
	return res, nil
}

// allowTree asks the Allow hook about every entry below the directory
// rel, a resolved path, as "write", and about the path each will have
// below to when to is not empty, for a recursive rm and the rename of a
// directory: the hook guards what lies below a directory too. The links
// below are not followed.
func (f *fsys) allowTree(rel, to string) error {
	ents, err := f.listRaw(rel)
	if err != nil {
		return err
	}
	for i, d := range ents {
		if i&255 == 0 {
			if err := f.halt(); err != nil {
				return err
			}
		}
		p := rel + "/" + d.name
		if rel == "." {
			p = d.name
		}
		if err := f.allow(accessWrite, f.jsPath(p)); err != nil {
			return &allowError{err}
		}
		q := ""
		if to != "" {
			q = to + "/" + d.name
			if err := f.allow(accessWrite, f.jsPath(q)); err != nil {
				return &allowError{err}
			}
		}
		if d.typ.IsDir() {
			if err := f.allowTree(p, q); err != nil {
				return err
			}
		}
	}
	return nil
}

// listRaw lists the directory rel, a resolved path, with the types of
// its entries, without asking the hook.
func (f *fsys) listRaw(rel string) ([]dirent, error) {
	if out, err, ok := f.readdirFast(rel, true); ok {
		return out, err
	}
	file, err := f.openRead(rel)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	ents, err := file.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	out := make([]dirent, len(ents))
	for i, d := range ents {
		out[i] = dirent{d.Name(), d.Type()}
	}
	return out, nil
}

// --- reading ------------------------------------------------------------------------

// maxFileSize is the largest file readFile reads, as Node's kIoMaxLength.
const maxFileSize = 1<<31 - 1

// tooLargeError is readFile's error for a file larger than the memory
// limit or 2 GiB: Node's ERR_FS_FILE_TOO_LARGE, a RangeError.
type tooLargeError struct {
	size  int64
	limit int64 // 0: Node's own 2 GiB bound
}

func (e *tooLargeError) Error() string {
	if e.limit == 0 {
		return "File size (" + itoa(e.size) + ") is greater than 2 GiB"
	}
	return "File size (" + itoa(e.size) + ") is greater than the memory limit (" + itoa(e.limit) + ")"
}

// openRead opens rel for reading. The descriptor is non-blocking, so that
// a FIFO without a writer does not block the open.
func (f *fsys) openRead(rel string) (*os.File, error) {
	return f.root.OpenFile(native(rel), os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

// readFile returns the content of the file rel, opened with flags, which
// write when writes is set. A file larger than limit (when above zero)
// fails before it is read. A directory fails with EISDIR for the read,
// without a path, as in Node.js.
func (f *fsys) readFile(rel, jsPath string, flags int, writes bool, limit int64, reserve func(int64) error) ([]byte, error) {
	var data []byte
	access := accessRead
	if writes {
		access = accessWrite
	}
	err := f.do(access, rel, true, writes, func(rel string) error {
		var err error
		var ok bool
		if data, err, ok = f.readFileFast(rel, flags, limit, reserve); ok {
			return err
		}
		file, err := f.root.OpenFile(native(rel), flags|syscall.O_NONBLOCK, 0o666)
		if err != nil {
			return err
		}
		defer file.Close()
		data, err = readAll(file, limit, reserve)
		return err
	})
	if err != nil {
		var se *sysError
		var tl *tooLargeError
		if errors.As(err, &se) || errors.As(err, &tl) {
			return nil, err
		}
		return nil, newSysError(err, "open", jsPath)
	}
	return data, nil
}

// readAll reads an open file as Node's readFile does: as many bytes as
// fstat reports, or until its end for a file that reports none.
func readAll(file *os.File, limit int64, reserve func(int64) error) ([]byte, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, &sysError{code: errnoCode(err), syscall: "fstat"}
	}
	if info.IsDir() {
		return nil, &sysError{code: "EISDIR", syscall: "read"}
	}
	size := info.Size()
	if size > maxFileSize {
		return nil, &tooLargeError{size: size}
	}
	if limit > 0 && size > limit {
		return nil, &tooLargeError{size: size, limit: limit}
	}
	if reserve != nil {
		n := size
		if !info.Mode().IsRegular() {
			n = 0
		}
		if err := reserve(n); err != nil {
			return nil, err
		}
	}
	if !info.Mode().IsRegular() {
		return readSpecial(file, limit)
	}
	if size == 0 {
		return readToEnd(file, limit)
	}
	buf := make([]byte, size)
	n, err := io.ReadFull(file, buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return nil, &sysError{code: errnoCode(err), syscall: "read"}
	}
	return buf[:n], nil
}

// readToEnd reads a file that reports no size, such as those of /proc,
// to its end.
func readToEnd(file io.Reader, limit int64) ([]byte, error) {
	bound := int64(maxFileSize)
	if limit > 0 {
		bound = limit
	}
	buf := make([]byte, 0, 8192)
	for {
		if len(buf) == cap(buf) {
			buf = slices.Grow(buf, cap(buf))
		}
		n, err := file.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if int64(len(buf)) > bound {
			return nil, &tooLargeError{size: int64(len(buf)), limit: limit}
		}
		if err == io.EOF {
			return buf, nil
		}
		if err != nil {
			return nil, &sysError{code: errnoCode(err), syscall: "read"}
		}
	}
}

// readSpecial reads a file that is neither regular nor a directory, a
// FIFO or a device, with non-blocking reads: one with no data waiting
// fails with EAGAIN rather than block the loop or a worker.
func readSpecial(file *os.File, limit int64) ([]byte, error) {
	rc, err := file.SyscallConn()
	if err != nil {
		return nil, &sysError{code: errnoCode(err), syscall: "read"}
	}
	return readToEnd(rawReader{rc}, limit)
}

var eof = io.EOF

type rawReader struct{ rc syscall.RawConn }

func (r rawReader) Read(p []byte) (int, error) {
	var n int
	var rerr error
	err := r.rc.Read(func(fd uintptr) bool {
		n, rerr = rawRead(fd, p)
		return true
	})
	if err != nil {
		return 0, err
	}
	if rerr != nil {
		return 0, rerr
	}
	if n == 0 && len(p) > 0 {
		return 0, io.EOF
	}
	return n, nil
}

// dirent is an entry of a directory listing: its name and type bits.
type dirent struct {
	name string
	typ  fs.FileMode
}

// typeUnknown is the type of an entry whose type the system did not give
// (DT_UNKNOWN) and that was not looked up.
const typeUnknown = fs.ModeIrregular

// sortDirents sorts a listing by name, as libuv's scandir sorts it.
func sortDirents(d []dirent) {
	slices.SortFunc(d, func(a, b dirent) int { return strings.Compare(a.name, b.name) })
}

// readdir lists the directory rel, sorted by name as libuv sorts them,
// with the type of each entry.
func (f *fsys) readdir(rel string) ([]dirent, error) { return f.readdirTypes(rel, true) }

// readdirTypes lists the directory rel; without types an entry may be
// typeUnknown.
func (f *fsys) readdirTypes(rel string, types bool) ([]dirent, error) {
	var out []dirent
	err := f.do(accessRead, rel, true, false, func(rel string) error {
		var err error
		var ok bool
		if out, err, ok = f.readdirFast(rel, types); ok {
			return err
		}
		file, err := f.openRead(rel)
		if err != nil {
			return err
		}
		defer file.Close()
		ents, err := file.ReadDir(-1)
		if err != nil {
			return err
		}
		out = make([]dirent, len(ents))
		for i, d := range ents {
			out[i] = dirent{d.Name(), d.Type()}
		}
		sortDirents(out)
		return nil
	})
	return out, err
}

// stat returns the information of rel, of the link itself for lstat.
func (f *fsys) stat(rel string, lstat bool) (fs.FileInfo, error) {
	var info fs.FileInfo
	err := f.do(accessRead, rel, !lstat, false, func(rel string) error {
		var err error
		var ok bool
		if info, err, ok = f.statFast(rel, lstat); ok {
			return err
		}
		if lstat {
			info, err = f.root.Lstat(native(rel))
		} else {
			info, err = f.root.Stat(native(rel))
		}
		return err
	})
	return info, err
}

// statData is stat for Node's fields, without an fs.FileInfo when the
// fast path takes it.
func (f *fsys) statData(rel string, lstat bool) (statData, error) {
	var st statData
	err := f.do(accessRead, rel, !lstat, false, func(rel string) error {
		if err, ok := f.statFastData(rel, lstat, &st); ok {
			return err
		}
		var info fs.FileInfo
		var err error
		if lstat {
			info, err = f.root.Lstat(native(rel))
		} else {
			info, err = f.root.Stat(native(rel))
		}
		if err == nil {
			st = statOf(info)
		}
		return err
	})
	return st, err
}

// isDir reports whether rel is a directory, following links, as Node's
// internalModuleStat does.
func (f *fsys) isDir(rel string) bool {
	info, err := f.stat(rel, false)
	return err == nil && info.IsDir()
}

// realpath returns the path JavaScript sees for rel with its links
// resolved.
func (f *fsys) realpath(rel string) (string, error) {
	var res string
	var err error
	if f.allow != nil {
		res, err = f.check(accessRead, rel, true, false)
	} else {
		res, err = f.resolve(rel, true, false)
	}
	if err != nil {
		return "", err
	}
	return f.jsPath(res), nil
}

// readlink returns the target of the link rel. An absolute target under
// the root is returned as JavaScript sees it.
func (f *fsys) readlink(rel string) (string, error) {
	var target string
	err := f.do(accessRead, rel, false, false, func(rel string) error {
		var err error
		target, err = f.root.Readlink(native(rel))
		return err
	})
	if err != nil {
		return "", err
	}
	t := filepath.ToSlash(target)
	if filepath.IsAbs(target) || strings.HasPrefix(t, "/") {
		if rest, ok := under(f.hostRoot, path.Clean(t)); ok {
			return f.jsPath(rest), nil
		}
	}
	return target, nil
}

// access checks rel as access(2) does with mode, a combination of R_OK,
// W_OK and X_OK, or F_OK (0) for existence. W_OK fails with EROFS without
// Write.
func (f *fsys) access(rel string, mode int) error {
	return f.do(accessRead, rel, true, false, func(rel string) error {
		info, err := f.root.Stat(native(rel))
		if err != nil {
			return err
		}
		if mode&wOK != 0 && !f.write {
			return errReadOnly
		}
		if mode == 0 {
			return nil
		}
		return checkAccess(info, mode)
	})
}

// Modes of access.
const (
	fOK = 0
	xOK = 1
	wOK = 2
	rOK = 4
)

// --- writing ------------------------------------------------------------------------

// writeFile writes data to rel, opened with flags.
func (f *fsys) writeFile(rel string, data []byte, flags int, perm fs.FileMode) error {
	return f.do(accessWrite, rel, true, true, func(rel string) error {
		file, err := f.root.OpenFile(native(rel), flags|syscall.O_NONBLOCK, perm)
		if err != nil {
			return err
		}
		_, werr := file.Write(data)
		cerr := file.Close()
		if werr != nil {
			return &sysError{code: errnoCode(werr), syscall: "write"}
		}
		if cerr != nil {
			return &sysError{code: errnoCode(cerr), syscall: "close"}
		}
		return nil
	})
}

// mkdir creates the directory rel.
func (f *fsys) mkdir(rel string, perm fs.FileMode) error {
	return f.do(accessWrite, rel, false, true, func(rel string) error {
		return f.root.Mkdir(native(rel), perm)
	})
}

// mkdirAll creates rel and the directories above it that do not exist.
func (f *fsys) mkdirAll(rel string, perm fs.FileMode) error {
	if !f.write {
		return errReadOnly
	}
	if f.allow != nil {
		// Each directory it creates is asked about.
		res, err := f.checkWith(accessWrite, rel, true, missingTail)
		if err != nil {
			return err
		}
		return f.root.MkdirAll(native(res), perm)
	}
	return f.do(accessWrite, rel, true, true, func(rel string) error {
		return f.root.MkdirAll(native(rel), perm)
	})
}

// exists reports whether rel exists, following links.
func (f *fsys) exists(rel string) bool {
	_, err := f.stat(rel, false)
	return err == nil
}

// remove removes the file or empty directory rel.
func (f *fsys) remove(rel string) error {
	defer f.fast.invalidate()
	return f.do(accessWrite, rel, false, false, func(rel string) error {
		return f.root.Remove(native(rel))
	})
}

// removeAll removes rel and what it holds.
func (f *fsys) removeAll(rel string) error {
	defer f.fast.invalidate()
	return f.do(accessWrite, rel, false, false, func(rel string) error {
		if f.allow != nil {
			// Nothing is removed unless the hook allows all of it.
			if info, err := f.root.Lstat(native(rel)); err == nil && info.IsDir() {
				if err := f.allowTree(rel, ""); err != nil {
					return err
				}
			}
		}
		return f.root.RemoveAll(native(rel))
	})
}

// rename renames from to to.
func (f *fsys) rename(from, to string) error {
	if !f.write {
		return errReadOnly
	}
	defer f.fast.invalidate()
	if f.allow != nil {
		a, err := f.check(accessWrite, from, false, false)
		if err != nil {
			return err
		}
		b, err := f.check(accessWrite, to, false, true)
		if err != nil {
			return err
		}
		from, to = a, b
		// What a directory holds moves with it: the hook is asked about
		// each entry, where it is and where it goes.
		if info, err := f.root.Lstat(native(from)); err == nil && info.IsDir() {
			if err := f.allowTree(from, to); err != nil {
				return err
			}
		}
	}
	err := f.renameRaw(from, to)
	if isEscape(err) {
		a, rerr := f.resolve(from, false, false)
		if rerr != nil {
			return rerr
		}
		b, rerr := f.resolve(to, false, true)
		if rerr != nil {
			return rerr
		}
		err = f.renameRaw(a, b)
	}
	return err
}

// renameRaw renames as rename(2) does. Go refuses to rename onto an
// existing directory: a directory may replace an empty one, as on Linux,
// and fails with ENOTEMPTY for one that is not.
func (f *fsys) renameRaw(from, to string) error {
	err := f.root.Rename(native(from), native(to))
	if err == nil || !errors.Is(err, syscall.EEXIST) {
		return err
	}
	src, serr := f.root.Lstat(native(from))
	dst, derr := f.root.Lstat(native(to))
	if serr != nil || derr != nil || !dst.IsDir() {
		return err
	}
	if !src.IsDir() {
		return syscall.EISDIR
	}
	if rerr := f.root.Remove(native(to)); rerr != nil {
		if errors.Is(rerr, syscall.ENOTEMPTY) || errors.Is(rerr, syscall.EEXIST) {
			return syscall.ENOTEMPTY
		}
		return rerr
	}
	return f.root.Rename(native(from), native(to))
}

// copyFile copies the file from to to, which excl requires not to exist.
// The copy gets the mode of the source, as libuv's copyfile gives it.
func (f *fsys) copyFile(from, to string, excl bool) error {
	if !f.write {
		return errReadOnly
	}
	var src *os.File
	err := f.do(accessRead, from, true, false, func(rel string) error {
		var err error
		src, err = f.openRead(rel)
		return err
	})
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	if info.IsDir() {
		return syscall.EISDIR
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if excl {
		flags |= os.O_EXCL
	}
	return f.do(accessWrite, to, true, true, func(rel string) error {
		dst, err := f.root.OpenFile(native(rel), flags|syscall.O_NONBLOCK, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, cerr := io.Copy(dst, src)
		if err := dst.Close(); cerr == nil {
			cerr = err
		}
		if cerr == nil {
			cerr = dst.Chmod(info.Mode().Perm())
			if errors.Is(cerr, fs.ErrClosed) {
				cerr = f.root.Chmod(native(rel), info.Mode().Perm())
			}
		}
		return cerr
	})
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
