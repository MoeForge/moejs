<div align="center">

<img src="docs/assets/moejs-logo.svg" width="160" alt="moejs">

# moejs

A JavaScript runtime in pure Go, for running JavaScript plugins inside Go programs.

<p align="center">
  <a href="./README.zh_CN.md">简体中文</a> |
  <strong>English</strong>
</p>

</div>

moejs supports modern JavaScript, including ES modules, classes,
async/await, Proxy and BigInt, and passes all 79,385 test262 tests it runs.

On the same plugins as Sobek, another pure-Go engine, a moejs call takes
about half the time and a runtime uses about a third of the memory, so a
server can give every concurrent request its own runtime.

## Performance

<p align="center">
  <img src="docs/assets/plugin-bench.en.png" alt="moejs plugin benchmark: throughput on small task-plugin calls, 100k-token agent requests and 8 MiB image requests, and memory in use at 32 workers">
</p>

The chart runs new-api's plugin workloads on the engines a Go program can
embed: small task-plugin calls, 100k-token coding-agent requests that a plugin
rewrites for the upstream, and requests that carry an 8 MiB image. QuickJS
called straight from C is shown in grey for reference. The chart was
measured on 2026-10-21 with moejs v0.1.0-alpha.9 on 8 vCPUs of an Intel
Xeon E5-2667 v4, and the setup is in
[docs/performance.md](docs/performance.md#plugin-scenarios).

The table below times single calls on new-api's 10 task plugins and 269
recorded calls, on the Go caller's side, including converting the arguments
and the result.

| | moejs | Sobek | QuickJS pure Go (modernc) | QuickJS (quickjs-go, default settings) | V8 (v8go) |
|---|--:|--:|--:|--:|--:|
| One plugin call | 6.9 µs | 14.3 µs | 35.2 µs | 104.5 µs | 56.6 µs ¹ |
| New runtime | 1.4 µs | 2.2 µs | 180 µs | 382 µs | 1,153 µs ¹ |
| Memory per runtime with the largest plugin loaded | 81 KiB | 264 KiB | 227 KiB ³ | 348 KiB ² | 1,544 KiB ² |

¹ V8's timings varied widely on the test machine. ² The engine's own heap.
³ How much the resident set grew: modernc.org/quickjs allocates outside the
Go heap and keeps no in-use count.

Sobek and modernc.org/quickjs, which is QuickJS translated to Go, are pure-Go
engines like moejs. quickjs-go and V8 run through cgo. The test machine, the
full results and how to reproduce them are in
[docs/performance.md](docs/performance.md).

## Features

### Pure Go

moejs builds with `CGO_ENABLED=0` and cross-compiles without a C toolchain.
The engine is ordinary Go code, so pprof and the race detector see inside it.

### Passing values

Plugin functions take Go maps, slices, structs and named types, or JSON.
moejs converts a map one level at a time, as far as the plugin reads it, and
JSON text in a `json.RawMessage` the same way. Results come back as Go values
or JSON, or go straight into your own struct with the same result as
`json.Unmarshal`. A `json.RawMessage` field receives the JSON text of its part
of the result, and `DecodeOptions` caps the size of a result before anything
is decoded. Host functions are plain Go functions, and one can return a
promise and settle it later from Go.

### What a plugin can reach

A plugin sees the JavaScript standard library and the globals you install.
Every `import` and `import()` goes through a Go function you provide, and
`eval` and `new Function` can be capped in length or turned off. Builtins are
frozen and shared by every runtime, so every plugin sees the same
`Array.prototype`. Each runtime has its own globals and time zone, and a host
can give a runtime its own mutable copy of the builtins for plugins that
patch them.

### TypeScript

`CompileTS` runs a TypeScript module the way Node strips types: the parser
drops the type syntax, so errors and stack traces point into the TypeScript
source. Enums, namespaces with values and other TypeScript that changes what
the code does at run time fail with a `*SyntaxError`.

### Timers and the event loop

A bare runtime has no timers. The `eventloop` package adds `setTimeout`,
`setInterval` and `setImmediate`, and an event loop on which host functions
settle promises from other goroutines.

### Web APIs

The `webapi` package installs `fetch`, `Headers`, `Request`, `Response`,
the Streams, `AbortController`, `URL` and `performance` on a runtime with an
event loop, for SDKs written against fetch. The host decides which network
fetch reaches through its `http.Client` and an allow hook: without a client
there is none, and without a hook every address the client reaches, so
give untrusted scripts a host list. Response bodies stream: Go reads at
most one chunk ahead of what JavaScript took, each byte is charged to the
runtime's memory limit, and cancel and abort reach the Go request. See
the [guide](docs/guide.md#web-apis).

<p align="center">
  <img src="docs/assets/webapi-bench.en.png" alt="moejs Web APIs against Node.js 24: fetch of small JSON requests at 1 and 8 concurrent, a 256 MiB streamed body and server-sent events">
</p>

The chart runs the same fetch script in moejs and in Node.js 24 against a
local loopback server, measured on 2026-10-25 on 8 vCPUs of an Intel Xeon
E5-2667 v4. moejs is faster on small requests and on a streamed body, and
server-sent events are the row where it is slower.

### Node.js file system

The `node` package gives a runtime `node:fs`, `node:fs/promises` and
`node:buffer`, for programs that read and write the files of a sandbox.
Every path resolves inside one host directory through `os.Root` (and
`openat2` with `RESOLVE_BENEATH` on Linux), so neither `..` nor a link
leaves it. The file system is read-only unless the host allows writes, an
allow hook sees each access, and what a program reads counts against its
memory limit. Errors carry Node's codes and messages, which the tests
compare with Node.js 24 case by case. See the
[guide](docs/guide.md#nodejs-modules).

### Timeouts and errors

Any goroutine can interrupt a running plugin, even one stuck in an endless
loop. A JavaScript throw, a syntax error, an interrupt and a panic in a host
function each come back as their own Go error type, and a thrown `Error` has
a V8-style stack trace. A runtime stays usable after a host function panics.

### Memory limit

`Options.MemoryLimit` caps what one request's JavaScript allocates. A plugin
that goes past it stops with an error that carries its JavaScript stack, and
the script cannot catch it. `Stats` reports a runtime's allocations and other
counters.

## Quick start

```sh
go get github.com/MoeForge/moejs
```

moejs needs Go 1.25 or later and depends only on the standard library (tests
use testify). Releases up to v0.1.0-alpha.10 have the module path
`github.com/Calcium-Ion/moejs`, and later ones `github.com/MoeForge/moejs`, so
upgrading from them changes the import path once.

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/MoeForge/moejs"
)

const source = `
export function buildRequest(task) {
  return {
    method: "POST",
    url: "https://api.example.com/v1/tasks",
    headers: { authorization: "Bearer " + utils.env("API_KEY") },
    body: { prompt: task.prompt.trim(), n: task.n ?? 1 },
  };
}
export function spin() { for (;;) {} }
`

// Request is what buildRequest returns.
type Request struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    struct {
		Prompt string `json:"prompt"`
		N      int    `json:"n"`
	} `json:"body"`
}

// Plugin is a compiled plugin and a pool of runtimes that have loaded it.
type Plugin struct {
	mod  *moejs.Module
	idle chan *moejs.Runtime
}

func NewPlugin(name, source string, size int) (*Plugin, error) {
	// Compile once. Every runtime in the pool loads the same Module.
	mod, err := moejs.Compile(name, source)
	if err != nil {
		return nil, err
	}
	return &Plugin{mod: mod, idle: make(chan *moejs.Runtime, size)}, nil
}

// Call runs hook with body, a JSON text such as a request body, on a runtime
// from the pool and decodes the result into out. Any number of goroutines can
// call it at once.
func (p *Plugin) Call(ctx context.Context, hook moejs.Hook, body string, out any) error {
	rt, err := p.get()
	if err != nil {
		return err
	}
	defer p.put(rt)

	// Interrupt stops the hook when ctx ends. Any goroutine can call it.
	interrupted := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		rt.Interrupt(context.Cause(ctx))
		close(interrupted)
	})
	defer func() {
		if !stop() {
			<-interrupted // Interrupt must return before rt goes back to the pool.
		}
	}()

	// ParseJSONString parses the text without copying it: strings in the
	// arguments share body's memory.
	arg, err := rt.ParseJSONString(body)
	if err != nil {
		return err
	}
	res, err := rt.Call(hook, arg)
	if err != nil {
		return err
	}
	// Unmarshal writes the result straight into out, with no JSON text in between.
	return rt.Unmarshal(res, out)
}

