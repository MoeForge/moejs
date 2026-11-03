package node

import (
	"io/fs"
	"syscall"
)

// statOf returns Node's fields of info. Linux's stat has no birth time:
// birthtimeMs is ctimeMs, as libuv gives it without statx.
func statOf(info fs.FileInfo) statData {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return genericStat(info)
	}
	ctime := ms(int64(st.Ctim.Sec), int64(st.Ctim.Nsec))
	return statData{
		dev: float64(st.Dev), mode: float64(st.Mode), nlink: float64(st.Nlink), uid: float64(st.Uid),
		gid: float64(st.Gid), rdev: float64(st.Rdev), blksize: float64(st.Blksize), ino: float64(st.Ino),
		size: float64(st.Size), blocks: float64(st.Blocks),
		atimeMs: ms(int64(st.Atim.Sec), int64(st.Atim.Nsec)), mtimeMs: ms(int64(st.Mtim.Sec), int64(st.Mtim.Nsec)),
		ctimeMs: ctime, birthtimeMs: ctime,
	}
}

// statOfSys returns Node's fields of st, as statOf does.
func statOfSys(st *syscall.Stat_t) statData {
	ctime := ms(int64(st.Ctim.Sec), int64(st.Ctim.Nsec))
	return statData{
		dev: float64(st.Dev), mode: float64(st.Mode), nlink: float64(st.Nlink), uid: float64(st.Uid),
		gid: float64(st.Gid), rdev: float64(st.Rdev), blksize: float64(st.Blksize), ino: float64(st.Ino),
		size: float64(st.Size), blocks: float64(st.Blocks),
		atimeMs: ms(int64(st.Atim.Sec), int64(st.Atim.Nsec)), mtimeMs: ms(int64(st.Mtim.Sec), int64(st.Mtim.Nsec)),
		ctimeMs: ctime, birthtimeMs: ctime,
	}
}
