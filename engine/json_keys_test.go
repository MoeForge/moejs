package engine

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chainShape returns the shape addProperty gives an object of r.plainRoot
// with names in order: the shape JSON.parse built before it followed the
// tree while parsing.
func chainShape(r *Realm, names ...string) *Shape {
	s := r.plainRoot
	for _, n := range names {
		s = s.addProperty(r, key(r, n), attrDefault)
	}
	return s
}

var jsonKeyRuns atomic.Int32

// TestJSONParseKeyTransitions checks JSON.parse's keys that come by a
// transition of the shape tree (jsonParser.key, Shape.childNamed) in a
// realm with shared intrinsics, whose plain root is in the process-wide tree,
// and in one with mutable intrinsics: the shapes are those of the
// addProperty chain, duplicates keep their first position and take the last
// value whether the first one came by a transition or not, an index key
// still builds the object key by key and more than maxShapeProps keys a
// dictionary, and escaped and non-ASCII keys still take their own path.
func TestJSONParseKeyTransitions(t *testing.T) {
	run := strconv.Itoa(int(jsonKeyRuns.Add(1)))
	for _, rc := range []struct {
		name string
		r    *Realm
	}{{"shared", newShared()}, {"mutable", NewRealm()}} {
		t.Run(rc.name, func(t *testing.T) {
			r := rc.r
			parse := func(text string) *Object {
				t.Helper()
				return jsonParseGo(t, r, text).AsObject()
			}
			get := func(o *Object, name string) Value {
				t.Helper()
				v, err := o.GetProp(r, key(r, name))
				require.NoError(t, err)
				return v
			}
			// A name of its own per run, so the first parse makes the
			// transitions and the second follows them.
			fresh := "jk_" + rc.name + "_" + run
			for i := range 2 {
				o := parse(`{"` + fresh + `":1,"model":"m","prompt":"p","n":2}`)
				assert.Same(t, chainShape(r, fresh, "model", "prompt", "n"), o.Shape(), "parse %d", i)
				assert.Equal(t, IntValue(2), get(o, "n"))
			}
			for _, c := range []struct {
				text  string
				names []string
				vals  []Value
			}{
				// The duplicate comes after keys that came by a transition, of
				// one that did, and of one that did not.
				{`{"model":1,"prompt":2,"model":3}`, []string{"model", "prompt"}, []Value{IntValue(3), IntValue(2)}},
				{`{"model":1,"model":2,"prompt":3}`, []string{"model", "prompt"}, []Value{IntValue(2), IntValue(3)}},
				{`{"model":1,"` + fresh + `x":2,"model":3,"` + fresh + `x":4,"prompt":5}`, []string{"model", fresh + "x", "prompt"}, []Value{IntValue(3), IntValue(4), IntValue(5)}},
				// Escaped names: by the intern path, then by the transition
				// they name, and as duplicates of plain ones.
				{`{"\u006dodel":1,"prompt":2}`, []string{"model", "prompt"}, []Value{IntValue(1), IntValue(2)}},
				{`{"model":1,"pro\u006dpt":2}`, []string{"model", "prompt"}, []Value{IntValue(1), IntValue(2)}},
				{`{"model":1,"\u006dodel":2}`, []string{"model"}, []Value{IntValue(2)}},
				{`{"model":1,"\u006dodel":2,"prompt":3}`, []string{"model", "prompt"}, []Value{IntValue(2), IntValue(3)}},
				// A name of its own after keys that came by a transition.
				{`{"model":1,"` + fresh + `y":2}`, []string{"model", fresh + "y"}, []Value{IntValue(1), IntValue(2)}},
				// Non-ASCII names.
				{`{"é":1,"model":2}`, []string{"é", "model"}, []Value{IntValue(1), IntValue(2)}},
				{`{"model":1,"é":2,"é":3}`, []string{"model", "é"}, []Value{IntValue(1), IntValue(3)}},
				// The empty name.
				{`{"":1,"model":2,"":3}`, []string{"", "model"}, []Value{IntValue(3), IntValue(2)}},
			} {
				for range 2 {
					o := parse(c.text)
					assert.Equal(t, c.names, keyNames(o.OwnEnumerableStringKeys()), c.text)
					assert.Same(t, chainShape(r, c.names...), o.Shape(), c.text)
					for i, n := range c.names {
						assert.Equal(t, c.vals[i], get(o, n), "%s: %s", c.text, n)
					}
				}
			}
			// An index key after keys that came by a transition: the object
			// is built key by key (the index key an element, the names in
			// their shape), and index keys come first.
			for range 2 {
				o := parse(`{"model":1,"prompt":2,"7":3,"n":4,"7":5}`)
				assert.Equal(t, []string{"7", "model", "prompt", "n"}, keyNames(o.OwnEnumerableStringKeys()))
				assert.Same(t, chainShape(r, "model", "prompt", "n"), o.Shape())
				assert.Equal(t, IntValue(5), get(o, "7"))
				assert.Equal(t, IntValue(4), get(o, "n"))
			}

			// maxShapeProps keys whose shape exists, then a duplicate of one
			// of them and one more key: a dictionary with the first
			// positions and the last values.
			var sb strings.Builder
			names := make([]string, maxShapeProps)
			sb.WriteString("{")
			for i := range names {
				names[i] = fresh + "_w" + strconv.Itoa(i)
				if i > 0 {
					sb.WriteString(",")
				}
				fmt.Fprintf(&sb, `"%s":%d`, names[i], i)
			}
			wide := sb.String()
			for range 2 {
				o := parse(wide + "}")
				assert.False(t, o.IsDictionaryMode())
				assert.Same(t, chainShape(r, names...), o.Shape())
			}
			for range 2 {
				o := parse(wide + `,"` + names[5] + `":"dup","` + fresh + `_last":1}`)
				assert.True(t, o.IsDictionaryMode())
				got := keyNames(o.OwnEnumerableStringKeys())
				require.Len(t, got, maxShapeProps+1)
				assert.Equal(t, names, got[:maxShapeProps])
				assert.Equal(t, "dup", get(o, names[5]).AsString().GoString())
				assert.Equal(t, IntValue(6), get(o, names[6]))
			}
			// A duplicate as the key past maxShapeProps.
			o := parse(wide + `,"` + names[63] + `":"dup"}`)
			assert.False(t, o.IsDictionaryMode())
			assert.Same(t, chainShape(r, names...), o.Shape())
			assert.Equal(t, "dup", get(o, names[63]).AsString().GoString())
		})
	}
}

