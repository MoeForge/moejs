package node

import (
	"errors"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// The Windows path forms, as Node's path.win32 reads them:
// drive-absolute (C:\x), UNC (\\server\share\x), rooted (\x), which
// takes the working directory's volume, drive-relative (C:x) and
// relative paths, either separator. A path is cleaned lexically, as
// Windows and os.Root do, before the mount check, which ignores case.
// The functions take paths with forward slashes, which they return; the
// paths JavaScript sees have backslashes. They are used on Windows, and
// in the tests elsewhere: fsys.win32 selects them at run time.

// win32Root returns the volume of p: its drive, as spelt ("C:"), or its
// UNC share ("//server/share"), and the length of p it takes; "" for a
// rooted or relative path, or a UNC path without a share.
func win32Root(p string) (string, int) {
	if len(p) >= 2 && win32IsDrive(p[0]) && p[1] == ':' {
		return p[:2], 2
	}
	if len(p) < 3 || p[0] != '/' || p[1] != '/' || p[2] == '/' {
		return "", 0
	}
	i := 2
	for i < len(p) && p[i] != '/' {
		i++
	}
	j := i
	for j < len(p) && p[j] == '/' {
		j++
	}
	k := j
	for k < len(p) && p[k] != '/' {
		k++
	}
	if k == j {
		return "", 0
	}
	if j == i+1 {
		return p[:k], k
	}
	return p[:i] + "/" + p[j:k], k
}

func win32IsDrive(c byte) bool { return 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' }

// win32IsAbs reports whether p is absolute: drive-absolute, UNC or
// rooted.
func win32IsAbs(p string) bool {
	return strings.HasPrefix(p, "/") || len(p) >= 3 && win32IsDrive(p[0]) && p[1] == ':' && p[2] == '/'
}

// win32Undevice returns the drive or UNC path that a \\?\ or \\.\ path
// names (\\?\C:\x is C:\x, \\?\UNC\server\share\x is \\server\share\x),
// as Node's toNamespacedPath makes them; ok is false for the other
// device paths (\\.\pipe\x, \\?\Volume{...}\x), which are not supported.
func win32Undevice(p string) (string, bool) {
	if len(p) < 3 || p[0] != '/' || p[1] != '/' || p[2] != '.' && p[2] != '?' || len(p) > 3 && p[3] != '/' {
		return p, true
	}
	var rest string
	if len(p) > 4 {
		rest = p[4:]
	}
	switch {
	case len(rest) >= 3 && win32IsDrive(rest[0]) && rest[1] == ':' && rest[2] == '/':
		return rest, true
	case len(rest) >= 4 && strings.EqualFold(rest[:3], "UNC") && rest[3] == '/':
		return "/" + rest[3:], true
	}
	return "", false
}

// win32Resolve returns p resolved against cwd, an absolute clean path, as
// path.win32.resolve does it: a rooted path takes cwd's volume (none for
// a drive-less mount like /workspace), a drive-relative one is relative
// to cwd on cwd's drive and to the drive's root on another.
func win32Resolve(p, cwd string) string {
	if vol, n := win32Root(p); vol != "" {
		rest := p[n:]
		if vol[0] == '/' || strings.HasPrefix(rest, "/") {
			return vol + rest
		}
		if cvol, _ := win32Root(cwd); strings.EqualFold(cvol, vol) {
			return win32Join(cwd, rest)
		}
		return vol + "/" + rest
	}
	if strings.HasPrefix(p, "/") {
		cvol, _ := win32Root(cwd)
		return cvol + p
	}
	return win32Join(cwd, p)
}

// win32Clean cleans the absolute path p lexically: "." and empty names
// go, ".." takes the name before it away, and none above the root. The
// result is the volume, "/" and the names: C:/x, //server/share/x, /x,
// and C:/ for a root.
func win32Clean(p string) string {
	vol, n := win32Root(p)
	rest := p[n:]
	buf := make([]byte, 0, len(vol)+1+len(rest))
	buf = append(buf, vol...)
	buf = append(buf, '/')
	root := len(buf)
	for i := 0; i < len(rest); {
		for i < len(rest) && rest[i] == '/' {
			i++
		}
		j := i
		for j < len(rest) && rest[j] != '/' {
			j++
		}
		switch seg := rest[i:j]; seg {
		case "", ".":
		case "..":
			k := root
			for m := len(buf) - 1; m >= root; m-- {
				if buf[m] == '/' {
					k = m
					break
				}
			}
			buf = buf[:k]
		default:
			if len(buf) > root {
				buf = append(buf, '/')
			}
			buf = append(buf, seg...)
		}
		i = j
	}
	return string(buf)
}

// win32Under returns the part of p below dir, both absolute and clean,
// and whether p is dir or below it. The volume and the names are
// compared without case, as Windows' file systems do by default.
func win32Under(dir, p string) (string, bool) {
	dvol, dn := win32Root(dir)
	pvol, pn := win32Root(p)
	if !strings.EqualFold(dvol, pvol) {
		return "", false
	}
	d, q := dir[dn+1:], p[pn+1:]
	for d != "" {
		ds, dr, _ := strings.Cut(d, "/")
		qs, qr, _ := strings.Cut(q, "/")
		if qs == "" || !strings.EqualFold(ds, qs) {
			return "", false
		}
		d, q = dr, qr
	}
	return q, true
}

// win32Sep returns p with Windows' separator.
func win32Sep(p string) string { return strings.ReplaceAll(p, "/", `\`) }

// win32Slash returns p with forward slashes.
func win32Slash(p string) string { return strings.ReplaceAll(p, `\`, "/") }

// win32Normalize returns p as path.win32.normalize does: backslashes,
// "." and ".." resolved, ".." kept where a relative path starts with it,
// the volume as spelt. Node joins the paths it builds with it (join), so
// that the Dirents and the names of a listing get these forms.
func win32Normalize(p string) string {
	if p == "" {
		return "."
	}
	p = win32Slash(p)
	vol, n := win32Root(p)
	rest := p[n:]
	abs := vol != "" && vol[0] == '/' || strings.HasPrefix(rest, "/")
	var tail string
	if abs {
		tail = path.Clean("/" + rest)[1:]
	} else {
		tail = path.Clean(rest)
	}
	if tail != "" && strings.HasSuffix(p, "/") {
		tail += "/"
	}
	switch {
	case abs:
		return win32Sep(vol + "/" + tail)
	default:
		return win32Sep(vol + tail)
	}
}

// win32Dir and win32Base split p, a path with backslashes as
// win32Normalize returns them, as path.win32's dirname and basename do.
func win32Dir(p string) string {
	vol, n := win32Root(win32Slash(p))
	rest := p[n:]
	i := strings.LastIndexByte(rest, '\\')
	switch {
	case i > 0:
		return p[:n+i]
	case i == 0:
		return p[:n+1]
	case vol != "":
		return vol
	}
	return "."
}

func win32Base(p string) string {
	_, n := win32Root(win32Slash(p))
	rest := p[n:]
	return rest[strings.LastIndexByte(rest, '\\')+1:]
}

// win32Namespaced returns p, an absolute path with backslashes, as Node's
// toNamespacedPath does: \\?\C:\x for a drive, \\?\UNC\server\share\x for
// a share; a drive-less path is left as it is.
func win32Namespaced(p string) string {
	switch {
	case len(p) >= 3 && win32IsDrive(p[0]) && p[1] == ':' && p[2] == '\\':
		return `\\?\` + p
	case strings.HasPrefix(p, `\\`):
		return `\\?\UNC\` + p[2:]
	}
	return p
}

// openWin32 sets the mount and the working directory from o on Windows:
// Mount is any absolute form, the root's own path when it is empty; Cwd
// is absolute or relative to Mount, and under it.
func (f *fsys) openWin32(o *FSOptions) error {
	mnt := f.hostRoot
	if o.Mount != "" {
		m, ok := win32Undevice(win32Slash(o.Mount))
		if !ok || !win32IsAbs(m) {
			return errors.New("node: FSOptions.Mount " + o.Mount + " is not an absolute path")
		}
		mnt = m
	}
	f.mount = win32Clean(mnt)
	cwd := f.mount
	if o.Cwd != "" {
		c, ok := win32Undevice(win32Slash(o.Cwd))
		if ok {
			cwd = win32Clean(win32Resolve(c, f.mount))
		}
		if !ok || cwd == "" {
			return errors.New("node: FSOptions.Cwd " + o.Cwd + " is not under the mount " + win32Sep(f.mount))
		}
	}
	rest, ok := win32Under(f.mount, cwd)
	if !ok {
		return errors.New("node: FSOptions.Cwd " + o.Cwd + " is not under the mount " + win32Sep(f.mount))
	}
	f.cwdRel = rest
	f.cwdAbs = win32Join(f.mount, rest)
	return nil
}

// win32Join joins the clean absolute dir, as the package keeps it with
// forward slashes, and rel.
func win32Join(dir, rel string) string {
	switch {
	case rel == "" || rel == ".":
		return dir
	case strings.HasSuffix(dir, "/"):
		return dir + rel
	}
	return dir + "/" + rel
}

// relWin32 is rel on Windows: p resolved against the working directory
// and cleaned, as Node's toNamespacedPath does before libuv sees it, then
// taken below the mount, without case. A \\?\ or \\.\ device path other
// than a drive's or a share's is outside.
func (f *fsys) relWin32(p string) (string, error) {
	p, ok := win32Undevice(win32Slash(p))
	if !ok {
		return "", errEscape
	}
	rest, ok := win32Under(f.mount, win32Clean(win32Resolve(p, f.cwdAbs)))
	if !ok {
		return "", errEscape
	}
	if rest == "" {
		return ".", nil
	}
	return rest, nil
}

// jsPathWin32 is jsPath on Windows: the mount as spelt, backslashes.
func (f *fsys) jsPathWin32(rel string) string {
	return win32Sep(win32Join(f.mount, rel))
}

// hostUnderWin32 returns the part below the root of the absolute link
// target t, without case, and whether it is below it.
func (f *fsys) hostUnderWin32(t string) (string, bool) {
	t, ok := win32Undevice(win32Slash(t))
	if !ok {
		return "", false
	}
	return win32Under(win32Clean(f.hostRoot), win32Clean(t))
}

// linkTarget reports whether the target t of a link, with forward
// slashes, is absolute, and if it is, its part below the root and
// whether it is below it.
func (f *fsys) linkTarget(t string) (abs bool, rest string, ok bool) {
	if f.win32 {
		if !win32IsAbs(t) && !(len(t) >= 2 && win32IsDrive(t[0]) && t[1] == ':') {
			return false, "", false
		}
		rest, ok = f.hostUnderWin32(t)
		return true, rest, ok
	}
	if !filepath.IsAbs(t) && !strings.HasPrefix(t, "/") {
		return false, "", false
	}
	rest, ok = under(f.hostRoot, path.Clean(t))
	return true, rest, ok
}

// fileURLPathWin32 returns the path of a file: URL as Node's
// getPathFromURLWin32 does: a host names a share, else the
// path needs a drive.
func fileURLPathWin32(u *url.URL) (string, string) {
	raw := strings.ToLower(u.EscapedPath())
	if strings.Contains(raw, "%2f") || strings.Contains(raw, "%5c") {
		return "", `File URL path must not include encoded \ or / characters`
	}
	p := win32Sep(u.Path)
	// The URL parser drops a "localhost" host of a file: URL.
	if host := strings.ToLower(u.Hostname()); host != "" && host != "localhost" {
		return `\\` + host + p, ""
	}
	if len(p) < 3 || !win32IsDrive(p[1]) || p[2] != ':' {
		return "", "File URL path must be absolute"
	}
	return p[1:], ""
}

// joinJS joins p, a directory as JavaScript gave it, and dir, a path
// below it with forward slashes, as Node's path.join does: path.win32's
// on Windows.
func (f *fsys) joinJS(p, dir string) string {
	if f.win32 {
		return win32Normalize(p + "/" + dir)
	}
	return path.Join(p, dir)
}

// subName returns the name of a recursive listing for name in dir, a path
// with forward slashes, with the system's separator.
func (f *fsys) subName(dir, name string) string {
	if f.win32 {
		return win32Sep(dir) + `\` + name
	}
	return dir + "/" + name
}

// globPath returns the match p of a glob, with forward slashes, as
// JavaScript sees it: with backslashes on Windows, where Node joins them
// with path.win32.
func (f *fsys) globPath(p string) string {
	if f.win32 {
		return win32Normalize(p)
	}
	return p
}

// globFull returns the match p of a glob with cwd, on Windows, as Node's
// Dirents of a glob have it: p when it is absolute, else joined to cwd.
func (f *fsys) globFull(cwd, p string) string {
	if win32IsAbs(p) {
		return win32Normalize(p)
	}
	return win32Normalize(cwd + "/" + p)
}

// listError returns err, the error of listing a directory, as libuv
// reports it: a file's is ENOTDIR on Windows too.
func (f *fsys) listError(err error) error {
	if f.win32 && errors.Is(err, syscall.ENOTDIR) {
		return errNotDir
	}
	return err
}

// dirJS returns the directory of p, a path as JavaScript gave it, as
// Node's path.dirname does: path.win32's on Windows.
func (f *fsys) dirJS(p string) string {
	if f.win32 {
		return win32Dir(win32Normalize(p))
	}
	return path.Dir(p)
}

// win32IsVolume reports whether p, with forward slashes, is a volume or
// its root: C:, C:/, //server/share.
func win32IsVolume(p string) bool {
	vol, n := win32Root(p)
	return vol != "" && (n == len(p) || n == len(p)-1 && p[n] == '/')
}
