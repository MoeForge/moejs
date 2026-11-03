// Package node installs a part of Node.js's built-in modules into a moejs
// runtime that runs on an event loop: node:fs and
// node:fs/promises over one directory of the host, and node:buffer with
// its Buffer class. It is meant for sandboxes, where a program reads and
// perhaps writes the files of a workspace and nothing else.
//
//	imp := node.Importer(hostImporter) // serves node:fs, node:fs/promises, node:buffer
//	rt := moejs.NewRuntime(moejs.Options{Importer: imp})
//	loop, err := eventloop.New(rt, eventloop.Options{})
//	api, err := node.InstallLoop(loop, node.Options{
//		FS:     &node.FSOptions{Root: "/srv/ws/123", Mount: "/workspace"},
//		Buffer: true, // the global Buffer
//	})
//	defer api.Close()
//	err = loop.Run(func(rt *moejs.Runtime) error { ... })
//
// The modules are small ES modules, compiled once per process, that
// Importer and Resolver return for the specifiers "node:fs",
// "node:fs/promises" and "node:buffer" and their bare forms "fs",
// "fs/promises" and "buffer". Each reads what it exports from its
// import.meta, which Install fills for its own runtime: a runtime without
// Install, or one whose Options have no FS for node:fs, fails the import
// with an Error that says so.
//
// # Sandbox
//
// Every path is resolved inside FSOptions.Root through an os.Root, so
// neither ".." nor a symbolic link leaves it. JavaScript sees Root at
// FSOptions.Mount, and relative paths resolve against FSOptions.Cwd. A
// path outside the mount, and one that a link or ".." takes out of the
// root, fails with EACCES. Writes fail with EROFS unless FSOptions.Write
// is set, and FSOptions.Allow sees every access first. See the package's
// documentation in docs/guide.md for the whole contract and the
// deviations from Node.js.
//
// # Event loop and resources
//
// The loop is package eventloop's (InstallLoop) or the host's own, which
// gives the package what Loop names: a way to run a task on the
// runtime's goroutine and a hold that keeps it alive. The synchronous
// functions run on the loop, as in Node.js. The promise
// and callback forms run their file system work on a bounded pool of
// goroutines (FSOptions.Workers) and settle on the loop, in a task of its
// tasks phase; an operation in flight keeps Run alive. What they hand to
// JavaScript, file contents, listings and names, is charged to the
// runtime's memory limit (moejs.Options.MemoryLimit) on the loop, and a
// file larger than the limit is refused before it is read.
package node

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
	"github.com/MoeForge/moejs/eventloop"
)

// Options configures Install.
type Options struct {
	// FS gives the runtime node:fs and node:fs/promises over a directory
	// of the host. Nil means no file system: importing them fails.
	FS *FSOptions
	// Buffer defines the global Buffer, as Node.js has it. node:buffer
	// can be imported either way.
	Buffer bool
}

// FSOptions configures node:fs.
type FSOptions struct {
	// Root is the directory of the host that the file system is: every
	// path resolves inside it. It must exist. Install opens it, and the
	// directory stays the same one if it is renamed later.
	Root string
	// Mount is the absolute path at which JavaScript sees Root, such as
	// "/workspace". An absolute path JavaScript gives must lie under it,
	// and realpath and glob return paths under it. Empty means Root
	// itself.
	Mount string
	// Cwd is the directory relative paths resolve against, as
	// process.cwd() is in Node.js: an absolute path under Mount, or a
	// path relative to Mount. Empty means Mount.
	Cwd string
	// Write allows the functions that change the file system: writeFile,
	// appendFile, mkdir, rm, rmdir, unlink, rename, copyFile and cp.
	// Without it they fail with EROFS.
	Write bool
	// Allow, when not nil, approves each access before it is made. op is
	// "read" for one that reads (contents, listings, metadata) and "write"
	// for one that changes the file system, as in Node.js's permission
	// model. path is the absolute path in JavaScript's view (under Mount)
	// with every symbolic link resolved, so that a link cannot dodge the
	// policy; a path that does not exist yet has its existing part
	// resolved. An error fails the operation: one that errors.Is finds
	// fs.ErrNotExist fails it with ENOENT, a syscall.Errno with that
	// errno, any other with EACCES. It is called on the loop for the
	// synchronous functions and on a worker goroutine for the others, so
	// concurrently.
	Allow func(op, path string) error
	// Workers bounds how many promise and callback operations do their
	// file system work at once; the others wait in a queue. 0 means 4,
	// the size of libuv's pool.
	Workers int
	// NoDescriptorCache turns off, on Linux, the cache of the descriptors
	// of the directories an operation looked in within the last
	// millisecond, which makes a stat or an open of a name in one a
	// single system call. A cached descriptor follows its directory if
	// another process renames it, out of the root too, for up to that
	// millisecond: set this when other processes may rearrange the tree
	// while the program runs. See docs/guide.md.
	NoDescriptorCache bool
}