// TestJSONParseKeyAtoms checks that a key JSON.parse finds by a transition
// is the atom of the transition, and that a key it interns does not share
// the content of the text.
func TestJSONParseKeyAtoms(t *testing.T) {
	r := newShared()
	run := strconv.Itoa(int(jsonKeyRuns.Add(1)))
	text := FromGoString(`{"jk_atom_` + run + `":{"jk_atom_inner_` + run + `":1}}`)
	for range 2 {
		v, err := r.JSONParse(text)
		require.NoError(t, err)
		o := v.AsObject()
		k := o.Shape().Props()[0].key
		require.True(t, k.IsString())
		assert.NotZero(t, k.String().atom)
		assert.Same(t, r.InternGoString("jk_atom_"+run), k.String())
		assert.False(t, contentIn(k.String(), text), "the atom does not alias the text")
		inner, _ := o.GetProp(r, k)
		ik := inner.AsObject().Shape().Props()[0].key
		assert.False(t, contentIn(ik.String(), text), "the atom does not alias the text")
	}
}

// TestShapeNamedChildren checks Shape.childNamed on a shared shape: while
// its snapshot keeps the children in a slice and through the names index,
// built with the map, past it, as publishes add to the index or replace
// it, for ASCII names of attrDefault children only.
func TestShapeNamedChildren(t *testing.T) {
	r := newShared()
	run := strconv.Itoa(int(jsonKeyRuns.Add(1)))
	root := &Shape{proto: r.ObjectPrototype, key: rootKey, shared: true}
	children := map[string]*Shape{}
	add := func(name string) {
		c := root.publish(key(r, name), attrDefault)
		require.NotNil(t, c, name)
		children[name] = c
	}
	check := func() {
		t.Helper()
		for name, c := range children {
			if isASCII(name) {
				assert.Same(t, c, root.childNamed(name), name)
			} else {
				assert.Nil(t, root.childNamed(name), "non-ASCII names are not indexed: %s", name)
			}
		}
		assert.Nil(t, root.childNamed("nk_absent_"+run))
		assert.Nil(t, root.childNamed("nk_hidden_"+run), "a child with other attributes")
	}
	require.NotNil(t, root.publish(key(r, "nk_hidden_"+run), attrHidden))
	add("nk_0_" + run)
	check()
	for i := 1; i < sharedSmallTransitions+4; i++ {
		add("nk_" + strconv.Itoa(i) + "_" + run)
	}
	add("nké_" + run)
	add("") // the empty name
	require.NotNil(t, root.sharedTrans.Load().m, "past the slice")
	tab := root.sharedTrans.Load().names
	require.NotNil(t, tab, "the index is built with the map")
	check()
	add("nk_late_" + run)
	assert.Same(t, tab, root.sharedTrans.Load().names, "a publish adds to the index in place")
	check()
	for i := range maxSharedDataTransitions - len(children) - 1 {
		add("nk_more_" + strconv.Itoa(i) + "_" + run)
	}
	check()
	// Code-bound names pass the data bound: past half full the table is
	// replaced by a larger one, which keeps every child.
	var code []PropertyKey
	for i := range 200 {
		code = append(code, key(r, "nk_code_"+strconv.Itoa(i)+"_"+run))
	}
	addCodeKeys(code)
	sizes := map[int]bool{len(tab.slots): true}
	for i := range 200 {
		add("nk_code_" + strconv.Itoa(i) + "_" + run)
		names := root.sharedTrans.Load().names
		assert.LessOrEqual(t, 2*names.n, len(names.slots), "at most half full")
		sizes[len(names.slots)] = true
	}
	total, last := 0, len(root.sharedTrans.Load().names.slots)
	for n := range sizes {
		total += n
	}
	assert.Greater(t, len(sizes), 2, "the table grows")
	assert.LessOrEqual(t, total, 2*last, "its tables add up to at most twice the last")
	check()
}

