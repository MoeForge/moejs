//go:build linux && (amd64 || arm64)

package node

import (
	"io/fs"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// On Linux the hot operations, reading a file, listing a directory and
// stat, resolve their path with openat2(2) and RESOLVE_BENEATH: the
// kernel walks the path in one call and fails one that leaves the root,
// through ".." or a link, with EXDEV. That takes the place of os.Root's
// walk, which opens each directory of the path in turn. An EXDEV, which a
// link with an absolute target under the root gets too, goes the
// os.Root way, which resolves it or fails it as an escape; so does
// everything on a kernel without openat2 (before 5.6, or one a seccomp
// filter denies it).

const sysOpenat2 = 437 // the same on every Linux architecture

// openHow is struct open_how.
type openHow struct {
	flags, mode, resolve uint64
}

const (
	resolveNoMagiclinks = 0x02
	resolveBeneath      = 0x08
	oPath               = 0x200000 // O_PATH
	atSymlinkNofollow   = 0x100    // AT_SYMLINK_NOFOLLOW
)

// fastRoot is a descriptor of the root for openat2. The read lock is held
// while a call uses the descriptor, so that Close does not close it under
// the call and let the number be reused.
type fastRoot struct {
	mu      sync.RWMutex
	fd      int // -1 once closed
	dirs    dirCache
	noCache bool // FSOptions.NoDescriptorCache: each descriptor is used once
}

// The directory cache keeps a descriptor of each directory an operation
// looked in lately, so that a stat or an open of a name in it takes one
// system call (fstatat or openat on the descriptor) rather than an
// openat2, an fstat and a close.
//
// A descriptor is opened with openat2 and RESOLVE_BENEATH, as every path
// is: the directory was beneath the root when it was opened. It is only
// ever used with one name that is neither "." nor "..", without
// following a link: a link as the last component goes the openat2 way
// from the root, which follows it beneath the root. So a descriptor
// reaches the entries of a directory that was inside the root, and
// nothing else. A descriptor binds the directory, not its path: the
// package's own rename, rm, rmdir and unlink empty the cache before they
// return, so that the program sees its changes at once, and an entry
// expires dirTTL after it was opened, so that a change made outside the
// sandbox, a directory renamed or replaced by another process, is seen
// within dirTTL. Until then an operation may still find the directory
// the path named before, which stays one that was inside the root. The
// cache holds at most dirMax entries.
const (
	dirTTL = time.Millisecond
	dirMax = 64
)

type dirCache struct {
	mu      sync.Mutex
	entries map[string]*dirEntry
	gen     uint64 // advanced by each invalidation
}

// dirEntry is a cached descriptor; refs counts the operations using it,
// and the last one closes it once it is dead.
type dirEntry struct {
	fd   int
	at   int64 // when opened, on the cache's clock
	refs int
	dead bool
}

// clock is the cache's monotonic clock.
var clockStart = time.Now()

func clock() int64 { return int64(time.Since(clockStart)) }

// dir returns a descriptor of the directory rel, a clean path below the
// root with no "." or ".." component, cached or opened; release it with
// put.
func (r *fastRoot) dir(rel string) (*dirEntry, error) {
	if r.noCache {
		fd, err := r.open(rel, oPath|syscall.O_DIRECTORY, 0)
		if err != nil {
			return nil, err
		}
		return &dirEntry{fd: fd, refs: 1, dead: true}, nil
	}
	c := &r.dirs
	now := clock()
	c.mu.Lock()
	e := c.entries[rel]
	if e != nil {
		if now-e.at < int64(dirTTL) {
			e.refs++
			c.mu.Unlock()
			return e, nil
		}
		c.kill(rel, e)
	}
	gen := c.gen
	c.mu.Unlock()
	fd, err := r.open(rel, oPath|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	e = &dirEntry{fd: fd, at: now, refs: 1}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != gen {
		// An invalidation ran while it opened: use it this once.
		e.dead = true
		return e, nil
	}
	if old := c.entries[rel]; old != nil {
		c.kill(rel, old)
	}
	if c.entries == nil {
		c.entries = make(map[string]*dirEntry, dirMax)
	}
	if len(c.entries) >= dirMax {
		var oldest string
		var at int64 = 1<<63 - 1
		for k, x := range c.entries {
			if x.at < at {
				oldest, at = k, x.at
			}
		}
		c.kill(oldest, c.entries[oldest])
	}
	c.entries[rel] = e
	return e, nil
}

// put releases a descriptor dir returned.
func (r *fastRoot) put(e *dirEntry) {
	c := &r.dirs
	c.mu.Lock()
	if e.refs--; e.dead && e.refs == 0 {
		syscall.Close(e.fd)
	}
	c.mu.Unlock()
}

// kill drops the entry of rel; c.mu is held.
func (c *dirCache) kill(rel string, e *dirEntry) {
	delete(c.entries, rel)
	e.dead = true
	if e.refs == 0 {
		syscall.Close(e.fd)
	}
}

// invalidate empties the cache, after the package changed the tree.
func (r *fastRoot) invalidate() {
	if r == nil {
		return
	}
	c := &r.dirs
	c.mu.Lock()
	c.gen++
	for k, e := range c.entries {
		c.kill(k, e)
	}
	c.mu.Unlock()
}

// splitName splits rel into its directory and last name when the cache
// may serve it: a path of names, none "." or "..", with no empty one.
// dir is "" for a name of the root itself.
func splitName(rel string) (dir, name string, ok bool) {
	start := 0
	last := -1
	for i := 0; i <= len(rel); i++ {
		if i < len(rel) && rel[i] != '/' {
			continue
		}
		c := rel[start:i]
		if c == "" || c == "." || c == ".." {
			return "", "", false
		}
		if i < len(rel) {
			last = i
		}
		start = i + 1
	}
	if last < 0 {
		return "", rel, true
	}
	return rel[:last], rel[last+1:], true
}

// at runs f with the descriptor of the directory dir ("" for the root).
func (r *fastRoot) at(dir string, f func(dirfd int) error) error {
	if dir == "" {
		r.mu.RLock()
		defer r.mu.RUnlock()
		if r.fd < 0 {
			return errClosed
		}
		return f(r.fd)
	}
	e, err := r.dir(dir)
	if err != nil {
		return err
	}
	defer r.put(e)
	return f(e.fd)
}

// newFastRoot opens dir for openat2, or returns nil when the kernel does
// not have it.
func newFastRoot(dir string) *fastRoot {
	fd, err := syscall.Open(dir, oPath|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil
	}
	r := &fastRoot{fd: fd}
	probe, err := r.open(".", oPath|syscall.O_CLOEXEC, 0)
	if err != nil {
		syscall.Close(fd)
		return nil
	}
	syscall.Close(probe)
	return r
}

func (r *fastRoot) close() {
	if r == nil {
		return
	}
	r.invalidate()
	r.mu.Lock()
	if r.fd >= 0 {
		syscall.Close(r.fd)
		r.fd = -1
	}
	r.mu.Unlock()
}

// open opens rel beneath the root.
func (r *fastRoot) open(rel string, flags int, mode uint32) (int, error) {
	p, err := syscall.BytePtrFromString(rel)
	if err != nil {
		return -1, err
	}
	how := openHow{flags: uint64(flags | syscall.O_CLOEXEC), resolve: resolveBeneath | resolveNoMagiclinks}
	if flags&syscall.O_CREAT != 0 {
		// openat2 refuses a mode without O_CREAT.
		how.mode = uint64(mode)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.fd < 0 {
		return -1, errClosed
	}
	for {
		fd, _, e := syscall.Syscall6(sysOpenat2, uintptr(r.fd), uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&how)), unsafe.Sizeof(how), 0, 0)
		if e == syscall.EINTR {
			continue
		}
		if e != 0 {
			return -1, e
		}
		return int(fd), nil
	}
}

// fallback reports whether an error of open sends the operation the
// os.Root way.
func fallback(err error) bool {
	return err == syscall.EXDEV || err == syscall.ENOSYS || err == syscall.EPERM
}

// statInfo is the fs.FileInfo of a stat the fast path made.
type statInfo struct {
	st syscall.Stat_t
}

func (s *statInfo) Name() string       { return "" }
func (s *statInfo) Size() int64        { return s.st.Size }
func (s *statInfo) Sys() any           { return &s.st }
func (s *statInfo) IsDir() bool        { return s.st.Mode&syscall.S_IFMT == syscall.S_IFDIR }
func (s *statInfo) ModTime() time.Time { return time.Unix(int64(s.st.Mtim.Sec), int64(s.st.Mtim.Nsec)) }
func (s *statInfo) Mode() fs.FileMode  { return fileMode(s.st.Mode) }

// fileMode converts st_mode to a Go file mode.
func fileMode(m uint32) fs.FileMode {
	mode := fs.FileMode(m & 0o777)
	switch m & syscall.S_IFMT {
	case syscall.S_IFDIR:
		mode |= fs.ModeDir
	case syscall.S_IFLNK:
		mode |= fs.ModeSymlink
	case syscall.S_IFIFO:
		mode |= fs.ModeNamedPipe
	case syscall.S_IFSOCK:
		mode |= fs.ModeSocket
	case syscall.S_IFCHR:
		mode |= fs.ModeDevice | fs.ModeCharDevice
	case syscall.S_IFBLK:
		mode |= fs.ModeDevice
	}
	if m&syscall.S_ISUID != 0 {
		mode |= fs.ModeSetuid
	}
	if m&syscall.S_ISGID != 0 {
		mode |= fs.ModeSetgid
	}
	if m&syscall.S_ISVTX != 0 {
		mode |= fs.ModeSticky
	}
	return mode
}

// statFast stats rel; ok is false when the operation must go the os.Root
// way. A name in a cached directory takes one fstatat.
func (f *fsys) statFast(rel string, lstat bool) (_ fs.FileInfo, _ error, ok bool) {
	r := f.fast
	if r == nil {
		return nil, nil, false
	}
	info := &statInfo{}
	if dir, name, ok := splitName(rel); ok {
		var err error
		if dir == "" {
			err = r.at("", func(dirfd int) error { return fstatat(dirfd, name, &info.st, atSymlinkNofollow) })
		} else if e, derr := r.dir(dir); derr != nil {
			err = derr
		} else {
			err = fstatat(e.fd, name, &info.st, atSymlinkNofollow)
			r.put(e)
		}
		if err != nil {
			if fallback(err) {
				return nil, nil, false
			}
			return nil, err, true
		}
		if lstat || info.st.Mode&syscall.S_IFMT != syscall.S_IFLNK {
			return info, nil, true
		}
		// A link, followed from the root, beneath it.
	}
	flags := oPath
	if lstat {
		flags |= syscall.O_NOFOLLOW
	}
	fd, err := r.open(rel, flags, 0)
	if err != nil {
		if fallback(err) {
			return nil, nil, false
		}
		return nil, err, true
	}
	err = syscall.Fstat(fd, &info.st)
	syscall.Close(fd)
	if err != nil {
		return nil, err, true
	}
	return info, nil, true
}

// statFastData is statFast for Node's fields, without an fs.FileInfo.
func (f *fsys) statFastData(rel string, lstat bool, st *statData) (_ error, ok bool) {
	var sys syscall.Stat_t
	err, ok := f.statFastInto(rel, lstat, &sys)
	if ok && err == nil {
		*st = statOfSys(&sys)
	}
	return err, ok
}

// statFastInto stats rel into st. A name in a cached directory takes one
// fstatat.
func (f *fsys) statFastInto(rel string, lstat bool, st *syscall.Stat_t) (_ error, ok bool) {
	r := f.fast
	if r == nil {
		return nil, false
	}
	if dir, name, ok := splitName(rel); ok {
		var err error
		if dir == "" {
			r.mu.RLock()
			if r.fd < 0 {
				err = errClosed
			} else {
				err = fstatat(r.fd, name, st, atSymlinkNofollow)
			}
			r.mu.RUnlock()
		} else if e, derr := r.dir(dir); derr != nil {
			err = derr
		} else {
			err = fstatat(e.fd, name, st, atSymlinkNofollow)
			r.put(e)
		}
		if err != nil {
			if fallback(err) {
				return nil, false
			}
			return err, true
		}
		if lstat || st.Mode&syscall.S_IFMT != syscall.S_IFLNK {
			return nil, true
		}
		// A link, followed from the root, beneath it.
	}
	flags := oPath
	if lstat {
		flags |= syscall.O_NOFOLLOW
	}
	fd, err := r.open(rel, flags, 0)
	if err != nil {
		if fallback(err) {
			return nil, false
		}
		return err, true
	}
	err = syscall.Fstat(fd, st)
	syscall.Close(fd)
	if err != nil {
		return err, true
	}
	return nil, true
}

// openFast opens rel with flags: a name in a cached directory with one
// openat that does not follow it, a link or any other path with openat2
// from the root.
func (f *fsys) openFast(rel string, flags int, mode uint32) (int, error) {
	r := f.fast
	if dir, name, ok := splitName(rel); ok {
		fd := -1
		var err error
		if dir == "" {
			err = r.at("", func(dirfd int) error {
				var err error
				fd, err = openat(dirfd, name, flags|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, mode)
				return err
			})
		} else if e, derr := r.dir(dir); derr != nil {
			err = derr
		} else {
			fd, err = openat(e.fd, name, flags|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, mode)
			r.put(e)
		}
		switch err {
		case nil:
			return fd, nil
		case syscall.ELOOP, syscall.ENOTDIR:
			// The name may be a link (O_NOFOLLOW fails it with ELOOP,
			// or with ENOTDIR under O_DIRECTORY): openat2 follows it.
		default:
			return -1, err
		}
	}
	return r.open(rel, flags, mode)
}

// namePtr returns name NUL-terminated, for the kernel: in buf when it
// fits, so that a short name needs no allocation.
func namePtr(buf *[128]byte, name string) (*byte, error) {
	if len(name) < len(buf) {
		copy(buf[:], name)
		buf[len(name)] = 0
		return &buf[0], nil
	}
	return syscall.BytePtrFromString(name)
}

// fstatat stats name in the directory dirfd.
func fstatat(dirfd int, name string, st *syscall.Stat_t, flags int) error {
	var buf [128]byte
	p, err := namePtr(&buf, name)
	if err != nil {
		return err
	}
	for {
		_, _, e := syscall.Syscall6(sysFstatat, uintptr(dirfd), uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(st)), uintptr(flags), 0, 0)
		if e == syscall.EINTR {
			continue
		}
		if e != 0 {
			return e
		}
		return nil
	}
}