// API is the modules installed in one runtime.
type API struct{ e *env }

// Loop is what the package needs of the event loop its runtime runs on:
// a way to run a task on the runtime's goroutine, and a hold that keeps
// the loop alive while work is in flight. A promise or callback operation
// takes a hold when it starts, on the runtime's goroutine, and when its
// work is done posts the task that settles it and then releases the hold.
// InstallLoop adapts package eventloop's loop; a host that drives its own
// loop implements Loop.
type Loop interface {
	// Runtime returns the loop's runtime.
	Runtime() *moejs.Runtime
	// Post queues task to run on the runtime's goroutine, outside any
	// JavaScript call, as the loop runs a callback, and reports whether
	// it took it: false once the loop is over, and the task is dropped.
	// It is called from any goroutine. The loop handles the task's error
	// as a callback's: an *moejs.InterruptedError, such as the memory
	// limit's, stops the program, and so does an exception, which a
	// callback of the callback form throws as an uncaught exception does
	// in Node.js. The task runs the promise jobs it queues before it
	// returns.
	Post(task func(*moejs.Runtime) error) bool
	// Hold keeps the loop alive until release is called: a loop with no
	// other work waits for the tasks that a held operation posts rather
	// than end. Hold is called on the runtime's goroutine, release from
	// any goroutine, once, after the operation's Post.
	Hold() (release func())
}

// InstallLoop is Install on a loop of package eventloop.
func InstallLoop(loop *eventloop.Loop, opts Options) (*API, error) {
	if loop == nil {
		return nil, errors.New("node: InstallLoop needs a loop")
	}
	return Install(eventLoop{loop}, opts)
}

// eventLoop is a loop of package eventloop as a Loop.
type eventLoop struct{ l *eventloop.Loop }

func (a eventLoop) Runtime() *moejs.Runtime { return a.l.Runtime() }

func (a eventLoop) Post(task func(*moejs.Runtime) error) bool { return eventloop.Post(a.l, task) }

func (a eventLoop) Hold() func() {
	done := a.l.Hold()
	return func() { done(nil) }
}

// Install installs the package into the loop's runtime: the bindings that
// the modules of Importer and Resolver export in it, and the global Buffer
// when opts.Buffer is set. It is called once per runtime, while the loop
// does not run or on the loop. It wraps the import.meta hook of the
// runtime's Importer, so the runtime keeps the Importer it was created
// with.
func Install(loop Loop, opts Options) (*API, error) {
	if loop == nil {
		return nil, errors.New("node: Install needs a loop")
	}
	rt := loop.Runtime()
	e := &env{loop: loop, rt: rt, r: rt.Realm(), opts: opts}
	if opts.FS != nil {
		fsys, err := openFS(opts.FS)
		if err != nil {
			return nil, err
		}
		e.fsys = fsys
		fsys.halt = e.halt
		e.pool = newPool(opts.FS.Workers)
		e.budget = newBudget(e.r.MemoryLimit())
	}
	atoms.once.Do(func() { atoms.init(e.r) })
	e.keys = &atoms.keys
	e.hookMeta()
	if opts.Buffer {
		r := e.r
		r.Global.DefineLazyProperty(r, atoms.buffer, func(*engine.Realm) engine.Value {
			return engine.ObjectValue(e.buffer().ctor)
		})
	}
	if !e.r.HasSharedIntrinsics() {
		// A script can replace or delete mutable builtins: take them
		// before one runs.
		e.intr = e.captureIntrinsics()
	}
	return &API{e}, nil
}