// TestJSONParseSharedRootConcurrent parses objects whose first keys are new
// to the shared root in realms on several goroutines, while the others
// look up the keys already published through the root's names index: each
// publish replaces the snapshot the index lives in (run with -race).
func TestJSONParseSharedRootConcurrent(t *testing.T) {
	run := strconv.Itoa(int(jsonKeyRuns.Add(1)))
	const workers, rounds = 6, 40
	var probe []PropertyKey
	r0 := newShared()
	for w := range workers {
		for i := range rounds {
			probe = append(probe, key(r0, fmt.Sprintf("jc_%s_%d_%d", run, w, i)))
		}
	}
	addCodeKeys(probe) // code-bound names are published at the root past its data bound
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			r := newShared()
			for i := range rounds {
				own := fmt.Sprintf("jc_%s_%d_%d", run, w, i)
				other := fmt.Sprintf("jc_%s_%d_%d", run, (w+1)%workers, i/2)
				for _, text := range []string{`{"` + own + `":1,"model":2}`, `{"` + other + `":3,"model":4}`, `{"model":5}`} {
					v, err := r.JSONParse(FromGoString(text))
					if err != nil {
						t.Error(err)
						return
					}
					o := v.AsObject()
					if got := o.Shape().Count(); got != len(strings.Split(text, ",")) {
						t.Errorf("%s: %d properties", text, got)
					}
				}
			}
		})
	}
	wg.Wait()
	r := newShared()
	for w := range workers {
		for i := range rounds {
			name := fmt.Sprintf("jc_%s_%d_%d", run, w, i)
			o := jsonParseGo(t, r, `{"`+name+`":1}`).AsObject()
			assert.Same(t, chainShape(r, name), o.Shape(), name)
			assert.True(t, o.Shape().IsShared(), name)
		}
	}
}