// get takes an idle runtime or makes a new one: host functions first, then the module.
func (p *Plugin) get() (*moejs.Runtime, error) {
	select {
	case rt := <-p.idle:
		return rt, nil
	default:
	}
	rt := moejs.NewRuntime(moejs.Options{})
	if err := rt.SetGlobal("utils", map[string]any{"env": moejs.NativeFunc(env)}); err != nil {
		return nil, err
	}
	if err := rt.Load(p.mod); err != nil {
		return nil, err
	}
	return rt, nil
}

// put resets the runtime and returns it to the pool.
func (p *Plugin) put(rt *moejs.Runtime) {
	rt.ClearInterrupt()
	rt.ReleaseCallData() // The idle runtime lets go of this call's arguments.
	select {
	case p.idle <- rt:
	default: // The pool is full.
	}
}

var secrets = map[string]string{"API_KEY": "test-key"}

// env is the host function behind utils.env.
func env(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
	name, err := r.ToString(moejs.Arg(args, 0))
	if err != nil {
		return moejs.Undefined(), err
	}
	v, ok := secrets[name.GoString()]
	if !ok {
		// A Go error becomes a JavaScript Error with this message.
		return moejs.Undefined(), fmt.Errorf("%s is not set", name.GoString())
	}
	return moejs.String(v), nil
}

