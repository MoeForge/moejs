//go:build !linux || !(amd64 || arm64)

package node

import "io/fs"

// fastRoot is Linux's openat2 path (fast_linux.go); elsewhere every
// operation goes the os.Root way.
type fastRoot struct{ noCache bool }

func newFastRoot(string) *fastRoot { return nil }

func (*fastRoot) close() {}

func (*fastRoot) invalidate() {}

func (f *fsys) statFast(string, bool) (fs.FileInfo, error, bool) { return nil, nil, false }

func (f *fsys) statFastData(string, bool, *statData) (error, bool) { return nil, false }

func (f *fsys) readFileFast(string, int, int64, func(int64) error) ([]byte, error, bool) {
	return nil, nil, false
}

func (f *fsys) readdirFast(string, bool) ([]dirent, error, bool) { return nil, nil, false }
