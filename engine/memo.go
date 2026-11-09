package engine

import "unsafe"

// realmMemos holds a realm's memos of results by value: the case
// conversions of short strings (builtin_string.go), the short ASCII
// concatenations (string.go) and the BigInts of at most 128 bits
// (bigint.go). Server-side rendering makes the same few thousand of these
// at every render, each anew; a result a memo holds is returned instead.
//
// A realm with lazy state creates it after memoAfter operations that one
// of the memos takes, counted in memoCount, so that a realm that makes a
// few, a plugin's, pays nothing. The concat memo's table is part of it, so
// that a concatenation, the most frequent of the three, loads no other
// pointer and checks no table; the case and BigInt memos' tables are
// allocated on their first use. The memos hold only results the engine made, never
// a value a script made, so ReleaseCallData leaves them, as it leaves the
// key memo. The memory account is charged for the holder and the tables
// when they are made, and the live measurement counts them with the
// results they hold (memlive.go).
type realmMemos struct {
	concats  concatMemo // in the holder, so that a concatenation loads no table
	cases    *caseMemo
	bigints  *bigintMemo
	concat   memoGate // the concat memo's
	bigint   memoGate // the BigInt memo's
	caseNext uint32   // conversions put in the case memo, which pick its ways

	// The last two results the concat memo returned with their pieces or
	// the addresses of their units, which a concatenation checks first,
	// the count of results kept there, whose parity picks the next one's
	// entry, and the gate that skips the check (concatRecent).
	recent     [2]concatRecent
	recentNext uint32
	recentGate memoGate
}

// memoAfter is the number of operations the memos take that a realm makes
// before it creates them, as the key memo after keyMemoAfter keys.
const memoAfter = 1024

// newMemos counts an operation one of the realm's memos would take and
// creates the memos after memoAfter of them; it returns them, nil before.
// The realm has lazy state and no memos yet.
//
//go:noinline
func (r *Realm) newMemos() *realmMemos {
	lz := r.lazy
	if lz.memoCount < memoAfter {
		lz.memoCount++
		return nil
	}
	lz.memos = new(realmMemos)
	r.charge(int(unsafe.Sizeof(realmMemos{})))
	return lz.memos
}

// memoGate skips a memo that seldom hits. A miss costs more than the
// operation without the memo, which it also makes: the hash, the
// comparisons, putting the result in. So a realm whose results seldom
// repeat, keys or BigInts built from a counter, would pay that on each:
// every memoWindow misses, the gate checks the hits since the last check,
// and when they are fewer than one in memoRate lookups, the next memoSkip
// operations the memo would take do not look in it, then it looks again.
// While skipped, an operation pays a load and a decrement, and the
// lookups of one window in about 65 when the memo samples.
type memoGate struct {
	hits   uint32 // since the last check
	misses uint32 // since the last check, which also pick the way a miss goes in
	skip   uint32 // operations left to make without the memo
}

const (
	memoWindow = 1024
	memoRate   = 8
	memoSkip   = 1 << 16
)

// miss counts a miss, ending the window after memoWindow of them.
func (g *memoGate) miss() {
	if g.misses++; g.misses == memoWindow {
		g.check()
	}
}

// check ends a window of memoWindow misses: the memo is skipped for the
// next memoSkip operations when fewer than one lookup in memoRate hit
// since the last check.
//
//go:noinline
func (g *memoGate) check() {
	if uint64(g.hits)*(memoRate-1) < uint64(g.misses) {
		g.skip = memoSkip
	}
	g.hits, g.misses = 0, 0
}
