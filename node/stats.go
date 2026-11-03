package node

import (
	"io/fs"
	"math"
	"time"

	"github.com/MoeForge/moejs/engine"
)

// statData are the fields of a Node.js Stats object, as numbers.
type statData struct {
	dev, mode, nlink, uid, gid, rdev, blksize, ino, size, blocks float64
	atimeMs, mtimeMs, ctimeMs, birthtimeMs                       float64
}

// ms converts a time to milliseconds as Node's msFromTimeSpec does.
func ms(sec, nsec int64) float64 { return float64(sec)*1000 + float64(nsec)/1e6 }

// Node's file type bits (fs.constants.S_IF*).
const (
	sIFMT   = 0o170000
	sIFSOCK = 0o140000
	sIFLNK  = 0o120000
	sIFREG  = 0o100000
	sIFBLK  = 0o060000
	sIFDIR  = 0o040000
	sIFCHR  = 0o020000
	sIFIFO  = 0o010000
)

// modeBits returns the st_mode of a Go file mode.
func modeBits(m fs.FileMode) uint32 {
	bits := uint32(m.Perm())
	switch {
	case m&fs.ModeDir != 0:
		bits |= sIFDIR
	case m&fs.ModeSymlink != 0:
		bits |= sIFLNK
	case m&fs.ModeNamedPipe != 0:
		bits |= sIFIFO
	case m&fs.ModeSocket != 0:
		bits |= sIFSOCK
	case m&fs.ModeCharDevice != 0:
		bits |= sIFCHR
	case m&fs.ModeDevice != 0:
		bits |= sIFBLK
	default:
		bits |= sIFREG
	}
	return bits
}

// genericStat returns the fields fs.FileInfo has.
func genericStat(info fs.FileInfo) statData {
	t := info.ModTime()
	m := ms(t.Unix(), int64(t.Nanosecond()))
	return statData{mode: float64(modeBits(info.Mode())), nlink: 1, size: float64(info.Size()),
		atimeMs: m, mtimeMs: m, ctimeMs: m, birthtimeMs: m}
}

// Dirent types, libuv's (fs.constants.UV_DIRENT_*).
const (
	direntUnknown = iota
	direntFile
	direntDir
	direntLink
	direntFIFO
	direntSocket
	direntChar
	direntBlock
)

// direntType returns the libuv type of a Go file mode's type bits.
func direntType(m fs.FileMode) int {
	switch {
	case m&fs.ModeDir != 0:
		return direntDir
	case m&fs.ModeSymlink != 0:
		return direntLink
	case m&fs.ModeNamedPipe != 0:
		return direntFIFO
	case m&fs.ModeSocket != 0:
		return direntSocket
	case m&fs.ModeCharDevice != 0:
		return direntChar
	case m&fs.ModeDevice != 0:
		return direntBlock
	case m.Type() == 0:
		return direntFile
	}
	return direntUnknown
}

// direntState is the type of a Dirent, which its methods read.
type direntState struct{ typ int }

// --- Stats --------------------------------------------------------------------------

