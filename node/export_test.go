package node

// SetNoFast turns Linux's openat2 path off for the installations made
// after it, so that the tests go the os.Root way, as every other system
// does.
func SetNoFast(v bool) { noFast = v }

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
