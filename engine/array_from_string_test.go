package engine

import (
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestArrayFromStringRepresentations(t *testing.T) {
	r := NewRealm()
	prefix := strings.Repeat("a", 64)
	prefixUnits, prefixPoints := make([]uint16, 64), make([][]uint16, 64)
	for i := range prefixUnits {
		prefixUnits[i], prefixPoints[i] = 'a', []uint16{'a'}
	}
	for _, tc := range []struct {
		name string
		s    *String
		want [][]uint16
	}{
		{"empty", emptyString, nil},
		{"single", FromGoString("a"), [][]uint16{{'a'}}},
		{"ASCII", FromGoString("hello"), [][]uint16{{'h'}, {'e'}, {'l'}, {'l'}, {'o'}}},
		{"UTF16", FromGoString("a\u4e2d\U0001f600b"), [][]uint16{{'a'}, {0x4e2d}, {0xd83d, 0xde00}, {'b'}}},
		{"lazy", FromHostString("a\u4e2d\U0001f600b"), [][]uint16{{'a'}, {0x4e2d}, {0xd83d, 0xde00}, {'b'}}},
		{"unpaired", FromUTF16([]uint16{0xd800, 'a', 0xdc00, 0xd800, 0xdc00, 0xd800}), [][]uint16{{0xd800}, {'a'}, {0xdc00}, {0xd800, 0xdc00}, {0xd800}}},
		{"ASCII rope", concat(FromGoString(prefix), FromGoString("b")), append(prefixPoints[:64:64], []uint16{'b'})},
		{"unpaired rope", concat(FromGoString(prefix), FromUTF16([]uint16{0xd83d})), append(prefixPoints[:64:64], []uint16{0xd83d})},
		{"pair across leaves", concat(FromUTF16(append(prefixUnits[:64:64], 0xd83d)), FromUTF16([]uint16{0xde00, 'b'})), append(prefixPoints[:64:64], []uint16{0xd83d, 0xde00}, []uint16{'b'})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := arrayFrom(r, ObjectValue(r.ArrayCtor), []Value{StringValue(tc.s)})
			require.NoError(t, err)
			// Compare UTF-16 units so lone surrogates cannot be hidden by
			// Go's replacement-character conversion.
			a := v.AsObject()
			require.Equal(t, uint32(len(tc.want)), a.ArrayLength())
			for i, expected := range tc.want {
				got, err := getIndex(r, a, int64(i))
				require.NoError(t, err)
				require.Equal(t, expected, got.AsString().UTF16())
			}
		})
	}
}

func TestArrayFromStringInterrupted(t *testing.T) {
	for _, text := range []string{"abc", strings.Repeat("a", 8192), strings.Repeat("\U0001f600", 8192)} {
		r := NewRealm()
		cause := errors.New("canceled")
		r.Interrupt(cause)
		// Call the builtin directly so it, rather than the public call
		// entry, must observe the pending interrupt.
		_, err := arrayFrom(r, ObjectValue(r.ArrayCtor), []Value{StringValue(FromGoString(text))})
		require.ErrorIs(t, err, cause)
		r.ClearInterrupt()
		v, err := arrayFrom(r, ObjectValue(r.ArrayCtor), []Value{StringValue(FromGoString("ok"))})
		require.NoError(t, err)
		require.Equal(t, uint32(2), v.AsObject().ArrayLength())
	}
}

func TestArrayFromStringObservableProtocol(t *testing.T) {
	runMutableProtoCases(t, []protoCase{
		{"custom iterator", `String.prototype[Symbol.iterator] = function*() { yield "custom"; }; return Array.from("abc").join()`, "custom"},
		{"iterator getter", `let reads=0; const original=String.prototype[Symbol.iterator]; Object.defineProperty(String.prototype, Symbol.iterator, {get(){reads++; return original;}}); return Array.from("ab").join()+"|"+reads`, "a,b|1"},
		{"custom next", `Object.getPrototypeOf(""[Symbol.iterator]()).next = function(){return {done:true}}; return Array.from("abc").length`, "0"},
		{"constructor changes iterator", `function C(){String.prototype[Symbol.iterator]=function*(){yield "changed"}}; return Array.from.call(C,"ab")[1]`, "b"},
		{"custom constructor", `let argc=-1; function C(){argc=arguments.length}; const a=Array.from.call(C,"a\u{1f600}"); return [a instanceof C,argc,a.length,a[1]].join()`, "true,0,2,😀"},
		{"mapper and receiver", `return Array.from("a\u{1f600}",function(c,i){return this.prefix+i+":"+c.length},{prefix:"p"}).join()`, "p0:1,p1:2"},
		{"mapper closes", `let closed=0; Object.getPrototypeOf(""[Symbol.iterator]()).return=function(){closed++;return {}}; try{Array.from("ab",()=>{throw new Error("mapped")})}catch(e){return e.message+":"+closed}`, "mapped:1"},
		{"nonconstructor receiver", `return Array.from.call(null,"a\u{1f600}").length`, "2"},
		{"boxed string", `return Array.from(new String("a\u{1f600}")).length`, "2"},
	})
}

// Allocation counts alone cannot catch a full-size backing array: it is
// one allocation regardless of the input length.
func TestArrayFromStringInterruptedAllocation(t *testing.T) {
	r := NewRealm() // Default configuration, with no memory limit.
	s := FromGoString(strings.Repeat("a", 64<<10))
	args := []Value{StringValue(s)}
	cause := errors.New("canceled")
	r.Interrupt(cause)
	_, err := arrayFrom(r, ObjectValue(r.ArrayCtor), args)
	require.ErrorIs(t, err, cause)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	const runs = 16
	for range runs {
		_, err = arrayFrom(r, ObjectValue(r.ArrayCtor), args)
		require.ErrorIs(t, err, cause)
	}
	runtime.ReadMemStats(&after)
	// Allow ample room for errors and incidental allocations, but not
	// the 1 MiB backing store that this input would require.
	require.Less(t, (after.TotalAlloc-before.TotalAlloc)/runs, uint64(64<<10))
}

func TestArrayFromStringSingleCharacterReuse(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    *String
		want []uint16
	}{
		{"BMP", FromGoString("中"), []uint16{0x4e2d}},
		{"lazy BMP", FromHostString("中"), []uint16{0x4e2d}},
		{"high surrogate", FromUTF16([]uint16{0xd800}), []uint16{0xd800}},
		{"low surrogate", FromUTF16([]uint16{0xdc00}), []uint16{0xdc00}},
		{"pair", FromGoString("😀"), []uint16{0xd83d, 0xde00}},
		{"lazy pair", FromHostString("😀"), []uint16{0xd83d, 0xde00}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRealm()
			v, err := arrayFrom(r, ObjectValue(r.ArrayCtor), []Value{StringValue(tc.s)})
			require.NoError(t, err)
			require.Equal(t, uint32(1), v.AsObject().ArrayLength())
			got, err := getIndex(r, v.AsObject(), 0)
			require.NoError(t, err)
			require.Same(t, tc.s, got.AsString())
			require.Equal(t, tc.want, got.AsString().UTF16())
		})
	}
}
