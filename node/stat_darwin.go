package node

import (
	"io/fs"
	"syscall"
)

// statOf returns Node's fields of info.
func statOf(info fs.FileInfo) statData {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return genericStat(info)
	}
	return statData{
		dev: float64(st.Dev), mode: float64(st.Mode), nlink: float64(st.Nlink), uid: float64(st.Uid),
		gid: float64(st.Gid), rdev: float64(st.Rdev), blksize: float64(st.Blksize), ino: float64(st.Ino),
		size: float64(st.Size), blocks: float64(st.Blocks),
		atimeMs:     ms(int64(st.Atimespec.Sec), int64(st.Atimespec.Nsec)),
		mtimeMs:     ms(int64(st.Mtimespec.Sec), int64(st.Mtimespec.Nsec)),
		ctimeMs:     ms(int64(st.Ctimespec.Sec), int64(st.Ctimespec.Nsec)),
		birthtimeMs: ms(int64(st.Birthtimespec.Sec), int64(st.Birthtimespec.Nsec)),
	}
}
