package webapi

import "errors"

// Plan 9's syscall package names no network errors, and its net package
// returns the kernel's strings: a fetch error there has no code but
// ETIMEDOUT, ENOTFOUND and ECONNRESET for a truncated body. These match
// nothing.
var (
	errConnRefused = errors.New("connection refused")
	errConnReset   = errors.New("connection reset")
)