// openat opens name in the directory dirfd.
func openat(dirfd int, name string, flags int, mode uint32) (int, error) {
	var buf [128]byte
	p, err := namePtr(&buf, name)
	if err != nil {
		return -1, err
	}
	for {
		fd, _, e := syscall.Syscall6(syscall.SYS_OPENAT, uintptr(dirfd), uintptr(unsafe.Pointer(p)), uintptr(flags), uintptr(mode), 0, 0)
		if e == syscall.EINTR {
			continue
		}
		if e != 0 {
			return -1, e
		}
		return int(fd), nil
	}
}

// readFileFast is readFile's work the fast way.
func (f *fsys) readFileFast(rel string, flags int, limit int64, reserve func(int64) error) (_ []byte, _ error, ok bool) {
	if f.fast == nil {
		return nil, nil, false
	}
	fd, err := f.openFast(rel, flags|syscall.O_NONBLOCK, 0o666)
	if err != nil {
		if fallback(err) {
			return nil, nil, false
		}
		return nil, err, true
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return nil, &sysError{code: errnoCode(err), syscall: "fstat"}, true
	}
	if st.Mode&syscall.S_IFMT == syscall.S_IFDIR {
		return nil, &sysError{code: "EISDIR", syscall: "read"}, true
	}
	size := st.Size
	if size > maxFileSize {
		return nil, &tooLargeError{size: size}, true
	}
	if limit > 0 && size > limit {
		return nil, &tooLargeError{size: size, limit: limit}, true
	}
	if reserve != nil {
		n := size
		if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
			n = 0
		}
		if err := reserve(n); err != nil {
			return nil, err, true
		}
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG || size == 0 {
		data, err := readToEnd(fdReader(fd), limit)
		return data, err, true
	}
	buf := make([]byte, size)
	n := 0
	for n < len(buf) {
		m, err := syscall.Read(fd, buf[n:])
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return nil, &sysError{code: errnoCode(err), syscall: "read"}, true
		}
		if m == 0 {
			break
		}
		n += m
	}
	return buf[:n], nil, true
}

