package engine

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
)

// shrunkDict returns a plain object in dictionary mode that holds base
// keys, after it held peak more and lost them to deletions.
func shrunkDict(t *testing.T, r *Realm, base, peak int) *Object {
	o := r.NewObject()
	set := func(name string) {
		_, err := o.Set(r, r.KeyFromGoString(name), IntValue(1), ObjectValue(o))
		require.NoError(t, err)
	}
	for i := range base {
		set(fmt.Sprintf("base%d", i))
	}
	for i := range peak {
		set(fmt.Sprintf("k%d", i))
	}
	for i := range peak {
		require.True(t, o.Delete(r, r.KeyFromGoString(fmt.Sprintf("k%d", i))))
	}
	require.NotZero(t, o.flags&flagDict)
	return o
}

// dictChurn adds and deletes keys on o, rounds times, and returns the work
// of the compactions the deletions ran: the entries each walked and the
// index slots each rebuilt.
func dictChurn(t *testing.T, r *Realm, o *Object, keys []PropertyKey, rounds int) int {
	work := 0
	for range rounds {
		for _, k := range keys {
			_, err := o.Set(r, k, IntValue(2), ObjectValue(o))
			require.NoError(t, err)
		}
		for _, k := range keys {
			d := o.dictTable()
			n, dead := len(d.entries), d.deadCount()
			require.True(t, o.Delete(r, k))
			if d.deadCount() <= dead {
				work += n + len(d.index.slots)
			}
		}
	}
	return work
}

// TestDictShrinkWork checks that the compactions of an object that once
// held many keys cost what it holds now: additions and deletions of ten
// keys on an object of 70 that held 2^17 more do the work they do on one
// that never held more than 80, where the entries it kept the capacity of
// made each compaction rebuild an index of 2^19 slots.
func TestDictShrinkWork(t *testing.T) {
	r := NewRealm()
	churn := make([]PropertyKey, 10)
	for i := range churn {
		churn[i] = r.KeyFromGoString(fmt.Sprintf("c%d", i))
	}
	small := shrunkDict(t, r, 70, 0)
	big := shrunkDict(t, r, 70, 1<<17)
	want := dictChurn(t, r, small, churn, 2000)
	got := dictChurn(t, r, big, churn, 2000)
	t.Logf("compaction work: %d after 2^17 keys came and went, %d without", got, want)
	require.NotZero(t, want)
	require.LessOrEqual(t, got, 2*want, "compaction work after 2^17 keys came and went")
	d := big.dictTable()
	require.Equal(t, 70, d.namedCount())
	require.LessOrEqual(t, cap(d.entries), 8*80)
	require.LessOrEqual(t, len(d.index.slots), len(small.dictTable().index.slots))
}

// TestDictShrinkMemory checks that a dictionary emptied by deletions lets
// its entries and index go: the compactions that shrink it charge the
// memory limit what they allocate, and the live measurement no longer
// counts what it held.
func TestDictShrinkMemory(t *testing.T) {
	r := NewRealmWith(RealmOptions{MemoryLimit: 1 << 40})
	const n = 200_000
	keys := make([]PropertyKey, n)
	for i := range keys {
		keys[i] = r.KeyFromGoString(fmt.Sprintf("k%d", i))
	}
	o := r.NewObject()
	for _, k := range keys {
		_, err := o.Set(r, k, IntValue(1), ObjectValue(o))
		require.NoError(t, err)
	}
	live := func() int64 {
		n, ok := r.measureLive(func(add func(Value)) { add(ObjectValue(o)) })
		require.True(t, ok)
		return n
	}
	full := live()
	d := o.dictTable()
	peak := d.size()
	shrinks := 0
	for _, k := range keys {
		c, before := cap(d.entries), r.Stats().AllocatedBytes
		require.True(t, o.Delete(r, k))
		charged := r.Stats().AllocatedBytes - before
		if cap(d.entries) == c {
			require.Zero(t, charged, "a deletion that moves no entries allocates nothing")
			continue
		}
		shrinks++
		require.Less(t, cap(d.entries), c)
		require.Equal(t, int64(cap(d.entries))*int64(unsafe.Sizeof(dictEntry{}))+int64(4*len(d.index.slots)), charged,
			"a shrink charges its entries and index")
	}
	require.NotZero(t, shrinks)
	require.Zero(t, d.namedCount())
	require.LessOrEqual(t, cap(d.entries), 2*dictShrinkMin)
	after := live()
	t.Logf("dictionary %d → %d bytes; live %d → %d bytes in %d shrinks", peak, d.size(), full, after, shrinks)
	require.GreaterOrEqual(t, full-after, (peak-d.size())*7/8, "the live measurement sees the smaller dictionary")
}