// TestJSONParseKeyKinds checks keys the fast path must treat as names, or
// leave to the escape and non-ASCII path, in a realm with shared intrinsics
// and one with mutable ones, each parsed twice (the second follows the
// transitions the first made): "__proto__" is an own property, not the
// prototype; "4294967295", "01" and "-0" are names, not indexes, and come
// after the indexes; escaped quotes and lone surrogates, escaped or raw,
// are keys of their own; and an escaped key that names an existing
// transition, then its raw duplicate, is one key with the last value.
func TestJSONParseKeyKinds(t *testing.T) {
	for _, rc := range []struct {
		name string
		r    *Realm
	}{{"shared", newShared()}, {"mutable", NewRealm()}} {
		t.Run(rc.name, func(t *testing.T) {
			r := rc.r
			get := func(o *Object, k PropertyKey) Value {
				t.Helper()
				v, err := o.GetProp(r, k)
				require.NoError(t, err)
				return v
			}
			for _, c := range []struct {
				text  string
				names []string // the named keys, in order
				vals  []Value
			}{
				{`{"__proto__":1,"model":2}`, []string{"__proto__", "model"}, []Value{IntValue(1), IntValue(2)}},
				{`{"model":1,"__proto__":2,"__proto__":3}`, []string{"model", "__proto__"}, []Value{IntValue(1), IntValue(3)}},
				{`{"4294967295":1,"model":2}`, []string{"4294967295", "model"}, []Value{IntValue(1), IntValue(2)}},
				{`{"model":1,"01":2,"-0":3,"4294967295":4}`, []string{"model", "01", "-0", "4294967295"}, []Value{IntValue(1), IntValue(2), IntValue(3), IntValue(4)}},
				{`{"a\"b":1,"model":2,"a\"b":3}`, []string{`a"b`, "model"}, []Value{IntValue(3), IntValue(2)}},
				{`{"model":1,"model":2}`, []string{"model"}, []Value{IntValue(2)}},
				{`{"prompt":0,"model":1,"model":2}`, []string{"prompt", "model"}, []Value{IntValue(0), IntValue(2)}},
			} {
				for range 2 {
					o := jsonParseGo(t, r, c.text).AsObject()
					assert.Equal(t, c.names, keyNames(o.OwnEnumerableStringKeys()), c.text)
					assert.Same(t, chainShape(r, c.names...), o.Shape(), c.text)
					assert.Same(t, r.ObjectPrototype, o.Proto(), c.text)
					for i, n := range c.names {
						assert.Equal(t, c.vals[i], get(o, key(r, n)), "%s: %s", c.text, n)
					}
				}
			}
			// Indexes first, among them 4294967294 (the largest); the names
			// that look like numbers after them, in their order.
			for range 2 {
				o := jsonParseGo(t, r, `{"b":1,"4294967295":2,"01":3,"-0":4,"0":5,"4294967294":6}`).AsObject()
				assert.Equal(t, []string{"0", "4294967294", "b", "4294967295", "01", "-0"}, keyNames(o.OwnEnumerableStringKeys()))
				assert.Same(t, chainShape(r, "b", "4294967295", "01", "-0"), o.Shape())
				assert.Equal(t, IntValue(6), get(o, key(r, "4294967294")))
			}
			// A lone surrogate, escaped and raw: the same key either way.
			lone := r.KeyFromString(FromUTF16([]uint16{0xD800}))
			raw := FromUTF16(append(append(FromGoString(`{"`).UTF16(), 0xD800), FromGoString(`":1,"model":2}`).UTF16()...))
			for _, text := range []*String{FromGoString(`{"\ud800":1,"model":2}`), raw, FromGoString(`{"\ud800":1,"model":2}`), raw} {
				v, err := r.JSONParse(text)
				require.NoError(t, err)
				o := v.AsObject()
				props := o.Shape().Props()
				require.Len(t, props, 2)
				assert.Equal(t, lone, props[0].key)
				assert.Equal(t, key(r, "model"), props[1].key)
				assert.Equal(t, IntValue(1), get(o, lone))
			}
		})
	}
}

