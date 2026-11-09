//go:build !plan9

package webapi

import "syscall"

// The errors of a refused and a reset connection, which netError names.
const (
	errConnRefused = syscall.ECONNREFUSED
	errConnReset   = syscall.ECONNRESET
)