// TestDictShrinkModel runs random additions, overwrites and deletions on
// an object against a model, through growth to thousands of keys,
// deletions down to a few, which shrink its entries, growth again and a
// second shrink, with churn around each size: its own keys in order, their
// values, absent keys, and the bounds of the dictionary after each
// compaction. The keys mix strings, symbols, array indices, which the
// object keeps apart from its dictionary, and strings that read as
// integers but are no index.
func TestDictShrinkModel(t *testing.T) {
	r := NewRealmWith(RealmOptions{MemoryLimit: 1 << 40})
	var keys []PropertyKey
	for i := range 6000 {
		switch i % 10 {
		case 0:
			keys = append(keys, SymbolKey(NewSymbol(asciiString(fmt.Sprint(i)))))
		case 1:
			keys = append(keys, r.KeyFromGoString(fmt.Sprint(i))) // an index
		case 2:
			keys = append(keys, r.KeyFromGoString(fmt.Sprint(-i)))
		case 3:
			keys = append(keys, r.KeyFromGoString(fmt.Sprintf("0%d", i)))
		default:
			keys = append(keys, r.KeyFromGoString(fmt.Sprintf("k%d", i)))
		}
	}
	keys = append(keys, r.KeyFromGoString("4294967295"), r.KeyFromGoString("4294967294"))
	require.False(t, keys[len(keys)-2].IsIndex(), "2^32-1 is no index")
	require.True(t, keys[len(keys)-1].IsIndex())

	o := r.NewObject()
	vals := map[PropertyKey]int{}
	var present, named []PropertyKey // named: string and symbol keys in insertion order
	want := func() []PropertyKey {
		var idx []uint32
		for _, k := range present {
			if k.IsIndex() {
				idx = append(idx, k.Index())
			}
		}
		slices.Sort(idx)
		out := make([]PropertyKey, 0, len(present))
		for _, i := range idx {
			out = append(out, IndexKey(i))
		}
		for _, k := range named {
			if k.IsString() {
				out = append(out, k)
			}
		}
		for _, k := range named {
			if k.IsSymbol() {
				out = append(out, k)
			}
		}
		return out
	}
	check := func(step int) {
		require.Equal(t, want(), o.OwnPropertyKeys(), "step %d: own keys", step)
		for _, k := range keys[:300] {
			desc, ok := o.GetOwnProperty(k)
			v, has := vals[k]
			require.Equal(t, has, ok, "step %d: %s", step, k.GoString())
			if has {
				require.Equal(t, IntValue(v), desc.Value, "step %d: %s", step, k.GoString())
			}
		}
		if d := o.dictTable(); o.flags&flagDict != 0 && len(d.entries) > dictLinear {
			require.NotNil(t, d.index, "step %d: an index past dictLinear entries", step)
		}
	}

	rng := rand.New(rand.NewPCG(7, 11))
	step := 0
	for _, ph := range []struct{ size, steps int }{{4000, 12000}, {20, 12000}, {2500, 8000}, {10, 8000}, {300, 3000}} {
		shrinks, grew := 0, 0
		for range ph.steps {
			step++
			grow := len(present) < ph.size
			if rng.IntN(4) == 0 {
				grow = !grow
			}
			switch {
			case len(present) != 0 && rng.IntN(10) == 0:
				k := present[rng.IntN(len(present))]
				_, err := o.Set(r, k, IntValue(step), ObjectValue(o))
				require.NoError(t, err)
				vals[k] = step
			case grow || len(present) == 0:
				k := keys[rng.IntN(len(keys))]
				for _, ok := vals[k]; ok; _, ok = vals[k] {
					k = keys[rng.IntN(len(keys))]
				}
				c := 0
				if d := o.dictTable(); d != nil {
					c = cap(d.entries)
				}
				_, err := o.Set(r, k, IntValue(step), ObjectValue(o))
				require.NoError(t, err)
				if d := o.dictTable(); o.flags&flagDict != 0 && cap(d.entries) > c {
					grew++
				}
				vals[k] = step
				present = append(present, k)
				if !k.IsIndex() {
					named = append(named, k)
				}
			default:
				i := rng.IntN(len(present))
				k := present[i]
				d, dead, c := o.dictTable(), 0, 0
				dict := o.flags&flagDict != 0 && !k.IsIndex()
				if dict {
					dead, c = d.deadCount(), cap(d.entries)
				}
				require.True(t, o.Delete(r, k))
				if dict && d.deadCount() <= dead {
					// A compaction: its capacity is at most eight times the
					// live entries, or twice dictShrinkMin.
					l := d.namedCount()
					require.True(t, cap(d.entries) <= 2*dictShrinkMin || cap(d.entries) <= 8*l,
						"step %d: capacity %d for %d entries", step, cap(d.entries), l)
					require.True(t, d.index == nil || len(d.index.slots) == 16 || len(d.index.slots) < 3*cap(d.entries))
					if cap(d.entries) < c {
						shrinks++
					}
				}
				delete(vals, k)
				present[i] = present[len(present)-1]
				present = present[:len(present)-1]
				if j := slices.Index(named, k); j >= 0 {
					named = slices.Delete(named, j, j+1)
				}
			}
			if step%250 == 0 {
				check(step)
			}
		}
		check(step)
		t.Logf("to %d keys: %d left, %d growths, %d shrinks", ph.size, len(present), grew, shrinks)
		switch ph.size {
		case 20, 10:
			require.NotZero(t, shrinks, "deletions to %d keys shrink the entries", ph.size)
		case 2500:
			require.NotZero(t, grew, "additions to %d keys grow them again", ph.size)
		}
	}
}