func main() {
	p, err := NewPlugin("plugin.js", source, runtime.GOMAXPROCS(0))
	if err != nil {
		panic(err)
	}
	// Look hooks up once and reuse them on every call.
	build, err := p.mod.Hook("buildRequest")
	if err != nil {
		panic(err)
	}
	spin, err := p.mod.Hook("spin")
	if err != nil {
		panic(err)
	}

	// Concurrent calls each get their own runtime.
	reqs := make([]Request, 3)
	var wg sync.WaitGroup
	for i := range reqs {
		wg.Go(func() {
			body := fmt.Sprintf(`{"prompt": " cat %d ", "n": %d}`, i, i+1)
			if err := p.Call(context.Background(), build, body, &reqs[i]); err != nil {
				panic(err)
			}
		})
	}
	wg.Wait()
	for _, req := range reqs {
		fmt.Println(req.Method, req.URL, req.Headers["authorization"], req.Body.Prompt, req.Body.N)
	}
	// POST https://api.example.com/v1/tasks Bearer test-key cat 0 1
	// POST https://api.example.com/v1/tasks Bearer test-key cat 1 2
	// POST https://api.example.com/v1/tasks Bearer test-key cat 2 3

	// A JavaScript throw comes back as *moejs.Exception.
	err = p.Call(context.Background(), build, "{}", &Request{})
	var exc *moejs.Exception
	fmt.Println(errors.As(err, &exc), exc.Name(), exc.Message())
	// true TypeError Cannot read properties of undefined (reading 'trim')

	// A hook still running when ctx ends stops with *moejs.InterruptedError.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = p.Call(ctx, spin, "null", nil)
	var interrupted *moejs.InterruptedError
	fmt.Println(errors.As(err, &interrupted), interrupted.Value)
	// true context deadline exceeded
}
```

`Call` takes its arguments as JSON text because that is how a request
usually brings them, and `ParseJSONString` hands the text to the plugin
without copying it. This is the "moejs (no copy)" path in the chart above. An
HTTP handler can read the body into a `strings.Builder`, growing it to
`r.ContentLength` first when that is known, and pass `b.String()`, which
shares the builder's buffer. `ParseJSON` takes a `[]byte` and copies it once,
and `FromGo` takes Go maps, slices and structs.

A server calling plugins the way `Plugin` does pays the least per request.
Taking a runtime from the pool is one channel receive, while a new runtime
with the largest plugin loaded takes about 71 µs. The [guide](docs/guide.md)
covers pools, module graphs, TypeScript, value conversion, promises, errors
and the event loop, and the
[package documentation](https://pkg.go.dev/github.com/MoeForge/moejs)
describes every function.

moejs ships a `default.pgo` profile recorded from the plugin workload. Go
applies a profile automatically only from the main package's directory, so
pass it when you build:

```sh
go build -pgo="$(go list -m -f '{{.Dir}}' github.com/MoeForge/moejs)/default.pgo" .
```

## JavaScript support

moejs runs ES modules and classic scripts, including sloppy mode and the
web-compatibility behaviour of Annex B. The language includes classes with
fields, private members and static blocks, destructuring, optional chaining,
generators, async functions and async iteration. The standard library has
`Proxy`, `Reflect`, `BigInt`, typed arrays, resizable `ArrayBuffer`s,
`WeakRef`, `structuredClone`, `TextEncoder`/`TextDecoder`, and recent
additions such as the `Set` methods, `Promise.try`, `Float16Array`,
`Array.fromAsync`, `Math.sumPrecise` and `Error.isError`. Regular
expressions support every flag and Unicode 17 properties, and `Date` takes
its time zones from Go.

On [test262](https://github.com/tc39/test262), 80,971 tests pass and 0
fail. The other 14,268 tests use features moejs does not implement and are
skipped. Results by directory are in
[bench/test262/RESULTS.md](bench/test262/RESULTS.md).

Not implemented: import attributes and JSON modules, `using` declarations
and `DisposableStack`, decorators, iterator helpers, `Intl.Collator`,
`Intl.RelativeTimeFormat`, `Intl.ListFormat`, `Intl.DisplayNames`,
`Intl.DurationFormat`, `Temporal`, `ShadowRealm`, `FinalizationRegistry`
and `JSON.rawJSON`. A bare `Runtime`
has no timers; the `eventloop` package installs them. [TODO.md](TODO.md)
lists all of these, the known wrong results and the limits.

## Status

moejs is in alpha, and the API may change between releases.

## Testing

```sh
# Test inputs are downloaded separately: new-api's plugins, a pinned
# test262 revision and pinned TypeScript sources (pi-mono, TypeScript,
# TypeBox). Tests that need them skip until they are downloaded.
bench/testdata/plugins/fetch.sh
bench/test262/fetch.sh
bench/tscorpus/fetch.sh

