//go:build unix

package node

import (
	"io/fs"
	"os"
	"slices"
	"sync"
	"syscall"
)

func rawRead(fd uintptr, p []byte) (int, error) {
	n, err := syscall.Read(int(fd), p)
	if n < 0 {
		n = 0
	}
	return n, err
}

// groups are the process's supplementary groups, read once.
var groups = sync.OnceValue(func() []int {
	g, _ := os.Getgroups()
	return g
})

// checkAccess checks mode against the permission bits of info for the
// process's user, as access(2) does: the owner's bits for the owner, the
// group's for a member, the others' otherwise; root may read and write
// anything, and execute what one of the bits allows.
func checkAccess(info fs.FileInfo, mode int) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	perm := uint32(st.Mode) & 0o777
	uid := os.Getuid()
	if uid == 0 {
		if mode&xOK != 0 && perm&0o111 == 0 && !info.IsDir() {
			return syscall.EACCES
		}
		return nil
	}
	var bits uint32
	switch gid := os.Getgid(); {
	case int(st.Uid) == uid:
		bits = perm >> 6
	case int(st.Gid) == gid || slices.Contains(groups(), int(st.Gid)):
		bits = perm >> 3
	default:
		bits = perm
	}
	if uint32(mode)&^(bits&7) != 0 {
		return syscall.EACCES
	}
	return nil
}