// TestJSONParseKeyOtherAttributes checks that a shared child with a parsed
// key's name and other attributes than a data property's (defineProperty
// made it) is not the parsed key's transition, at the root and below it:
// Shape.childNamed indexes attrDefault children only.
func TestJSONParseKeyOtherAttributes(t *testing.T) {
	r := newShared()
	run := strconv.Itoa(int(jsonKeyRuns.Add(1)))
	top, prefix, below := "ja_top_"+run, "ja_prefix_"+run, "ja_below_"+run
	addCodeKeys([]PropertyKey{key(r, top), key(r, prefix), key(r, below)}) // past the shared data bound
	hidden := r.plainRoot.addProperty(r, key(r, top), attrHidden)
	hiddenBelow := chainShape(r, prefix).addProperty(r, key(r, below), attrHidden)
	require.True(t, hidden.IsShared() && hiddenBelow.IsShared())
	for range 2 {
		o := jsonParseGo(t, r, `{"`+top+`":1}`).AsObject()
		assert.NotSame(t, hidden, o.Shape())
		assert.Same(t, chainShape(r, top), o.Shape())
		assert.Equal(t, []string{top}, keyNames(o.OwnEnumerableStringKeys()), "enumerable")
		o = jsonParseGo(t, r, `{"`+prefix+`":1,"`+below+`":2}`).AsObject()
		assert.NotSame(t, hiddenBelow, o.Shape())
		assert.Same(t, chainShape(r, prefix, below), o.Shape())
		assert.Equal(t, []string{prefix, below}, keyNames(o.OwnEnumerableStringKeys()))
		d, ok := o.GetOwnProperty(key(r, below))
		require.True(t, ok)
		assert.Equal(t, attrDefault, d.attrs, "writable, enumerable and configurable")
	}
}

// TestJSONParseDataBoundAfterPrefix checks the keys after a prefix that
// came by the fast path when the prefix's shared shape has reached its
// bound of data transitions (maxSharedDataTransitions): the object
// continues in the realm's own tree (Realm.localize) with the same keys,
// values and order as addProperty gives, and the shapes before the bound
// stay shared.
func TestJSONParseDataBoundAfterPrefix(t *testing.T) {
	r := newShared()
	run := strconv.Itoa(int(jsonKeyRuns.Add(1)))
	prefix := "jb_prefix_" + run
	addCodeKeys([]PropertyKey{key(r, prefix)}) // the root keeps publishing it
	data := func(i int) string { return "jb_" + run + "_" + strconv.Itoa(i) }
	for i := range maxSharedDataTransitions {
		o := jsonParseGo(t, r, `{"`+prefix+`":0,"`+data(i)+`":1}`).AsObject()
		require.True(t, o.Shape().IsShared(), "data transition %d is published", i)
	}
	for range 2 {
		o := jsonParseGo(t, r, `{"`+prefix+`":0,"`+data(maxSharedDataTransitions)+`":1,"`+data(100)+`":2}`).AsObject()
		assert.False(t, o.Shape().IsShared(), "past the bound")
		assert.Same(t, chainShape(r, prefix, data(maxSharedDataTransitions), data(100)), o.Shape())
		assert.Equal(t, []string{prefix, data(maxSharedDataTransitions), data(100)}, keyNames(o.OwnEnumerableStringKeys()))
		v, err := o.GetProp(r, key(r, data(100)))
		require.NoError(t, err)
		assert.Equal(t, IntValue(2), v)
		// The prefix and a key before the bound still come by the shared tree.
		o = jsonParseGo(t, r, `{"`+prefix+`":0,"`+data(3)+`":1}`).AsObject()
		assert.True(t, o.Shape().IsShared())
		assert.Same(t, chainShape(r, prefix, data(3)), o.Shape())
	}
}

