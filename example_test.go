package moejs_test

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unsafe"

	"github.com/MoeForge/moejs"
)

// ExampleRuntime_ParseJSONString parses a request body the host holds as a
// []byte without copying it: the host does not modify the body until the
// request is done and nothing derived from it is kept.
func ExampleRuntime_ParseJSONString() {
	mod, _ := moejs.Compile("hook.js", `export function decode(req) { return req.model + " " + req.image.length; }`)
	hook, _ := mod.Hook("decode")
	rt := moejs.NewRuntime(moejs.Options{})
	_ = rt.Load(mod)

	body := []byte(`{"model": "gpt-image-1", "image": "iVBORw0KGgo="}`)
	arg, _ := rt.ParseJSONString(unsafe.String(unsafe.SliceData(body), len(body)))
	res, _ := rt.Call(hook, arg)
	fmt.Println(res.String())
	rt.ReleaseCallData()
	// Output: gpt-image-1 12
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