// Close closes the root directory and ends the operations that wait for a
// worker: their promises reject, and their callbacks are called, with an
// EBADF error the next time the loop runs. Operations that a worker runs
// finish first. A file system function called after Close fails with
// EBADF. Close may be called from any goroutine, and more than once.
func (a *API) Close() error {
	e := a.e
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	e.halted.Store(true)
	e.mu.Unlock()
	var err error
	if e.pool != nil {
		e.pool.close()
	}
	e.budget.close()
	if e.fsys != nil {
		err = e.fsys.close()
	}
	if g := e.liveReg; g != nil {
		g.e.Store(nil)
	}
	return err
}

// env is the state of one installation. Except for mu and what it guards,
// and the file system and the pool, which are safe for concurrent use,
// only the loop's goroutine uses it.
type env struct {
	loop   Loop
	rt     *moejs.Runtime
	r      *engine.Realm
	opts   Options
	keys   *keys
	fsys   *fsys
	pool   *pool
	budget *budget // nil without a memory limit
	intr   *intrinsics

	mu     sync.Mutex
	closed bool
	// halted mirrors closed for the walks on the workers (halt).
	halted atomic.Bool
	// epoch advances each time the package sees the runtime interrupted:
	// the operations of an interrupted program that have not started
	// when it advances are dropped rather than run (run).
	epoch atomic.Uint64

	// The objects of the modules, created on first use.
	fsObj, promisesObj, bufferMod *engine.Object
	bufferCls                     *bufferClass
	statsTmpl                     *engine.ObjectTemplate
	statsProto, direntProto       *engine.Object
	statsCtor, direntCtor         *engine.Object
	constants                     *engine.Object
	inspectCustom                 *engine.Symbol

	// inflight lists the promises and callbacks of the operations in
	// flight, under a memory limit, for its measurements (live.go).
	inflight map[*inflightOp]struct{}
	liveReg  *liveReg
}

// keys are the property keys the package uses often.
type keys struct {
	encoding, flag, withFileTypes, recursive, bigint, throwIfNoEntry, mode   engine.PropertyKey
	force, maxRetries, retryDelay, errorOnExist, filter, dereference         engine.PropertyKey
	cwd, exclude, followSymlinks, length, href, protocol, pathname, hostname engine.PropertyKey
	errno, code, syscall, path, dest, name, parentPath, message, info        engine.PropertyKey
	dev, ino, nlink, uid, gid, rdev, size, blksize, blocks                   engine.PropertyKey
	atimeMs, mtimeMs, ctimeMs, birthtimeMs, typ, data, buffer, byteOffset    engine.PropertyKey
	constructor, prototype, flush, preserveTimestamps, verbatimSymlinks      engine.PropertyKey
}

// atoms are the property keys of the package, interned once per process.
var atoms atomTable

type atomTable struct {
	once   sync.Once
	keys   keys
	buffer engine.PropertyKey // the global Buffer
}

