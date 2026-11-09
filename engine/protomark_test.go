package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPrototypesMarked: objects the engine makes prototypes outside
// SetPrototypeOf and object creation are flagged, so that a change to them
// advances the prototype epoch and no inline cache keeps an entry through
// them. Error is the prototype of the other error constructors and of
// AggregateError; a bound function takes its target's prototype, which a
// proxy's trap can make any object.
func TestPrototypesMarked(t *testing.T) {
	r := NewRealm()
	require.NotZero(t, r.errorCtors[KindError].flags&flagIsPrototype)

	// A cached inherited property deleted and redefined as an accessor.
	assert.Equal(t, "1,1,,2,9", runScripts(t, `function f(o) { return o.foo; }
Error.foo = 1; Error.bar = 2;
var a = [f(TypeError), f(TypeError)];
delete Error.foo; a.push(f(TypeError));
Error.foo = 2; a.push(f(RangeError));
Object.defineProperty(Error, "foo", {get() { return 9; }}); a.push(f(TypeError));
a.join()`))
	assert.Equal(t, "3,3,", runScripts(t, `function f(o) { return o.foo; }
Error.foo = 3; var a = [f(AggregateError), f(AggregateError)];
delete Error.foo; a.push(f(AggregateError)); a.join()`))

	accTable(t, [][2]string{
		{`var P = {foo: 1};
		  var bf = Function.prototype.bind.call(new Proxy(function () {}, {getPrototypeOf() { return P; }}), null);
		  function f(o) { return o.foo; }
		  var a = [f(bf), f(bf)];
		  delete P.foo; a.push(f(bf));
		  Object.setPrototypeOf(P, {foo: 5}); a.push(f(bf));
		  return a;`, `[1,1,null,5]`},
	})
}
