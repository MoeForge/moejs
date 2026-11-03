package engines

import (
	"errors"
	"fmt"

	"github.com/MoeForge/moejs"
)

// MoejsOptions configures the native adapter.
type MoejsOptions struct {
	// MutableIntrinsics gives every runtime its own mutable builtins instead
	// of the shared frozen ones the plugin host uses.
	MutableIntrinsics bool
	Host              Host
}

// MoejsEngine drives moejs through its native host API (package moejs) the
// way a host built on it does: Compile once, NewRuntime + SetGlobal of utils
// and console as NativeFunc namespaces + Load per runtime, a Hook per
// (export, path) resolved once per runtime, FromGo per argument, Call, ToGo
// on the result.
type MoejsEngine struct {
	opts MoejsOptions
}

// NewMoejsEngine returns the native adapter with the plugin host defaults
// (shared intrinsics, fixed clock).
func NewMoejsEngine() *MoejsEngine { return NewMoejsEngineWith(MoejsOptions{Host: DefaultHost}) }

// NewMoejsEngineWith returns the native adapter with explicit options.
func NewMoejsEngineWith(opts MoejsOptions) *MoejsEngine { return &MoejsEngine{opts: opts} }

func (e *MoejsEngine) Name() string {
	if e.opts.MutableIntrinsics {
		return "moejs-mutable"
	}
	return "moejs"
}

type moejsModule struct {
	mod *moejs.Module
}

func (m *moejsModule) ModuleName() string { return m.mod.Name() }

func (e *MoejsEngine) Compile(name, source string) (CompiledModule, error) {
	mod, err := moejs.Compile(name, source)
	if err != nil {
		return nil, err
	}
	return &moejsModule{mod: mod}, nil
}

// MoejsRuntime is one moejs.Runtime with the host globals installed.
type MoejsRuntime struct {
	rt    *moejs.Runtime
	hooks map[string]*moejsHookNode
}

// moejsHookNode caches the Hook of one path prefix: hooks[export] is the
// export, its members the paths below it, so a lookup walks maps with the
// caller's strings and allocates nothing.
type moejsHookNode struct {
	hook    moejs.Hook
	err     error
	ready   bool
	members map[string]*moejsHookNode
}

func (e *MoejsEngine) NewRuntime() (Runtime, error) {
	rt := moejs.NewRuntime(moejs.Options{MutableIntrinsics: e.opts.MutableIntrinsics})
	if err := installMoejsGlobals(rt, e.opts.Host); err != nil {
		return nil, err
	}
	return &MoejsRuntime{rt: rt, hooks: map[string]*moejsHookNode{}}, nil
}

// Moejs exposes the underlying runtime for footprint and flow measurements.
func (rt *MoejsRuntime) Moejs() *moejs.Runtime { return rt.rt }

func installMoejsGlobals(rt *moejs.Runtime, h Host) error {
	str := func(r *moejs.Realm, v moejs.Value) (string, error) {
		s, err := r.ToString(v)
		if err != nil {
			return "", err
		}
		return s.GoString(), nil
	}
	utils := map[string]any{
		"hasCapability": moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
			name, err := str(r, moejs.Arg(args, 0))
			if err != nil {
				return moejs.Undefined(), err
			}
			return moejs.Bool(h.HasCapability(name)), nil
		}),
		"json": map[string]any{
			"clone": moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
				arg := moejs.Arg(args, 0)
				if arg.IsUndefined() {
					return moejs.Undefined(), errJSONCloneUndefined
				}
				cloned, err := h.JSONClone(r.ToGo(arg))
				if err != nil {
					return moejs.Undefined(), err
				}
				return r.FromGo(cloned)
			}),
		},
		"unixNow": moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
			return moejs.Int(h.UnixNow), nil
		}),
		"jwtSignHS256": moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
			claims, _ := r.ToGo(moejs.Arg(args, 0)).(map[string]any)
			secret, err := str(r, moejs.Arg(args, 1))
			if err != nil {
				return moejs.Undefined(), err
			}
			token, err := h.JWTSignHS256(claims, secret)
			if err != nil {
				return moejs.Undefined(), err
			}
			return moejs.String(token), nil
		}),
		"hmacSHA256": moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
			message, err := str(r, moejs.Arg(args, 0))
			if err != nil {
				return moejs.Undefined(), err
			}
			secret, err := str(r, moejs.Arg(args, 1))
			if err != nil {
				return moejs.Undefined(), err
			}
			return moejs.String(h.HmacSHA256(message, secret)), nil
		}),
		"base64": moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
			v, err := str(r, moejs.Arg(args, 0))
			if err != nil {
				return moejs.Undefined(), err
			}
			return moejs.String(h.Base64(v)), nil
		}),
		"base64URL": moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
			v, err := str(r, moejs.Arg(args, 0))
			if err != nil {
				return moejs.Undefined(), err
			}
			return moejs.String(h.Base64URL(v)), nil
		}),
		"base64URLDecode": moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
			v, err := str(r, moejs.Arg(args, 0))
			if err != nil {
				return moejs.Undefined(), err
			}
			decoded, err := h.Base64URLDecode(v)
			if err != nil {
				return moejs.Undefined(), err
			}
			return moejs.String(decoded), nil
		}),
		"uuid": moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
			return moejs.String(h.UUID), nil
		}),
		"volcSignV4": moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
			req, _ := r.ToGo(moejs.Arg(args, 0)).(map[string]any)
			signed, err := h.VolcSignV4(req)
			if err != nil {
				return moejs.Undefined(), err
			}
			return r.FromGo(signed)
		}),
	}
	if err := rt.SetGlobal("utils", utils); err != nil {
		return err
	}
	return rt.SetGlobal("console", map[string]any{
		"log": moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
			for _, a := range args {
				if _, err := r.ToString(a); err != nil {
					return moejs.Undefined(), err
				}
			}
			return moejs.Undefined(), nil
		}),
	})
}

