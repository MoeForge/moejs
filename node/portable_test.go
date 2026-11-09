package node_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/MoeForge/moejs/node"
	"github.com/stretchr/testify/require"
)

// TestNativePortable runs the package over a temporary directory with
// the system's own absolute path of it as the mount: on Windows a drive
// letter path, where both separators work.
func TestNativePortable(t *testing.T) {
	root := realDirT(t)
	abs, err := filepath.Abs(root)
	require.NoError(t, err)
	h := newHarness(t, node.Options{FS: &node.FSOptions{Root: abs, Mount: abs, Write: true}})
	h.write(map[string]string{"a.txt": "a", "sub/b.txt": "b", "sub/c.js": "c"})
	win := runtime.GOOS == "windows"
	// The absolute forms of a path: the native one, and on Windows the
	// one with forward slashes.
	forms := func(rel string) []string {
		native := filepath.Join(abs, filepath.FromSlash(rel))
		if win {
			return []string{native, filepath.ToSlash(native)}
		}
		return []string{native}
	}
	sep := string(filepath.Separator)
	native := func(rel string) string { return filepath.Join(abs, filepath.FromSlash(rel)) }

	for i, p := range forms("w.txt") {
		body := "w" + strconv.Itoa(i)
		h.run(`fs.writeFileSync(` + jsq(p) + `, ` + jsq(body) + `)`)
		data, err := os.ReadFile(native("w.txt"))
		require.NoError(t, err)
		require.Equal(t, body, string(data))
		require.Equal(t, body, h.run(`return fs.readFileSync(`+jsq(p)+`, "utf8")`))
	}
	h.run(`fs.writeFileSync("sub/d.txt", "d")`)
	if win {
		h.run(`fs.writeFileSync("sub\\e.txt", "e")`)
		require.Equal(t, "e", h.run(`return fs.readFileSync("sub\\e.txt", "utf8")`))
		h.run(`fs.rmSync("sub\\e.txt")`)
	}
	require.Equal(t, "a", h.run(`return fs.readFileSync("a.txt", "utf8")`))
	require.Equal(t, "b", h.run(`return fs.readFileSync("sub/b.txt", "utf8")`))

	for _, p := range append(forms("sub"), "sub") {
		require.Equal(t, `["b.txt","c.js","d.txt"]`, h.run(`return fs.readdirSync(`+jsq(p)+`)`), p)
	}
	want := `["a.txt","sub","w.txt","sub` + strings.ReplaceAll(sep, `\`, `\\`) + `b.txt","sub` + strings.ReplaceAll(sep, `\`, `\\`) + `c.js","sub` + strings.ReplaceAll(sep, `\`, `\\`) + `d.txt"]`
	for _, p := range append(forms("."), ".") {
		require.Equal(t, want, h.run(`return fs.readdirSync(`+jsq(p)+`, {recursive: true})`), p)
	}

	for _, p := range append(forms("sub/b.txt"), "sub/b.txt") {
		require.Equal(t, native("sub/b.txt"), h.run(`return fs.realpathSync(`+jsq(p)+`)`), p)
		require.Equal(t, native("sub/b.txt"), h.run(`return await fsp.realpath(`+jsq(p)+`)`), p)
	}
	require.Equal(t, abs, h.run(`return fs.realpathSync(".")`))

	require.Equal(t, `["a.txt","w.txt"]`, h.run(`return fs.globSync("*.txt")`))
	require.Equal(t, `["sub`+strings.ReplaceAll(sep, `\`, `\\`)+`b.txt","sub`+strings.ReplaceAll(sep, `\`, `\\`)+`d.txt"]`, h.run(`return fs.globSync("sub/*.txt")`))
	for _, p := range forms("sub") {
		res := h.run(`return fs.globSync(` + jsq(p+"/*.txt") + `)`)
		require.Equal(t, `[`+jsq(native("sub/b.txt"))+`,`+jsq(native("sub/d.txt"))+`]`, res, p)
		res = h.run(`return fs.globSync("*.js", {cwd: ` + jsq(p) + `})`)
		require.Equal(t, `["c.js"]`, res, p)
	}

	for i, p := range forms("m/x/y") {
		top := "m" + strconv.Itoa(i)
		p = strings.Replace(p, string(filepath.Separator)+"m"+string(filepath.Separator), string(filepath.Separator)+top+string(filepath.Separator), 1)
		p = strings.Replace(p, "/m/", "/"+top+"/", 1)
		first := h.run(`return fs.mkdirSync(` + jsq(p) + `, {recursive: true})`)
		wantFirst := native(top)
		if win {
			wantFirst = `\\?\` + wantFirst
		} else {
			wantFirst = p[:len(p)-len("/x/y")]
		}
		require.Equal(t, wantFirst, first, p)
		info, err := os.Stat(native(top + "/x/y"))
		require.NoError(t, err)
		require.True(t, info.IsDir())
	}
	h.run(`fs.mkdirSync("r/s", {recursive: true})`)
	_, err = os.Stat(native("r/s"))
	require.NoError(t, err)

	forms2 := forms("w.txt")
	h.run(`fs.renameSync(` + jsq(forms2[len(forms2)-1]) + `, "renamed.txt")`)
	_, err = os.Stat(native("renamed.txt"))
	require.NoError(t, err)
	_, err = os.Stat(native("w.txt"))
	require.True(t, os.IsNotExist(err))
	h.run(`fs.renameSync("renamed.txt", ` + jsq(native("w.txt")) + `)`)
	_, err = os.Stat(native("w.txt"))
	require.NoError(t, err)

	for _, p := range append(forms("a.txt"), "a.txt") {
		require.Equal(t, "true 1", h.run(`const s = fs.statSync(`+jsq(p)+`); return s.isFile() + " " + s.size`), p)
	}
	require.Equal(t, "true", h.run(`return fs.statSync(`+jsq(native("sub"))+`).isDirectory()`))
	require.Equal(t, "ENOENT", h.run(`try { fs.statSync(`+jsq(native("nope"))+`) } catch (e) { return e.code }`))

	fs2 := forms("w.txt")
	h.run(`fs.rmSync(` + jsq(fs2[len(fs2)-1]) + `)`)
	_, err = os.Stat(native("w.txt"))
	require.True(t, os.IsNotExist(err))
	h.run(`fs.rmSync(` + jsq(native("m0")) + `, {recursive: true})`)
	_, err = os.Stat(native("m0"))
	require.True(t, os.IsNotExist(err))
	h.run(`fs.rmSync("r", {recursive: true})`)
	_, err = os.Stat(native("r"))
	require.True(t, os.IsNotExist(err))

	// Links, where the system lets the test make them.
	if err := os.Symlink("a.txt", native("probe")); err != nil {
		t.Logf("no symlinks: %v", err)
		return
	}
	require.NoError(t, os.Remove(native("probe")))
	require.NoError(t, os.Symlink("a.txt", native("link")))
	require.NoError(t, os.Symlink(native("sub/b.txt"), native("abs-link")))
	require.Equal(t, "a", h.run(`return fs.readFileSync("link", "utf8")`))
	require.Equal(t, "b", h.run(`return fs.readFileSync("abs-link", "utf8")`))
	require.Equal(t, "a.txt", h.run(`return fs.readlinkSync("link")`))
	require.Equal(t, native("sub/b.txt"), h.run(`return fs.readlinkSync("abs-link")`))
	require.Equal(t, native("a.txt"), h.run(`return fs.realpathSync(`+jsq(native("link"))+`)`))
	require.Equal(t, native("sub/b.txt"), h.run(`return fs.realpathSync("abs-link")`))
}

// TestErrnoMapping checks the Windows error code mapping against libuv's
// uv_translate_sys_error.
func TestErrnoMapping(t *testing.T) {
	// The cases of uv_translate_sys_error in libuv v1.52.1 src/win/error.c,
	// with the values of Go's ERROR_* and WSA* constants.
	cases := []struct {
		codes []uint32
		name  string
	}{
		{[]uint32{10013, 740, 1920}, "EACCES"},
		{[]uint32{1227, 10048}, "EADDRINUSE"},
		{[]uint32{10049}, "EADDRNOTAVAIL"},
		{[]uint32{10047}, "EAFNOSUPPORT"},
		{[]uint32{10035, 232}, "EAGAIN"},
		{[]uint32{10037}, "EALREADY"},
		{[]uint32{1004, 6}, "EBADF"},
		{[]uint32{33, 231, 32}, "EBUSY"},
		{[]uint32{995, 10004}, "ECANCELED"},
		{[]uint32{1113}, "ECHARSET"},
		{[]uint32{1236, 10053}, "ECONNABORTED"},
		{[]uint32{1225, 10061}, "ECONNREFUSED"},
		{[]uint32{64, 10054}, "ECONNRESET"},
		{[]uint32{183, 80}, "EEXIST"},
		{[]uint32{998, 10014}, "EFAULT"},
		{[]uint32{1232, 10065}, "EHOSTUNREACH"},
		{[]uint32{122, 13, 87, 1464, 10022, 10046}, "EINVAL"},
		{[]uint32{1102, 1111, 23, 1166, 1165, 1393, 1129, 1101, 31, 1106, 1117, 1104, 205, 110, 1103, 156}, "EIO"},
		{[]uint32{10056}, "EISCONN"},
		{[]uint32{1921}, "ELOOP"},
		{[]uint32{4, 10024}, "EMFILE"},
		{[]uint32{10040}, "EMSGSIZE"},
		{[]uint32{111, 206}, "ENAMETOOLONG"},
		{[]uint32{1231, 10051}, "ENETUNREACH"},
		{[]uint32{10055}, "ENOBUFS"},
		{[]uint32{161, 267, 203, 2, 123, 15, 4392, 126, 3, 11001, 11004}, "ENOENT"},
		{[]uint32{8, 14}, "ENOMEM"},
		{[]uint32{82, 112, 277, 1100, 39}, "ENOSPC"},
		{[]uint32{2250, 10057}, "ENOTCONN"},
		{[]uint32{145}, "ENOTEMPTY"},
		{[]uint32{10038}, "ENOTSOCK"},
		{[]uint32{50}, "ENOTSUP"},
		{[]uint32{109}, "EOF"},
		{[]uint32{5, 1314}, "EPERM"},
		{[]uint32{230, 233, 10058}, "EPIPE"},
		{[]uint32{10043}, "EPROTONOSUPPORT"},
		{[]uint32{19}, "EROFS"},
		{[]uint32{121, 10060}, "ETIMEDOUT"},
		{[]uint32{17}, "EXDEV"},
		{[]uint32{1}, "EISDIR"},
		{[]uint32{208}, "E2BIG"},
		{[]uint32{10044}, "ESOCKTNOSUPPORT"},
		{[]uint32{193}, "EFTYPE"},
	}
	for _, c := range cases {
		for _, code := range c.codes {
			got := node.WinSysErrorCode(code)
			require.Equal(t, c.name, got, "code %d", code)
		}
	}
	// The codes not in the switch: UNKNOWN
	require.Equal(t, "UNKNOWN", node.WinSysErrorCode(999))
	require.Equal(t, "UNKNOWN", node.WinSysErrorCode(12345))
}

// TestErrnoNumbers checks the Windows errno numbers against libuv's
// values, from errno.h or a list noting the version.
func TestErrnoNumbers(t *testing.T) {
	// MOEJS_UV_ERRNO_H names libuv's uv/errno.h, as Node's headers
	// install it (include/node/uv/errno.h); without it, the list below.
	var exp map[string]int
	if hdr := os.Getenv("MOEJS_UV_ERRNO_H"); hdr != "" {
		var err error
		exp, err = parseUVErrnoH(hdr)
		require.NoError(t, err)
	} else {
		exp = uvErrnoNumbers_1_52_1
	}
	got := node.WinErrnoNumbers()
	for name, n := range exp {
		require.Equal(t, n, got[name], name)
	}
	for name := range got {
		_, ok := exp[name]
		require.True(t, ok, "unexpected "+name)
	}
	require.Len(t, got, len(exp))
}

// parseUVErrnoH returns the Windows codes of libuv's uv/errno.h: the
// last literal value each UV__ name is defined to, which is the #else
// branch Windows takes. The getaddrinfo codes (UV__EAI_*) are left out.
func parseUVErrnoH(name string) (map[string]int, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	m := map[string]int{}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "#"))
		if len(f) != 3 || f[0] != "define" || !strings.HasPrefix(f[1], "UV__") || strings.HasPrefix(f[1], "UV__EAI_") {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(f[2], "(-%d)", &n); err == nil {
			m[strings.TrimPrefix(f[1], "UV__")] = n
		}
	}
	return m, nil
}

// uvErrnoNumbers_1_52_1 are the Windows codes of libuv 1.52.1's
// uv/errno.h, negated, for the test without the header.
var uvErrnoNumbers_1_52_1 = map[string]int{
	"E2BIG": 4093, "EACCES": 4092, "EADDRINUSE": 4091, "EADDRNOTAVAIL": 4090,
	"EAFNOSUPPORT": 4089, "EAGAIN": 4088, "EALREADY": 4084, "EBADF": 4083,
	"EBUSY": 4082, "ECANCELED": 4081, "ECHARSET": 4080, "ECONNABORTED": 4079,
	"ECONNREFUSED": 4078, "ECONNRESET": 4077, "EDESTADDRREQ": 4076, "EEXIST": 4075,
	"EFAULT": 4074, "EHOSTUNREACH": 4073, "EINTR": 4072, "EINVAL": 4071,
	"EIO": 4070, "EISCONN": 4069, "EISDIR": 4068, "ELOOP": 4067,
	"EMFILE": 4066, "EMSGSIZE": 4065, "ENAMETOOLONG": 4064, "ENETDOWN": 4063,
	"ENETUNREACH": 4062, "ENFILE": 4061, "ENOBUFS": 4060, "ENODEV": 4059,
	"ENOENT": 4058, "ENOMEM": 4057, "ENONET": 4056, "ENOSPC": 4055,
	"ENOSYS": 4054, "ENOTCONN": 4053, "ENOTDIR": 4052, "ENOTEMPTY": 4051,
	"ENOTSOCK": 4050, "ENOTSUP": 4049, "EPERM": 4048, "EPIPE": 4047,
	"EPROTO": 4046, "EPROTONOSUPPORT": 4045, "EPROTOTYPE": 4044, "EROFS": 4043,
	"ESHUTDOWN": 4042, "ESPIPE": 4041, "ESRCH": 4040, "ETIMEDOUT": 4039,
	"ETXTBSY": 4038, "EXDEV": 4037, "EFBIG": 4036, "ENOPROTOOPT": 4035,
	"ERANGE": 4034, "ENXIO": 4033, "EMLINK": 4032, "EHOSTDOWN": 4031,
	"EREMOTEIO": 4030, "ENOTTY": 4029, "EFTYPE": 4028, "EILSEQ": 4027,
	"EOVERFLOW": 4026, "ESOCKTNOSUPPORT": 4025, "ENODATA": 4024, "EUNATCH": 4023,
	"ENOEXEC": 4022, "EOF": 4095, "UNKNOWN": 4094,
}
