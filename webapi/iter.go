package webapi

import "github.com/MoeForge/moejs/engine"

const (
	iterKeys = iota
	iterValues
	iterEntries
)

// pairSource is the state of an object with pair iterators: Headers and
// URLSearchParams.
type pairSource interface{ iteratorClass() classID }

// pairIterator is the state of a Headers or URLSearchParams iterator.
type pairIterator struct {
	target any
	kind   int
	index  int
}

// iterMethod is a method that returns a pair iterator of class id.
func (e *env) iterMethod(name string, id classID, kind int) member {
	return member{name: name, fn: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
		if !this.IsObject() {
			return undefined, r.TypeError("Method %s called on incompatible receiver %s", name, r.DisplayString(this))
		}
		t, ok := this.AsObject().Internal().(pairSource)
		if !ok || t.iteratorClass() != id {
			return undefined, r.TypeError("Method %s called on incompatible receiver %s", name, r.DisplayString(this))
		}
		o, _ := e.newObject(id, nil, &pairIterator{target: t, kind: kind})
		return engine.ObjectValue(o), nil
	}}
}

// pairIteratorSpec is the prototype of the iterators of name over the
// pairs that pairAt returns.
func (e *env) pairIteratorSpec(name string, pairAt func(t any, i int) (k, v engine.Value, ok bool)) classSpec {
	return classSpec{
		name: name, noCtor: true, proto: e.r.IteratorPrototype,
		members: []member{
			{name: "next", fn: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				it, err := thisAs[*pairIterator](r, this, name+".next")
				if err != nil {
					return undefined, err
				}
				k, v, ok := pairAt(it.target, it.index)
				if !ok {
					return e.iterResult(undefined, true), nil
				}
				it.index++
				switch it.kind {
				case iterKeys:
					return e.iterResult(k, false), nil
				case iterValues:
					return e.iterResult(v, false), nil
				}
				return e.iterResult(engine.ObjectValue(r.NewArrayFromSlice([]engine.Value{k, v})), false), nil
			}},
		},
	}
}
