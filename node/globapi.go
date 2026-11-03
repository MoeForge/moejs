package node

import (
	"errors"
	"math"
	"path"

	"github.com/MoeForge/moejs/engine"
)

// globOp implements glob, globSync and fs.promises.glob, which returns an
// async iterator of the matches rather than a promise.
func (e *env) globOp(c call, args []engine.Value) (engine.Value, error) {
	r := e.r
	pv := engine.Arg(args, 0)
	if !pv.IsString() {
		if ok, err := r.IsArray(pv); err != nil {
			return undefined, err
		} else if !ok {
			return undefined, e.argTypeError("patterns", "of type string", pv)
		}
	}
	// The patterns and the exclude patterns of one call hold at most
	// maxCallPatterns bytes in all, each distinct string counted and
	// copied once, so that an array that repeats one long pattern costs
	// what the pattern does.
	seen := map[*engine.String]bool{}
	left := maxCallPatterns
	patterns, err := e.globStrings(pv, "patterns", seen, &left)
	if err != nil {
		return undefined, err
	}
	opts := engine.Arg(args, 1)
	if isFunction(opts) {
		opts = undefined
	}
	if !opts.IsNullish() && !opts.IsObject() {
		return undefined, e.argTypeError("options", "of type object", opts)
	}
	var o globOptions
	cwd := "."
	if cv, err := e.optValue(opts, e.keys.cwd); err != nil {
		return undefined, err
	} else if !cv.IsUndefined() {
		if cwd, err = e.pathArg(cv, "options.cwd"); err != nil {
			return undefined, err
		}
	}
	if o.withTypes, err = e.optTruthy(opts, e.keys.withFileTypes); err != nil {
		return undefined, err
	}
	if o.followSymlinks, err = e.optBool(opts, e.keys.followSymlinks, "followSymlinks", false); err != nil {
		return undefined, err
	}
	excludeFn := undefined
	var excludes []string
	if xv, err := e.optValue(opts, e.keys.exclude); err != nil {
		return undefined, err
	} else if isFunction(xv) {
		excludeFn = xv
	} else if !xv.IsNullish() {
		ok, err := r.IsArray(xv)
		if err != nil {
			return undefined, err
		}
		if !ok {
			return undefined, e.argTypeError("options.exclude", "of type function or an instance of Array", xv)
		}
		if excludes, err = e.globStrings(xv, "options.exclude", seen, &left); err != nil {
			return undefined, err
		}
	}
	// What compiling takes is bounded and charged, and the runtime's
	// interrupt is checked between the expressions.
	cc := &globCompile{charge: r.ChargeMemory, halt: r.CheckInterrupt}
	compile := func(list []string) ([]globPattern, error) {
		var out []globPattern
		done := map[string]bool{}
		for _, p := range list {
			if done[p] {
				continue
			}
			done[p] = true
			g, err := compileGlob(p, cc)
			if err != nil {
				var ge *globError
				if errors.As(err, &ge) {
					return nil, r.Throw(r.NewErrorValue(engine.KindTypeError, engine.FromGoString(ge.msg)))
				}
				return nil, err
			}
			out = append(out, g...)
		}
		return out, nil
	}
	if o.exclude, err = compile(excludes); err != nil {
		return undefined, err
	}
	compiled, err := compile(patterns)
	if err != nil {
		return undefined, err
	}
	f := e.fsys
	hold := &holding{e: e, sync: c.mode == modeSync, what: "list of matches"}
	c.hold = hold
	o.grow = hold.grow
	done := func(ms []globMatch) (engine.Value, error) {
		return e.globValue(cwd, ms, o.withTypes, excludeFn, hold.sync)
	}
	work := func() ([]globMatch, error) { return f.glob(cwd, compiled, o) }
	if c.mode != modePromise {
		return run(e, c, work, done)
	}
	p, err := run(e, c, work, done)
	if err != nil {
		return undefined, err
	}
	return e.asyncIterator(p), nil
}

// maxCallPatterns bounds the bytes of the distinct pattern strings of one
// call of glob, the exclude patterns included, minimatch's bound on one
// pattern, and their number.
const maxCallPatterns = 64 << 10

