package moejs_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInterruptUnwrap checks that the cause a host interrupts with is
// reachable through the *InterruptedError: errors.Is finds
// context.DeadlineExceeded for a deadline, and errors.As the host's own
// error type.
func TestInterruptUnwrap(t *testing.T) {
	mod, err := moejs.Compile("loop.js", "export function loop() { for (;;) {} }")
	require.NoError(t, err)
	hook := mustHook(t, mod, "loop")
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(mod))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { rt.Interrupt(context.Cause(ctx)) })
	defer stop()
	_, err = rt.Call(hook)
	var interrupted *moejs.InterruptedError
	require.ErrorAs(t, err, &interrupted)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, context.DeadlineExceeded, interrupted.Unwrap())
	rt.ClearInterrupt()

	type hostCause struct{ error }
	rt.Interrupt(hostCause{errors.New("quota")})
	_, err = rt.Call(hook)
	var cause hostCause
	require.ErrorAs(t, err, &cause)
	assert.EqualError(t, cause, "quota")
	rt.ClearInterrupt()

	rt.Interrupt("not an error")
	_, err = rt.Call(hook)
	require.ErrorAs(t, err, &interrupted)
	assert.Nil(t, errors.Unwrap(err))
	assert.NotErrorIs(t, err, context.DeadlineExceeded)
	rt.ClearInterrupt()
}

// ExampleInterruptedError_Unwrap stops a hook at a deadline and finds the
// deadline in the error.
func ExampleInterruptedError_Unwrap() {
	mod, _ := moejs.Compile("loop.js", "export function loop() { for (;;) {} }")
	hook, _ := mod.Hook("loop")
	rt := moejs.NewRuntime(moejs.Options{})
	_ = rt.Load(mod)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { rt.Interrupt(context.Cause(ctx)) })
	defer stop()
	_, err := rt.Call(hook)
	rt.ClearInterrupt()
	fmt.Println(errors.Is(err, context.DeadlineExceeded))
	// Output: true
}