// statsClasses creates Stats and its prototype chain, as Node's: the
// prototype of Stats.prototype has the is* methods (StatsBase's), and
// Stats.prototype the lazy dates and _checkModeProperty.
func (e *env) statsClasses() {
	if e.statsProto != nil {
		return
	}
	r := e.r
	base := r.NewObjectWithProto(r.ObjectPrototype)
	baseCtor := r.NewNativeConstructor(r.InternGoString("StatsBase"), 10, nil, nil)
	baseCtor.DefineOwnDataFast(r, e.keys.prototype, engine.ObjectValue(base), attrW)
	base.DefineOwnDataFast(r, e.keys.constructor, engine.ObjectValue(baseCtor), attrProto)
	checks := []struct {
		name string
		bits float64
	}{
		{"isDirectory", sIFDIR}, {"isFile", sIFREG}, {"isBlockDevice", sIFBLK}, {"isCharacterDevice", sIFCHR},
		{"isSymbolicLink", sIFLNK}, {"isFIFO", sIFIFO}, {"isSocket", sIFSOCK},
	}
	for _, c := range checks {
		bits := c.bits
		e.method(base, c.name, 0, attrProto, func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
			return e.checkMode(this, bits)
		})
	}
	proto := r.NewObjectWithProto(base)
	ctor := r.NewNativeConstructor(r.InternGoString("Stats"), 14, func(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		return e.statsFromArgs(args, nil)
	}, func(r *engine.Realm, args []engine.Value, newTarget *engine.Object) (engine.Value, error) {
		return e.statsFromArgs(args, newTarget)
	})
	ctor.SetPrototypeOf(r, baseCtor)
	ctor.DefineOwnDataFast(r, e.keys.prototype, engine.ObjectValue(proto), attrW)
	proto.DefineOwnDataFast(r, e.keys.constructor, engine.ObjectValue(ctor), attrProto)
	e.method(proto, "_checkModeProperty", 1, attrProto, func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
		bits, err := r.ToNumber(engine.Arg(args, 0))
		if err != nil {
			return undefined, err
		}
		return e.checkMode(this, bits)
	})
	for _, f := range []struct {
		name string
		ms   engine.PropertyKey
	}{{"atime", e.keys.atimeMs}, {"mtime", e.keys.mtimeMs}, {"ctime", e.keys.ctimeMs}, {"birthtime", e.keys.birthtimeMs}} {
		key := engine.StringKey(r.InternGoString(f.name))
		msKey := f.ms
		get := r.NewNativeFunction(r.InternGoString("get "+f.name), 0, func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
			v, err := r.GetV(this, msKey)
			if err != nil {
				return undefined, err
			}
			n, err := r.ToNumber(v)
			if err != nil {
				return undefined, err
			}
			d := engine.ObjectValue(r.NewDate(time.UnixMilli(int64(math.Round(n)))))
			if math.IsNaN(n) || math.IsInf(n, 0) {
				d = engine.ObjectValue(r.NewDate(time.Time{}))
			}
			return d, setOwn(r, this, key, d)
		})
		set := r.NewNativeFunction(r.InternGoString("set "+f.name), 1, func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
			return undefined, setOwn(r, this, key, engine.Arg(args, 0))
		})
		proto.DefineOwnAccessorFast(r, key, get, set, attrE|attrC)
	}
	e.statsProto = proto
	e.statsCtor = ctor
	k := e.keys
	e.statsTmpl = r.NewObjectTemplate(proto, []engine.PropertyKey{k.dev, k.mode, k.nlink, k.uid, k.gid, k.rdev,
		k.blksize, k.ino, k.size, k.blocks, k.atimeMs, k.mtimeMs, k.ctimeMs, k.birthtimeMs}, attrMethod)
}

// setOwn defines the own data property key of the object v, as Node's
// setOwnProperty does.
func setOwn(r *engine.Realm, v engine.Value, key engine.PropertyKey, val engine.Value) error {
	if !v.IsObject() {
		return r.TypeError("Cannot define property on %s", r.DisplayString(v))
	}
	_, err := v.AsObject().CreateDataProperty(r, key, val)
	return err
}

// checkMode implements _checkModeProperty: whether this.mode's type bits
// are bits.
func (e *env) checkMode(this engine.Value, bits float64) (engine.Value, error) {
	r := e.r
	v, err := r.GetV(this, e.keys.mode)
	if err != nil {
		return undefined, err
	}
	m, err := r.ToNumber(v)
	if err != nil {
		return undefined, err
	}
	return engine.Bool(int64(m)&sIFMT == int64(bits)), nil
}