func (a *atomTable) init(r *engine.Realm) {
	k := func(s string) engine.PropertyKey { return engine.StringKey(r.InternGoString(s)) }
	a.keys = keys{
		encoding: k("encoding"), flag: k("flag"), withFileTypes: k("withFileTypes"), recursive: k("recursive"),
		bigint: k("bigint"), throwIfNoEntry: k("throwIfNoEntry"), mode: k("mode"),
		force: k("force"), maxRetries: k("maxRetries"), retryDelay: k("retryDelay"), errorOnExist: k("errorOnExist"),
		filter: k("filter"), dereference: k("dereference"),
		cwd: k("cwd"), exclude: k("exclude"), followSymlinks: k("followSymlinks"), length: k("length"),
		href: k("href"), protocol: k("protocol"), pathname: k("pathname"), hostname: k("hostname"),
		errno: k("errno"), code: k("code"), syscall: k("syscall"), path: k("path"), dest: k("dest"),
		name: k("name"), parentPath: k("parentPath"), message: k("message"), info: k("info"),
		dev: k("dev"), ino: k("ino"), nlink: k("nlink"), uid: k("uid"), gid: k("gid"), rdev: k("rdev"),
		size: k("size"), blksize: k("blksize"), blocks: k("blocks"),
		atimeMs: k("atimeMs"), mtimeMs: k("mtimeMs"), ctimeMs: k("ctimeMs"), birthtimeMs: k("birthtimeMs"),
		typ: k("type"), data: k("data"), buffer: k("buffer"), byteOffset: k("byteOffset"),
		constructor: k("constructor"), prototype: k("prototype"), flush: k("flush"),
		preserveTimestamps: k("preserveTimestamps"), verbatimSymlinks: k("verbatimSymlinks"),
	}
	a.buffer = k("Buffer")
}

// intrinsics are the builtins the package calls: the realm's own, never
// what a script put in their place (see webapi's).
type intrinsics struct {
	uint8Array, uint8ArrayProto *engine.Object
	uint8ClampedProto           *engine.Object
	arrayBuffer                 *engine.Object
	symbolFor                   *engine.Object
}

// halt returns the error that stops a long walk of the file system or a
// long native loop: Close's, or the runtime's pending interrupt (the
// host's Interrupt or Terminate, or the memory limit's). It is safe on
// any goroutine.
func (e *env) halt() error {
	if e.halted.Load() {
		return closedError()
	}
	if err := e.r.CheckInterrupt(); err != nil {
		e.epoch.Add(1)
		return err
	}
	return nil
}

// intrinsics returns the builtins the package calls.
func (e *env) intrinsics() *intrinsics {
	if e.intr == nil {
		e.intr = e.captureIntrinsics()
	}
	return e.intr
}

func (e *env) captureIntrinsics() *intrinsics {
	r := e.r
	get := func(o *engine.Object, name string) *engine.Object {
		if o == nil {
			return nil
		}
		v, err := o.GetProp(r, r.KeyFromGoString(name))
		if err != nil || !v.IsObject() {
			return nil
		}
		return v.AsObject()
	}
	x := &intrinsics{uint8Array: get(r.Global, "Uint8Array"), arrayBuffer: get(r.Global, "ArrayBuffer")}
	x.uint8ArrayProto = get(x.uint8Array, "prototype")
	x.uint8ClampedProto = get(get(r.Global, "Uint8ClampedArray"), "prototype")
	x.symbolFor = get(get(r.Global, "Symbol"), "for")
	return x
}

// Property attribute bits of the engine (engine.DataDescriptor).
const (
	attrW = 1 << 0
	attrE = 1 << 1
	attrC = 1 << 2
	// attrMethod is the attribute set of a method of a Node.js module or
	// class: writable, enumerable and configurable.
	attrMethod = attrW | attrE | attrC
	// attrProto is the attribute set of a method of a class prototype:
	// writable and configurable, not enumerable.
	attrProto = attrW | attrC
)

var undefined = engine.Undefined()

// str converts a Go string.
func str(s string) engine.Value { return engine.StringValue(engine.FromGoString(s)) }

// method defines the native method name of o.
func (e *env) method(o *engine.Object, name string, length int, attrs uint8, fn engine.NativeFunc) *engine.Object {
	r := e.r
	f := r.NewNativeFunction(r.InternGoString(name), length, fn)
	o.DefineOwnDataFast(r, engine.StringKey(r.InternGoString(name)), engine.ObjectValue(f), attrs)
	return f
}

// set defines the data property name of o.
func (e *env) set(o *engine.Object, name string, v engine.Value, attrs uint8) {
	o.DefineOwnDataFast(e.r, engine.StringKey(e.r.InternGoString(name)), v, attrs)
}