func (rt *MoejsRuntime) Instantiate(module CompiledModule) error {
	m, ok := module.(*moejsModule)
	if !ok {
		return fmt.Errorf("moejs: module compiled by another engine (%T)", module)
	}
	return wrapMoejsError(rt.rt.Load(m.mod))
}

// Hook returns the cached Hook of export and path.
func (rt *MoejsRuntime) Hook(export string, path []string) (moejs.Hook, error) {
	mod := rt.rt.Module()
	if mod == nil {
		return moejs.Hook{}, errors.New("moejs: module not instantiated")
	}
	n := rt.hooks[export]
	if n == nil {
		n = &moejsHookNode{}
		rt.hooks[export] = n
	}
	for _, member := range path {
		next := n.members[member]
		if next == nil {
			if n.members == nil {
				n.members = map[string]*moejsHookNode{}
			}
			next = &moejsHookNode{}
			n.members[member] = next
		}
		n = next
	}
	if !n.ready {
		n.hook, n.err = mod.Hook(export, path...)
		n.ready = true
	}
	return n.hook, n.err
}

func (rt *MoejsRuntime) Call(export string, path []string, args ...any) (any, error) {
	prepared, err := rt.Prepare(args...)
	if err != nil {
		return nil, err
	}
	raw, err := rt.CallPrepared(export, path, prepared)
	if err != nil {
		return nil, err
	}
	v := raw.(moejs.Value)
	if state, result, ok := moejs.PromiseResult(v); ok {
		return rt.promise(state, result)
	}
	out, err := rt.rt.ToGo(v)
	return out, wrapMoejsError(err)
}

// promise exports a promise's state and result as {state, value} (see
// settledPromise).
func (rt *MoejsRuntime) promise(state moejs.PromiseState, result moejs.Value) (any, error) {
	name := "fulfilled"
	switch state {
	case moejs.PromisePending:
		return settledPromise("pending", nil), nil
	case moejs.PromiseRejected:
		name = "rejected"
	}
	out, err := rt.rt.ToGo(result)
	return settledPromise(name, out), wrapMoejsError(err)
}

func (rt *MoejsRuntime) Prepare(args ...any) (PreparedArgs, error) {
	vals := make([]moejs.Value, len(args))
	for i, a := range args {
		v, err := rt.rt.FromGo(a)
		if err != nil {
			return nil, err
		}
		vals[i] = v
	}
	return vals, nil
}

func (rt *MoejsRuntime) CallPrepared(export string, path []string, args PreparedArgs) (RawResult, error) {
	h, err := rt.Hook(export, path)
	if err != nil {
		return nil, wrapMoejsError(err)
	}
	out, err := rt.rt.Call(h, args.([]moejs.Value)...)
	if err != nil {
		return nil, wrapMoejsError(err)
	}
	return out, nil
}

func (rt *MoejsRuntime) Export(raw RawResult) any {
	out, _ := rt.rt.ToGo(raw.(moejs.Value))
	return out
}

func (rt *MoejsRuntime) Close() {}

// RunScript runs source as a script, sloppy unless it starts with a "use
// strict" directive, and exports its completion value (the corpus programs
// that test sloppy mode are scripts).
func (rt *MoejsRuntime) RunScript(name, source string) (any, error) {
	s, err := moejs.CompileScript(name, source)
	if err != nil {
		return nil, err
	}
	v, err := rt.rt.RunScript(s)
	if err != nil {
		return nil, wrapMoejsError(err)
	}
	out, err := rt.rt.ToGo(v)
	return out, wrapMoejsError(err)
}

// wrapMoejsError maps the native API's errors onto the adapter contract:
// a missing or non-callable hook is ErrNotFound, a throw a HookError from
// the exception's data properties (no user code runs, so a thrown object
// without a data message has an empty Message where Sobek runs toString).
func wrapMoejsError(err error) error {
	if err == nil {
		return nil // errors.As would allocate exc on every call
	}
	if errors.Is(err, moejs.ErrHookNotFound) || errors.Is(err, moejs.ErrNotCallable) {
		return ErrNotFound
	}
	var exc *moejs.Exception
	if !errors.As(err, &exc) {
		return err
	}
	return &HookError{Name: exc.Name(), Message: exc.Message()}
}
