//go:build linux

package node

import "syscall"

const sysFstatat = syscall.SYS_NEWFSTATAT
