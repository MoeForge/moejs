package engine

import (
	"testing"
)

// TestResumedFrameDropsSavedRegisters: a frame that resumed from a real
// suspension runs on the register stack, so the registers its suspension
// saved must not keep a value it drops after the resumption. The 96 MB
// array the frame sets to null must not stay reachable from them, or the
// measurement counts it next to the 16 MB arrays the frame then allocates
// and the 100 MB limit stops the script: an async call that continues in
// place past further awaits, a generator resumed past a yield and an async
// generator resumed past an await.
func TestResumedFrameDropsSavedRegisters(t *testing.T) {
	for name, src := range map[string]string{
		"async": `async function f(){
			let big = new Array(6e6).fill(1);
			let res; const p = new Promise(r => res = r); queueMicrotask(() => res());
			await p;
			big = null;
			let n = 0;
			for (let i = 0; i < 6; i++) { await 0; let t = new Array(1e6).fill(2); n += t.length; t = null; }
			return n;
		}
		f(); "x"`,
		"generator": `function* g(){
			let big = new Array(6e6).fill(1);
			yield;
			big = null;
			let n = 0;
			for (let i = 0; i < 6; i++) { let t = new Array(1e6).fill(2); n += t.length; t = null; }
			return n;
		}
		const it = g(); it.next(); it.next(); "x"`,
		"async generator": `async function* g(){
			let big = new Array(6e6).fill(1);
			let res; const p = new Promise(r => res = r); queueMicrotask(() => res());
			await p;
			big = null;
			let n = 0;
			for (let i = 0; i < 6; i++) { let t = new Array(1e6).fill(2); n += t.length; t = null; }
			yield n;
		}
		g().next(); "x"`,
	} {
		t.Run(name, func(t *testing.T) {
			r := NewRealmWith(RealmOptions{MemoryLimit: 100 << 20})
			if _, err := runScriptValue(t, r, src); err != nil {
				t.Fatal(err)
			}
		})
	}
}