// globStrings reads a pattern, or an array of them (name for its errors),
// as Go strings: each distinct string once (seen), its length counted
// against left before it is copied.
func (e *env) globStrings(v engine.Value, name string, seen map[*engine.String]bool, left *int) ([]string, error) {
	r := e.r
	var out []string
	add := func(s *engine.String) error {
		if seen[s] {
			return nil
		}
		seen[s] = true
		if *left -= s.Len(); *left < 0 {
			return r.Throw(r.NewErrorValue(engine.KindTypeError, engine.FromGoString(
				"The glob patterns hold more than "+itoa(maxCallPatterns)+" characters in all")))
		}
		if len(seen) > maxCallPatterns {
			return r.Throw(r.NewErrorValue(engine.KindTypeError, engine.FromGoString(
				"There are more than "+itoa(maxCallPatterns)+" glob patterns")))
		}
		out = append(out, s.GoString())
		return nil
	}
	if v.IsString() {
		return out, add(v.AsString())
	}
	o := v.AsObject()
	n, err := r.LengthOfArrayLike(o)
	if err != nil {
		return nil, err
	}
	for i := int64(0); i < n; i++ {
		if i&1023 == 1023 {
			if err := r.CheckInterrupt(); err != nil {
				return nil, err
			}
		}
		item, err := o.GetProp(r, r.KeyFromGoString(itoa(i)))
		if err != nil {
			return nil, err
		}
		if !item.IsString() {
			return nil, e.argTypeError(name+"["+itoa(i)+"]", "of type string", item)
		}
		if err := add(item.AsString()); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// globValue converts the matches of glob: paths, or Dirents whose
// parentPath is the match's directory joined to cwd, without those the
// exclude function excludes, or whose directory it does.
func (e *env) globValue(cwd string, ms []globMatch, withTypes bool, excludeFn engine.Value, charged bool) (engine.Value, error) {
	r := e.r
	if !charged {
		size := 0
		for _, m := range ms {
			size += len(m.path) + listedOverhead
		}
		if err := r.ChargeMemory(size); err != nil {
			return undefined, err
		}
	}
	items := make([]engine.Value, 0, len(ms))
	excluded := map[string]bool{}
	var isExcluded func(p string, typ int) (bool, error)
	isExcluded = func(p string, typ int) (bool, error) {
		if excludeFn.IsUndefined() {
			return false, nil
		}
		if x, ok := excluded[p]; ok {
			return x, nil
		}
		if dir := path.Dir(p); dir != "." && dir != "/" && dir != p {
			if x, err := isExcluded(dir, direntDir); err != nil || x {
				excluded[p] = x
				return x, err
			}
		}
		arg := str(p)
		if withTypes {
			arg = e.newDirent(str(path.Base(p)), str(path.Dir(path.Join(cwd, p))), typ)
		}
		v, err := r.Call(excludeFn, undefined, []engine.Value{arg})
		if err != nil {
			return false, err
		}
		x := engine.ToBoolean(v)
		excluded[p] = x
		return x, nil
	}
	for _, m := range ms {
		typ := direntType(m.typ)
		if x, err := isExcluded(m.path, typ); err != nil {
			return undefined, err
		} else if x {
			continue
		}
		if withTypes {
			full := path.Join(cwd, m.path)
			items = append(items, e.newDirent(str(path.Base(full)), str(path.Dir(full)), typ))
			continue
		}
		items = append(items, str(m.path))
	}
	return engine.ObjectValue(r.NewArrayFromSlice(items)), nil
}

// asyncIterator returns an async iterator of the items of the array that
// the promise p fulfills with, as fs.promises.glob returns: next() waits
// for the walk, then yields one match after the other.
func (e *env) asyncIterator(p engine.Value) engine.Value {
	r := e.r
	it := r.NewObject()
	index := 0
	e.method(it, "next", 0, attrProto, func(r *engine.Realm, _ engine.Value, _ []engine.Value) (engine.Value, error) {
		i := index
		index++
		return e.then(p, func(arr engine.Value) (engine.Value, error) {
			n, err := r.LengthOfArrayLike(arr.AsObject())
			if err != nil {
				return undefined, err
			}
			res := r.NewObject()
			if int64(i) >= n {
				res.DefineOwnDataFast(r, engine.StringKey(r.InternGoString("value")), undefined, attrMethod)
				res.DefineOwnDataFast(r, engine.StringKey(r.InternGoString("done")), engine.Bool(true), attrMethod)
				return engine.ObjectValue(res), nil
			}
			v, err := arr.AsObject().GetProp(r, r.KeyFromGoString(itoa(int64(i))))
			if err != nil {
				return undefined, err
			}
			res.DefineOwnDataFast(r, engine.StringKey(r.InternGoString("value")), v, attrMethod)
			res.DefineOwnDataFast(r, engine.StringKey(r.InternGoString("done")), engine.Bool(false), attrMethod)
			return engine.ObjectValue(res), nil
		})
	})
	e.method(it, "return", 1, attrProto, func(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		index = math.MaxInt
		res := r.NewObject()
		res.DefineOwnDataFast(r, engine.StringKey(r.InternGoString("value")), engine.Arg(args, 0), attrMethod)
		res.DefineOwnDataFast(r, engine.StringKey(r.InternGoString("done")), engine.Bool(true), attrMethod)
		return e.then(p, func(engine.Value) (engine.Value, error) { return engine.ObjectValue(res), nil })
	})
	self := r.NewNativeFunction(r.InternGoString("[Symbol.asyncIterator]"), 0, func(*engine.Realm, engine.Value, []engine.Value) (engine.Value, error) {
		return engine.ObjectValue(it), nil
	})
	it.DefineOwnDataFast(r, engine.SymbolKey(engine.SymAsyncIterator), engine.ObjectValue(self), attrProto)
	return engine.ObjectValue(it)
}

// handled gives the promise p a reaction to its rejection that does
// nothing, so that its rejection is not reported as unhandled.
func (e *env) handled(p engine.Value) error {
	r := e.r
	then, err := p.AsObject().GetProp(r, engine.StringKey(r.InternGoString("then")))
	if err != nil {
		return err
	}
	noop := r.NewNativeFunction(engine.EmptyString(), 1, func(*engine.Realm, engine.Value, []engine.Value) (engine.Value, error) {
		return undefined, nil
	})
	_, err = r.Call(then, p, []engine.Value{undefined, engine.ObjectValue(noop)})
	return err
}

// then returns p.then(f), with the realm's own then.
func (e *env) then(p engine.Value, f func(engine.Value) (engine.Value, error)) (engine.Value, error) {
	r := e.r
	then, err := p.AsObject().GetProp(r, engine.StringKey(r.InternGoString("then")))
	if err != nil {
		return undefined, err
	}
	fn := r.NewNativeFunction(engine.EmptyString(), 1, func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		return f(engine.Arg(args, 0))
	})
	return r.Call(then, p, []engine.Value{engine.ObjectValue(fn)})
}
