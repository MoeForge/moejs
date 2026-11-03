package engine

import (
	"unicode/utf8"

	"github.com/MoeForge/moejs/bytecode"
)

// Tagged-template objects (GetTemplateObject). Each GetTemplate site owns one
// inline-cache slot; the realm's template object for the site is created on
// the site's first evaluation and the slot remembers it (loc = 1 + index
// into realmLazy.templates), so repeated evaluations return the same object
// and realms or code without tagged templates pay nothing. Dynamic code
// (dynamic.go), whose slots the realm reclaims and whose sites the program
// may create without bound, keeps the object of a site in its meta's
// constant instead, which dies with the code.

// templateObject returns the template object of the ConstTemplate k for the
// site whose inline-cache slot is e, of the innermost frame's function.
func (r *Realm) templateObject(k *bytecode.Const, e *ICEntry) *Object {
	if e.loc != 0 {
		return r.lazy.templates[e.loc-1]
	}
	var slot *Value
	if st := &r.interp; st.nframes > 0 {
		fd := st.frames[st.nframes-1].fn.internal.(*FunctionData)
		if fd.meta.dyn != 0 {
			slot = &fd.meta.consts[constIndex(fd.code.Consts, k)]
			if slot.IsObject() {
				return slot.AsObject()
			}
		}
	}
	n := len(k.Cooked)
	vals := make([]Value, 2*n)
	cooked, raw := vals[:n:n], vals[n:]
	for i, s := range k.Cooked {
		if s == bytecode.UndefinedCooked {
			cooked[i] = Undefined()
		} else {
			cooked[i] = StringValue(fromWTF8(s))
		}
		raw[i] = StringValue(fromWTF8Text(k.Raw[i]))
	}
	rawObj := r.NewArrayFromSlice(raw)
	rawObj.Freeze(r)
	o := r.NewArrayFromSlice(cooked)
	o.shape = o.shape.addProperty(r, StringKey(AtomRaw), 0)
	o.slots = append(o.slots, ObjectValue(rawObj))
	o.Freeze(r)
	if slot != nil {
		*slot = ObjectValue(o)
		return o
	}
	l := r.lazyState()
	l.templates = append(l.templates, o)
	e.loc = uint32(len(l.templates))
	return o
}

// fromWTF8 converts a compiler string constant (WTF-8: lone surrogates are
// encoded as three-byte sequences) to a String.
func fromWTF8(s string) *String {
	if isASCII(s) {
		return FromGoString(s)
	}
	return FromUTF16(appendWTF8Units(nil, s))
}

// fromWTF8Text converts compiled source text (a regular expression
// literal's pattern, a template's raw strings, a function's source text),
// which is UTF-8 unless it comes from dynamic source with a lone surrogate
// (String.wtf8): valid UTF-8 converts as FromGoString does, and a lone
// surrogate's WTF-8 to the code unit.
func fromWTF8Text(s string) *String {
	if utf8.ValidString(s) {
		return FromGoString(s)
	}
	return FromUTF16(appendWTF8Units(make([]uint16, 0, len(s)), s))
}
