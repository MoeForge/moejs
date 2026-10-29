package node

// SetNoFast turns Linux's openat2 path off for the installations made
// after it, so that the tests go the os.Root way, as every other system
// does.
func SetNoFast(v bool) { noFast = v }

// SetWin32ForTest turns the Windows path forms on for the installations
// made after it, so that they are tested on every system.
func SetWin32ForTest(v bool) { win32ForTest = v }

// WinSysErrorCode returns the code libuv gives the Windows error n.
func WinSysErrorCode(n uint32) string { return winSysErrorCode(n) }

// WinErrnoNumbers returns libuv's codes on Windows, negated.
func WinErrnoNumbers() map[string]int { return winErrnoNumbers }

// BudgetUsed returns what the workers hold of the installation's budget.
func BudgetUsed(a *API) int64 {
	b := a.e.budget
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}
