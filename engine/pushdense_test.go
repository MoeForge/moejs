package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestPushDense covers Object.Push's one append of all its values, to an
// array whose elements all fit its dense storage, through push and the
// ArrayPush op of array literals, and the arrays that still take addIndex
// value by value: storage short of the length and sparse elements.
func TestPushDense(t *testing.T) {
	runScriptCases(t, []scriptCase{
		{"many values", []string{`var a = [1], n = 0; for (var i = 0; i < 300; i++) n = a.push(i, i + 1, i + 2);
			[n, a.length, a[0], a[900], a.slice(-3)].join(":")`}, "901:901:1:301:299,300,301"},
		{"no values", []string{`var a = [1, 2]; a.push() + ":" + a.join()`}, "2:1,2"},
		{"storage short of the length", []string{`var a = [1]; a.length = 4; [a.push(5), a.length, 2 in a, a[4]].join(":")`}, "5:5:false:5"},
		{"sparse elements", []string{`var a = [1]; a[100000] = 2; [a.push(3, 4), a[100001], a[100002], a.length].join(":")`}, "100003:3:4:100003"},
		{"holes stay holes", []string{`var a = [1, , 3]; a.push(4, 5); [1 in a, a.length, a[3], a[4]].join(":")`}, "false:5:4:5"},
		{"literals", []string{`function f(x) { return [x, x + 1, [x], , ...[x, x]]; } var a = f(1); JSON.stringify(a) + ":" + (3 in a)`}, "[1,2,[1],null,1,1]:false"},
		{"pushed to itself", []string{`var a = [1, 2]; a.push.apply(a, a); a.push(...a); a.join()`}, "1,2,1,2,1,2,1,2"},
		{"non-extensible", []string{`var a = Object.preventExtensions([1]), r; try { a.push(2); r = "pushed"; } catch (e) { r = e.name + ":" + a.length; } r`}, "TypeError:1"},
		{"an index setter on the prototype", []string{`var seen = []; Object.defineProperty(Array.prototype, 1, {set(v) { seen.push(v); }, configurable: true});
			var a = [0]; a.push(7, 8); var r = [a.length, seen.join(), a.hasOwnProperty(1), a[2]].join(":"); delete Array.prototype[1]; r`}, "3:7:false:8"},
	})
}

// TestEqualsInterned checks that two interned strings are equal only when
// they are one String, and that an interned string and one that is not
// compare by content.
func TestEqualsInterned(t *testing.T) {
	r := NewRealm()
	a, b := r.InternGoString("equals-interned-a"), r.InternGoString("equals-interned-b")
	assert.False(t, a.Equals(b))
	assert.True(t, a.Equals(r.InternGoString("equals-interned-a")))
	assert.True(t, a.Equals(FromGoString("equals-interned-a")))
	assert.True(t, FromGoString("equals-interned-a").Equals(a))
	assert.False(t, FromGoString("equals-interned-b").Equals(a))
	u := r.Intern(FromGoString("équals-interned"))
	assert.True(t, u.Equals(FromGoString("équals-interned")))
	assert.False(t, u.Equals(r.Intern(FromGoString("équals-internee"))))
	assert.Equal(t, "true:false:true", runScripts(t, `var k = "equals-interned-c", o = {[k]: 1};
		[Object.keys(o)[0] === "equals-interned-c", Object.keys(o)[0] === "equals-interned-d", Object.keys(o)[0] === "equals-" + "interned-c"].join(":")`))
}