# Unit, audit and fuzz-corpus tests of the engine.
go test ./...

# Differential tests against Sobek and esbuild, the expression corpus, the
# benchmarks and test262. bench/ is a separate Go module, so Sobek and the
# cgo engines are dependencies of bench/ only. Its V8 and QuickJS baselines
# need cgo.
cd bench && go test -timeout 30m ./...
```

## Acknowledgements

moejs takes design ideas from these projects. Its code was written
independently, except for the parts [THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES)
lists: Intl's date, time zone and number formatting is ported from
[ICU](https://icu.unicode.org/), and the `Date.parse` parser, the stack
trace format and the callee text of "is not a function" errors from V8.
Intl's locale data is [Unicode CLDR](https://cldr.unicode.org/).

- [goja](https://github.com/dop251/goja) and [Sobek](https://github.com/grafana/sobek):
  the Go interop conventions and the `Export` rules. Sobek is also the
  reference for the differential tests.
- [QuickJS](https://bellard.org/quickjs/): the 16-byte value layout, atoms,
  shape transitions and compact builtin tables.
- [V8](https://v8.dev/): hidden classes, inline caches, prototype validity
  checks (reduced to one counter), the Ignition register interpreter,
  `Date.parse` and the `Error.prototype.stack` format.
- [Lua 5.x](https://www.lua.org/): the fixed-width register instruction
  encoding.
- [JavaScriptCore](https://webkit.org/) and [SpiderMonkey](https://spidermonkey.dev/):
  NaN-boxing.
- [TypeScript](https://github.com/microsoft/TypeScript): the grammar and
  the disambiguation rules that `CompileTS` follows.
- [esbuild](https://github.com/evanw/esbuild): how to write a fast
  JavaScript parser in Go.
- [Hardened JavaScript / SES](https://github.com/endojs/endo/tree/master/packages/ses):
  the `lockdown()` model behind the shared frozen builtins.
- [modernc.org/quickjs](https://pkg.go.dev/modernc.org/quickjs): the pure-Go
  QuickJS transpilation baseline of the benchmarks.
- [quickjs-go](https://github.com/buke/quickjs-go) and [v8go](https://github.com/rogchap/v8go):
  the cgo baselines of the benchmarks.
- [test262](https://github.com/tc39/test262): the conformance suite.
- [new-api](https://github.com/QuantumNous/new-api): the plugin host. Its
  `pkg/jsplugin` decides which APIs moejs has to support.

## License

moejs is licensed under the [Apache License 2.0](LICENSE). It contains Unicode
CLDR and Unicode Character Database data and code ported from ICU and V8,
which are under their own licenses.
[THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES) lists them with their full texts. A program that embeds moejs ships those notices too.

The benchmarks run new-api's task plugins, which are licensed under AGPL-3.0
and downloaded separately by
[`bench/testdata/plugins/fetch.sh`](bench/testdata/plugins/README.md).
