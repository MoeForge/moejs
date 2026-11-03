package node

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
)

// moduleID names a module of the package.
type moduleID uint8

const (
	modFS moduleID = iota
	modFSPromises
	modBuffer
	numModules
)

// moduleSpec is a module of the package: its name, what it exports from
// its binding, and the names Node.js exports that it does not implement,
// which throw ERR_METHOD_NOT_IMPLEMENTED.
type moduleSpec struct {
	name    string
	exports []string
}

var moduleSpecs = [numModules]moduleSpec{
	modFS: {name: "node:fs", exports: append(append([]string{}, fsNames...), fsMissing...)},
	modFSPromises: {name: "node:fs/promises",
		exports: append(append([]string{}, promisesNames...), promisesMissing...)},
	modBuffer: {name: "node:buffer", exports: bufferNames},
}

// fsNames are the exports of node:fs that the package implements.
var fsNames = []string{
	"access", "accessSync", "appendFile", "appendFileSync", "constants", "copyFile", "copyFileSync",
	"cp", "cpSync", "exists", "existsSync", "glob", "globSync", "lstat", "lstatSync", "mkdir", "mkdirSync",
	"promises", "readFile", "readFileSync", "readdir", "readdirSync", "readlink", "readlinkSync",
	"realpath", "realpathSync", "rename", "renameSync", "rm", "rmSync", "rmdir", "rmdirSync",
	"stat", "statSync", "unlink", "unlinkSync", "writeFile", "writeFileSync", "Stats", "Dirent",
}

// fsMissing are the functions and classes of Node's node:fs that the
// package does not implement.
var fsMissing = []string{
	"Dir", "ReadStream", "WriteStream", "chmod", "chmodSync", "chown", "chownSync", "close", "closeSync",
	"createReadStream", "createWriteStream", "fchmod", "fchmodSync", "fchown", "fchownSync",
	"fdatasync", "fdatasyncSync", "fstat", "fstatSync", "fsync", "fsyncSync", "ftruncate", "ftruncateSync",
	"futimes", "futimesSync", "lchmod", "lchmodSync", "lchown", "lchownSync", "link", "linkSync",
	"lutimes", "lutimesSync", "mkdtemp", "mkdtempSync", "open", "openAsBlob", "openSync", "opendir",
	"opendirSync", "read", "readSync", "readv", "readvSync", "statfs", "statfsSync", "symlink", "symlinkSync",
	"truncate", "truncateSync", "unwatchFile", "utimes", "utimesSync", "watch", "watchFile", "write",
	"writeSync", "writev", "writevSync",
}

var promisesNames = []string{
	"access", "appendFile", "constants", "copyFile", "cp", "glob", "lstat", "mkdir", "readFile", "readdir",
	"readlink", "realpath", "rename", "rm", "rmdir", "stat", "unlink", "writeFile",
}

var promisesMissing = []string{
	"chmod", "chown", "lchmod", "lchown", "link", "lutimes", "mkdtemp", "open", "opendir", "statfs",
	"symlink", "truncate", "utimes", "watch",
}

var bufferNames = []string{
	"Buffer", "constants", "kMaxLength", "kStringMaxLength", "INSPECT_MAX_BYTES", "isUtf8", "isAscii",
	"atob", "btoa",
}

// specifiers maps the specifiers the package serves to their modules.
var specifiers = map[string]moduleID{
	"node:fs": modFS, "fs": modFS,
	"node:fs/promises": modFSPromises, "fs/promises": modFSPromises,
	"node:buffer": modBuffer, "buffer": modBuffer,
}

// modules are the compiled modules, once per process.
var modules [numModules]struct {
	once sync.Once
	m    atomic.Pointer[moejs.Module]
	err  error
}

// module returns the module id, compiled on first use.
func module(id moduleID) (*moejs.Module, error) {
	x := &modules[id]
	x.once.Do(func() {
		m, err := moejs.Compile(moduleSpecs[id].name, moduleSource(id))
		x.err = err
		x.m.Store(m)
	})
	return x.m.Load(), x.err
}