// statsFromArgs implements new Stats(dev, ..., birthtimeMs).
func (e *env) statsFromArgs(args []engine.Value, newTarget *engine.Object) (engine.Value, error) {
	e.statsClasses()
	r := e.r
	o := r.NewObjectWithProto(e.statsProto)
	if newTarget != nil && newTarget != e.statsCtor {
		var err error
		if o, err = r.OrdinaryCreateFromConstructor(newTarget, e.statsProto, engine.ClassObject); err != nil {
			return undefined, err
		}
	}
	k := e.keys
	for i, key := range []engine.PropertyKey{k.dev, k.mode, k.nlink, k.uid, k.gid, k.rdev, k.blksize, k.ino, k.size, k.blocks,
		k.atimeMs, k.mtimeMs, k.ctimeMs, k.birthtimeMs} {
		o.DefineOwnDataFast(r, key, engine.Arg(args, i), attrMethod)
	}
	return engine.ObjectValue(o), nil
}

// newStats creates the Stats of st: its fourteen fields in one
// allocation, with the shape of the template statsClasses made.
func (e *env) newStats(st statData) engine.Value {
	e.statsClasses()
	n := engine.NumberValue
	vals := [14]engine.Value{n(st.dev), n(st.mode), n(st.nlink), n(st.uid), n(st.gid), n(st.rdev), n(st.blksize),
		n(st.ino), n(st.size), n(st.blocks), n(st.atimeMs), n(st.mtimeMs), n(st.ctimeMs), n(st.birthtimeMs)}
	return engine.ObjectValue(e.r.NewObjectFrom(e.statsTmpl, vals[:]))
}

// --- Dirent -------------------------------------------------------------------------

// direntClass creates Dirent.
func (e *env) direntClass() {
	if e.direntProto != nil {
		return
	}
	r := e.r
	proto := r.NewObjectWithProto(r.ObjectPrototype)
	ctor := r.NewNativeConstructor(r.InternGoString("Dirent"), 3, nil, func(r *engine.Realm, args []engine.Value, newTarget *engine.Object) (engine.Value, error) {
		o, err := r.OrdinaryCreateFromConstructor(newTarget, e.direntProto, engine.ClassObject)
		if err != nil {
			return undefined, err
		}
		t, _ := r.ToNumber(engine.Arg(args, 1))
		o.DefineOwnDataFast(r, e.keys.name, engine.Arg(args, 0), attrMethod)
		o.DefineOwnDataFast(r, e.keys.parentPath, engine.Arg(args, 2), attrMethod)
		o.SetInternal(&direntState{typ: int(t)})
		return engine.ObjectValue(o), nil
	})
	ctor.DefineOwnDataFast(r, e.keys.prototype, engine.ObjectValue(proto), attrW)
	proto.DefineOwnDataFast(r, e.keys.constructor, engine.ObjectValue(ctor), attrProto)
	checks := []struct {
		name string
		typ  int
	}{
		{"isDirectory", direntDir}, {"isFile", direntFile}, {"isBlockDevice", direntBlock}, {"isCharacterDevice", direntChar},
		{"isSymbolicLink", direntLink}, {"isFIFO", direntFIFO}, {"isSocket", direntSocket},
	}
	for _, c := range checks {
		typ := c.typ
		e.method(proto, c.name, 0, attrProto, func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
			if this.IsObject() {
				if d, ok := this.AsObject().Internal().(*direntState); ok {
					return engine.Bool(d.typ == typ), nil
				}
			}
			return engine.False(), nil
		})
	}
	e.direntProto = proto
	e.direntCtor = ctor
}

// newDirent creates a Dirent of name in the directory parent.
func (e *env) newDirent(name, parent engine.Value, typ int) engine.Value {
	e.direntClass()
	r := e.r
	o := r.NewObjectCap(2)
	o.SetPrototypeOf(r, e.direntProto)
	o.DefineOwnDataFast(r, e.keys.name, name, attrMethod)
	o.DefineOwnDataFast(r, e.keys.parentPath, parent, attrMethod)
	o.SetInternal(&direntState{typ: typ})
	return engine.ObjectValue(o)
}