// TestJSONParseSharedChildConcurrent is TestJSONParseSharedRootConcurrent
// below the root: objects whose first key is a shared prefix and whose
// second is new to the prefix's shape, parsed in realms on several
// goroutines while the others find the keys already published there
// (run with -race).
func TestJSONParseSharedChildConcurrent(t *testing.T) {
	run := strconv.Itoa(int(jsonKeyRuns.Add(1)))
	const workers, rounds = 6, 40
	r0 := newShared()
	prefix := "jp_prefix_" + run
	probe := []PropertyKey{key(r0, prefix)}
	name := func(w, i int) string { return fmt.Sprintf("jp_%s_%d_%d", run, w, i) }
	for w := range workers {
		for i := range rounds {
			probe = append(probe, key(r0, name(w, i)))
		}
	}
	addCodeKeys(probe) // code-bound names pass the data bound
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			r := newShared()
			for i := range rounds {
				for _, text := range []string{
					`{"` + prefix + `":1,"` + name(w, i) + `":2}`,
					`{"` + prefix + `":3,"` + name((w+1)%workers, i/2) + `":4,"model":5}`,
					`{"` + prefix + `":6}`,
				} {
					v, err := r.JSONParse(FromGoString(text))
					if err != nil {
						t.Error(err)
						return
					}
					if got := v.AsObject().Shape().Count(); got != len(strings.Split(text, ",")) {
						t.Errorf("%s: %d properties", text, got)
					}
				}
			}
		})
	}
	wg.Wait()
	r := newShared()
	for w := range workers {
		for i := range rounds {
			o := jsonParseGo(t, r, `{"`+prefix+`":1,"`+name(w, i)+`":2}`).AsObject()
			assert.Same(t, chainShape(r, prefix, name(w, i)), o.Shape(), name(w, i))
			assert.True(t, o.Shape().IsShared(), name(w, i))
		}
	}
}

// TestSharedTransitionLookup: past the slice, a shared shape's publish and
// child find an ASCII attrDefault child through the names index and every
// other child (other attributes, a non-ASCII name) through the map, and
// the snapshot stays 64 bytes.
func TestSharedTransitionLookup(t *testing.T) {
	r := newShared()
	run := strconv.Itoa(int(jsonKeyRuns.Add(1)))
	root := &Shape{proto: r.ObjectPrototype, key: rootKey, shared: true}
	type entry struct {
		key   PropertyKey
		attrs uint8
	}
	children := map[entry]*Shape{}
	add := func(k PropertyKey, attrs uint8) {
		c := root.publish(k, attrs)
		require.NotNil(t, c)
		children[entry{k, attrs}] = c
	}
	for i := range sharedSmallTransitions + 6 {
		add(key(r, "tl_"+strconv.Itoa(i)+"_"+run), attrDefault)
	}
	add(key(r, "tl_0_"+run), attrHidden)
	add(key(r, "tlé_"+run), attrDefault)
	add(key(r, ""), attrDefault)
	add(key(r, "tl_2_"+run), 0)
	snap := root.sharedTrans.Load()
	require.NotNil(t, snap.m, "past the slice")
	require.NotNil(t, snap.names)
	for e, c := range children {
		assert.Same(t, c, snap.lookup(e.key, e.attrs))
		assert.Same(t, c, root.child(e.key, e.attrs))
		assert.Same(t, c, root.publish(e.key, e.attrs), "publish finds the child")
	}
	assert.Nil(t, snap.lookup(key(r, "tl_absent_"+run), attrDefault))
	assert.Nil(t, snap.lookup(key(r, "tl_1_"+run), attrHidden))
	assert.Nil(t, root.child(key(r, "tl_absent_"+run), attrDefault))
	assert.Nil(t, root.child(key(r, "tl_1_"+run), attrHidden))
	if is64 { // a 64-bit layout
		assert.Equal(t, uintptr(64), unsafe.Sizeof(sharedTransitions{}))
	}
}