// fdReader reads a descriptor; a read with nothing to read on a
// non-blocking one fails with EAGAIN.
type fdReader int

func (r fdReader) Read(p []byte) (int, error) {
	for {
		n, err := syscall.Read(int(r), p)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n == 0 && len(p) > 0 {
			return 0, eof
		}
		return n, nil
	}
}

// readdirFast lists rel the fast way, with the types the system gives;
// with types, it stats those it does not give.
func (f *fsys) readdirFast(rel string, types bool) (_ []dirent, _ error, ok bool) {
	if f.fast == nil {
		return nil, nil, false
	}
	fd, err := f.openFast(rel, syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		if fallback(err) {
			return nil, nil, false
		}
		return nil, err, true
	}
	defer syscall.Close(fd)
	var out []dirent
	buf := make([]byte, 16<<10)
	for {
		n, err := syscall.ReadDirent(fd, buf)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return nil, err, true
		}
		if n <= 0 {
			break
		}
		for b := buf[:n]; len(b) >= 19; {
			reclen := int(*(*uint16)(unsafe.Pointer(&b[16])))
			if reclen < 19 || reclen > len(b) {
				break
			}
			typ := b[18]
			name := b[19:reclen]
			for i, c := range name {
				if c == 0 {
					name = name[:i]
					break
				}
			}
			b = b[reclen:]
			if len(name) == 0 || (name[0] == '.' && (len(name) == 1 || len(name) == 2 && name[1] == '.')) {
				continue
			}
			d := dirent{name: string(name), typ: direntMode(typ)}
			if d.typ == typeUnknown && types {
				var st syscall.Stat_t
				if err := fstatat(fd, d.name, &st, atSymlinkNofollow); err == nil {
					d.typ = fileMode(st.Mode).Type()
				}
			}
			out = append(out, d)
		}
	}
	sortDirents(out)
	return out, nil, true
}

// direntMode converts a d_type.
func direntMode(t byte) fs.FileMode {
	switch t {
	case syscall.DT_REG:
		return 0
	case syscall.DT_DIR:
		return fs.ModeDir
	case syscall.DT_LNK:
		return fs.ModeSymlink
	case syscall.DT_FIFO:
		return fs.ModeNamedPipe
	case syscall.DT_SOCK:
		return fs.ModeSocket
	case syscall.DT_CHR:
		return fs.ModeDevice | fs.ModeCharDevice
	case syscall.DT_BLK:
		return fs.ModeDevice
	}
	return typeUnknown
}