// moduleSource returns the source of the module id: it reads its binding
// from import.meta and exports its members, with the binding as the
// default export.
func moduleSource(id moduleID) string {
	spec := moduleSpecs[id]
	var b strings.Builder
	b.WriteString("const binding = import.meta.binding;\n")
	b.WriteString("if (binding === undefined) throw new Error('Cannot import \"" + spec.name +
		"\": the host did not install the node package in this runtime');\n")
	b.WriteString("export default binding;\n")
	for _, name := range spec.exports {
		b.WriteString("export const " + name + " = binding." + name + ";\n")
	}
	return b.String()
}

// idOf returns the id of m when it is a module of the package.
func idOf(m *moejs.Module) (moduleID, bool) {
	for i := range modules {
		if modules[i].m.Load() == m {
			return moduleID(i), true
		}
	}
	return 0, false
}

// Resolver returns a Resolver that resolves the specifiers of the package,
// "node:fs", "node:fs/promises", "node:buffer", "fs", "fs/promises" and
// "buffer", to its modules, and passes every other to next. A nil next
// resolves no other specifier: a "node:" one fails with Node's message,
// "No such built-in module: node:path", any other with "Cannot find
// module". The modules are compiled once per process; each exports what
// Install put in its runtime.
func Resolver(next moejs.Resolver) moejs.Resolver {
	return func(referrer moejs.Referrer, specifier string) (*moejs.Module, error) {
		if id, ok := specifiers[specifier]; ok {
			return module(id)
		}
		if next != nil {
			return next(referrer, specifier)
		}
		if strings.HasPrefix(specifier, "node:") {
			return nil, errors.New("No such built-in module: " + specifier)
		}
		return nil, errors.New("Cannot find module '" + specifier + "'")
	}
}

// Importer returns an Importer whose Resolve is Resolver(next.Resolve) and
// whose Meta is next's: the host's Importer with the package's modules
// added. next may be nil. Like any Importer, the result may serve any
// number of runtimes.
func Importer(next *moejs.Importer) *moejs.Importer {
	imp := &moejs.Importer{}
	var resolve moejs.Resolver
	if next != nil {
		resolve, imp.Meta = next.Resolve, next.Meta
	}
	imp.Resolve = Resolver(resolve)
	return imp
}

// hookMeta wraps the runtime's import.meta hook so that the import.meta
// of a module of the package holds this installation's binding; the
// import.meta of every other module is the host's. The hooks stay the
// Importer's otherwise.
func (e *env) hookMeta() {
	r := e.r
	h := &engine.ImportHooks{}
	if prev := r.ImportHooks(); prev != nil {
		*h = *prev
	}
	prevMeta := h.Meta
	h.Meta = func(r *engine.Realm, module any, meta *engine.Object) error {
		if m, ok := module.(*moejs.Module); ok {
			if id, ok := idOf(m); ok {
				return e.fillMeta(id, meta)
			}
		}
		if prevMeta != nil {
			return prevMeta(r, module, meta)
		}
		return nil
	}
	r.SetImportHooks(h)
}

// fillMeta fills the import.meta of the module id with its binding.
func (e *env) fillMeta(id moduleID, meta *engine.Object) error {
	r := e.r
	var binding *engine.Object
	switch id {
	case modFS, modFSPromises:
		if e.fsys == nil {
			return r.Throw(engine.ObjectValue(r.NewError(engine.KindError,
				"Cannot import \"%s\": this runtime has no file system", moduleSpecs[id].name)))
		}
		if id == modFS {
			binding = e.fsModule()
		} else {
			binding = e.promisesModule()
		}
	case modBuffer:
		binding = e.bufferModule()
	}
	meta.DefineOwnDataFast(r, engine.StringKey(r.InternGoString("binding")), engine.ObjectValue(binding), attrMethod)
	return nil
}

// notImplemented returns a function that throws Node's
// ERR_METHOD_NOT_IMPLEMENTED for the function name of module.
func (e *env) notImplemented(module, name string) *engine.Object {
	r := e.r
	return r.NewNativeFunction(r.InternGoString(name), 0, func(r *engine.Realm, _ engine.Value, _ []engine.Value) (engine.Value, error) {
		return undefined, e.throwCoded(engine.KindError, "ERR_METHOD_NOT_IMPLEMENTED",
			"The "+module+"."+name+"() method is not implemented")
	})
}
