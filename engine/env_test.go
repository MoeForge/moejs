package engine

import (
	"runtime"
	"testing"
	"unsafe"
)

// TestEnvBlock checks an environment of each size against its block: the
// slots start undefined, sit right after the header, keep what they are
// given across a collection, and cost one allocation of (n+1) Values.
func TestEnvBlock(t *testing.T) {
	parent := NewEnv(nil, 1)
	for n := range 10 {
		e := NewEnv(parent, n)
		if e.Parent() != parent || e.Len() != n || len(e.Slots()) != n || cap(e.Slots()) != n {
			t.Fatalf("n=%d: parent %p len %d slots %d/%d", n, e.Parent(), e.Len(), len(e.Slots()), cap(e.Slots()))
		}
		if n > 0 && uintptr(unsafe.Pointer(&e.Slots()[0])) != uintptr(unsafe.Pointer(e))+unsafe.Sizeof(Value{}) {
			t.Fatalf("n=%d: slots not after the header", n)
		}
		for i := range n {
			if !e.Slot(i).IsUndefined() {
				t.Fatalf("n=%d: slot %d starts %v", n, i, e.Slot(i))
			}
			e.SetSlot(i, StringValue(FromGoString(string(rune('a'+i))+"-slot")))
		}
		runtime.GC()
		for i := range n {
			if got := e.Slot(i).String(); got != string(rune('a'+i))+"-slot" {
				t.Fatalf("n=%d: slot %d after GC = %q", n, i, got)
			}
		}
		if e.Parent() != parent {
			t.Fatalf("n=%d: parent lost", n)
		}
		if a := testing.AllocsPerRun(10, func() { _ = newEnvSized(parent, n) }); a != 1 {
			t.Fatalf("n=%d: %v allocations", n, a)
		}
	}
}

// TestEnvDepth checks every encoded depth and slot against a chain of
// environments, including depth zero and the last depth the byte holds.
func TestEnvDepth(t *testing.T) {
	var envs [256]*Env
	var parent *Env
	for i := range envs {
		envs[i] = NewEnv(parent, 8)
		parent = envs[i]
	}
	for d := range envs {
		want := envs[len(envs)-1-d]
		if got := parent.up(uint8(d)); got != want {
			t.Fatalf("depth %d: got %p, want %p", d, got, want)
		}
		for s := range 8 {
			w := uint32(s)<<24 | uint32(d)<<16 | 0xffff
			wantSlot := &want.Slots()[s]
			if got := parent.getSlotAt(w); got != wantSlot {
				t.Fatalf("GetEnv depth %d slot %d: got %p, want %p", d, s, got, wantSlot)
			}
			if got := parent.slotAt(w); got != wantSlot {
				t.Fatalf("depth %d slot %d: got %p, want %p", d, s, got, wantSlot)
			}
		}
	}
	if envs[0].up(1) != nil || (*Env)(nil).up(0) != nil {
		t.Fatal("walk past the root did not return nil")
	}
}

// TestEnvNilSlots checks that the slots of a nil environment, which a bad
// Env depth in edited bytecode reaches, panic as a nil dereference the run
// can recover from, and not as a fatal checkptr error under -race.
func TestEnvNilSlots(t *testing.T) {
	defer func() {
		if _, ok := recover().(runtime.Error); !ok {
			t.Fatal("no runtime error panic")
		}
	}()
	var e *Env
	_ = e.Slots()
}

// TestEnvNegative checks that NewEnv refuses a negative slot count, which
// would otherwise give slots past the end of the block.
func TestEnvNegative(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	NewEnv(nil, -1)
}
