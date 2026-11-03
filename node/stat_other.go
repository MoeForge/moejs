//go:build !linux && !darwin

package node

import "io/fs"

// statOf returns Node's fields of info, from what fs.FileInfo holds: the
// times are all the modification time.
func statOf(info fs.FileInfo) statData { return genericStat(info) }
