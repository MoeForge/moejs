# moejs guide

The [package documentation](https://pkg.go.dev/github.com/MoeForge/moejs)
describes every function, and the [README](../README.md) has a complete
example.

[简体中文](guide.zh_CN.md)

## Runtimes and pools

### Modules and runtimes

A `Module` is immutable, and any number of runtimes can load it at the same
time. A host compiles each plugin once and keeps a pool of runtimes for it.

One goroutine at a time uses a `Runtime`. Other goroutines can call only
`Interrupt` and `ClearInterrupt`.

### Returning a runtime to its pool

When a request is done with a runtime, the host calls `rt.ReleaseCallData()`
and then returns the runtime to its pool. The idle runtime then lets go of
the request's arguments. A request is done after its last `Call`, `ToGo`,
`Get` or `AppendJSON`, because each of them can run JavaScript.

`Call` leaves this step to the host. A request that runs several hooks then
converts its data once, and a host without a pool skips the step.

Values already returned stay valid. Data the module stored stays alive along
with everything it references, so an object the module kept from an argument
can keep the whole argument alive. A `json.RawMessage` is copied when it
is converted, so the host can reuse its buffer for the next request. A Go
map or slice that the module stores before reading it still refers to the
host's values, `json.RawMessage` bytes inside it included, and those must
not change while the module keeps it.

### Memory

Until `ReleaseCallData`, a runtime keeps each request's input and output.
The input is the copy of the text that `ParseJSON` makes, or for `FromGo`
the host's own Go values, which it refers to rather than copies. The
exceptions are a `json.RawMessage`, whose text `FromGo` copies, and the
types it converts from their `json.Marshal` text. The live heap therefore
grows with the number of requests in flight times the size of one request,
on top of what the host itself keeps of each request. `ParseJSONString`,
and `FromGo` of the host's maps, slices and strings, add no copy of the
input's ASCII strings that have no escapes, such as base64 data. A Go string
that is not ASCII, or is at least 64 KiB long, keeps its UTF-8 bytes until
JavaScript first reads its content, and a JSON string that is not ASCII or
has escapes keeps its JSON text the same way. It is then decoded, to UTF-16
when it is not ASCII, and keeps those bytes as well; its length is known
without decoding it. Under a memory limit, such a string that a parse
makes, or that `FromGo` makes for a value in a Go map or slice, is charged
as if decoded when it is made. A Go string that `FromGo` or `moejs.String`
converts on its own is charged nothing.

A runtime also keeps a finished call's values reachable from the engine's
scratch, the interpreter's registers and frame records, until a later call
overwrites them. What a call made and then dropped can therefore stay in
Go's heap after the call returns, where the memory limit's measurements do
not see it, because JavaScript can no longer reach it. A host that pools
runtimes calls `ReleaseCallData` between requests, which drops that scratch
along with the request's data. The `eventloop` package's `Run` drops the
scratch when it ends, so a runtime reused for one loop program after
another does not keep the last program's values.

Under the default `GOGC=100`, a Go process's resident memory runs at
roughly twice its live heap, because the collector lets the heap grow by
as much as is live before it collects again. It runs higher with many
goroutines, and when long collection cycles overlap with contention for
the CPU.

To cap memory, set `GOMEMLIMIT` well above the expected peak live heap,
since the limit counts all of the Go runtime's memory and not the heap
alone, and keep `GOGC` at its default. Throughput is then unaffected until
the process nears the limit. A `GOMEMLIMIT` below the live heap makes the
collector run continuously, and throughput drops sharply. Lowering `GOGC`
trades throughput for memory across the whole process.

A promise an async function call returned keeps only its result once the
call has returned or thrown, and never the call's frame: while the call
waits at an `await`, the promise it awaits keeps the frame, its locals and
the scope it captured, so a call left waiting on a promise that will never
settle and that nothing else holds (`await new Promise(() => {})`) is freed
even while its own promise is held. A promise `then` returned keeps its
handlers until the promise it came from settles. To free them, drop such
promises or settle what they wait on.

As an example, take an 8.1 MB JSON body with five base64 images, 32
requests in flight with `GOMAXPROCS=8`, and a host that keeps each body
until its request is done (Intel i5-13500H, Linux, go1.26.6, medians of 3
runs of 10 seconds). With `ParseJSON` the live heap was about 500 MB and
the resident memory about 1.1 GB. With `ParseJSONString` they were about
330 MB and 740 MB, and the process served 1.5 times as many requests per
second. Still with `ParseJSONString`, `GOMEMLIMIT=768MiB` left the requests
per second as they were, `GOMEMLIMIT=256MiB`, below the live heap, cut them
by 38%, and `GOGC=25` cut them by 9% and the resident memory to about
410 MB.

### Interrupts

`Interrupt(v)` stops running code, and any goroutine can call it. The pending
call returns an `*InterruptedError` that carries `v`. A script stops at the
next loop back-edge or call, and long-running builtins check for the
interrupt while they work.

An interrupt that arrives while nothing runs stops the next call, so the
host calls `ClearInterrupt` before it reuses the runtime.

A runtime that an event loop runs (`eventloop`, below) is interrupted
through `Loop.Interrupt`. `Runtime.Interrupt` alone stops the JavaScript
that is running, but does not wake a loop that sleeps until its next timer.

`RunContext(ctx, fn)` gives a sequence of runtime operations one cancellation
scope. `fn` runs on the calling goroutine with exclusive use of the runtime;
it can include `Load`, `Call` and result decoding. An already canceled context
or pending interrupt prevents entry. Cancellation returns an `*InterruptedError`
carrying `context.Cause(ctx)`; an error from `fn` takes precedence.

Before returning or propagating a panic, `RunContext` stops its watcher or
waits for it to finish. The host can then call `ClearInterrupt` and
`ReleaseCallData` before reuse. It does not roll back changes or retry a failed
`Load`. Blocking Go code must also honor `ctx`; host-side compilation uses
`CompileOptions.Context`. Event loops still require `Loop.Interrupt`.

### Memory limit

`Options.MemoryLimit` caps what the runtime's JavaScript keeps plus what it
allocates. The runtime counts what it allocates since the last reset, and
`ReleaseCallData` resets the count, and so does `ResetAllocation`, for a host
without a pool. When the count passes the limit, the runtime first measures
what the program keeps (below) and counts from that measurement: a program
that makes garbage and keeps little runs on, however much it allocates. A
call that still passes the limit stops as an interrupt does: it returns an `*InterruptedError` whose value is
a `*MemoryLimitError` carrying the limit, the bytes counted and the
JavaScript stack at the allocation, and `errors.Is(err, moejs.ErrMemoryLimit)`
holds.

```go
rt := moejs.NewRuntime(moejs.Options{MemoryLimit: 64 << 20})
res, err := rt.Call(hook, arg)
if errors.Is(err, moejs.ErrMemoryLimit) {
	var mle *moejs.MemoryLimitError
	errors.As(err, &mle)
	log.Printf("plugin used %d bytes, limit %d\n%s", mle.Allocated, mle.Limit, mle.Stack)
}
rt.ClearInterrupt()
rt.ReleaseCallData() // a new budget for the next request
pool.Put(rt)
```

A script cannot catch the hit. The engine allocates without failing, so the
only way to stop a script that allocates is an interrupt, and an interrupt
runs no `catch` or `finally` and drops the queued promise jobs. A script that
caught it could go on allocating. The builtins that know a result's size
before they allocate it throw the `RangeError` they throw for a size past
the engine's limits for a size past what is left of the budget instead, and
a script can catch that: `repeat`, `padStart` and `padEnd`, `new
ArrayBuffer`, typed array constructors, `btoa`, `escape`, `toBase64` and
`toHex`.

Between measurements the count is of allocation: nothing a request frees is
taken off until the runtime measures. It covers objects,
the storage their properties and elements grow into, strings, `Map` and
`Set` tables, buffers, `BigInt` results, closures' environments and code
compiled by `eval` and `Function`, each by about what it asks the Go
allocator for. A rope, the result of joining two
long strings, counts its node, and its units once it is read and flattened.
`FromGo` counts the wrappers it makes for the host's maps and slices but not
the host's Go values, which it refers to without copying; `ParseJSON`
counts its copy of the text. A few internal caches of bounded size are not
counted. The interpreter's register stack and frame table count as they
grow, so a deep recursion with many locals stops at the limit;
`ReleaseCallData` trims them. Expect the count to run within a small factor of what the request
allocates, and set the limit with room.

Setup does not count against the first request: `Load` and `SetGlobal`
are setup, and each ends by resetting the count when it succeeds (for
`Load`, also when it returns `ErrModulePending`). A failed `Load` leaves
the count and the hit's error in `Stats` for the host to read. Called from
a host function, `SetGlobal` does not reset, which would give the running
script a new budget. A host that sets a global for each request sets it
before the request's first `Call`: between two hooks of one request, the
reset would let the request allocate the limit again. A reset keeps the last
measurement, so what the module keeps from request to request still counts;
a host that keeps a runtime across requests can rebase it instead of
resetting it (below).

After a hit the runtime is reusable: `ClearInterrupt` and `ReleaseCallData`
leave it as a pooled runtime after any request. A host that would rather not
trust the module's state, which the hit may have left half-updated, drops
the runtime. Clearing the interrupt without a reset leaves the budget spent,
and the next allocation stops the runtime again.

The hit is the runtime's single pending interrupt, so a host's own
`Interrupt` (a timeout) from another goroutine can replace it, and the call
then returns the host's error. `Stats().MemoryLimitHits` counts the hit
either way, and `Stats().LastMemoryLimitError` keeps its error until the
next reset or rebase.

With no limit the runtime keeps no count, and each place that would charge
an allocation costs two loads and two branches.

A reset bounds what one request allocates. A program that lives through
many tasks, such as an agent that awaits tool results or a chain of timers,
can keep growing while each task stays small. `RebaseAllocation` bounds what
such a program keeps. Called between tasks where a host would reset, it
measures what the runtime's JavaScript retains and makes that the start of
the budget: the limit then counts what the program keeps plus what it
allocated since the measurement. `Stats().LiveBytes` is the last
measurement.

```go
// Between two tasks of the host's own loop.
err := rt.RebaseAllocation(func(add func(moejs.Value)) {
	for _, p := range pending { // promises only Go code holds
		add(p)
	}
})
if err != nil {
	return err // the program keeps more than the limit
}
```

The measurement walks what the runtime's global object, global bindings,
module and queued promise jobs reach, and counts it as the limit counts
allocation. A string that shares the bytes of a longer one of 64
characters or more, a `slice`, a `split` piece or a regular expression's
match, counts the longer one once, since it keeps it alive; so does a value
`JSON.parse` or `ParseJSONString` reads out of a long text while the limit
is set. A property name longer
than 64 bytes that only data supplies (a computed key, a `JSON.parse` key)
stays in the runtime's own shapes, which the measurement counts with the
shapes the runtime keeps for its objects, and dies with the runtime;
shorter ones may go to the shapes every runtime of the process shares,
which are bounded and not counted. String constants of compiled code,
which every runtime running it shares, are not counted either.

It does not see values that only Go code holds: what a `NativeFunc`
closure captured, or a promise of `NewPromise`, which only its settle
functions keep, along with the async functions awaiting it. The host
passes those through the function it gives `RebaseAllocation`, which is
called only when a measurement is made, or registers a function that adds
them to every measurement:

```go
// pending holds the promises of NewPromise this host settles later.
remove := rt.AddLiveRoots(func(add func(moejs.Value)) {
	mu.Lock() // a lock only the host's own goroutines hold
	defer mu.Unlock()
	for _, p := range pending {
		add(p)
	}
})
defer remove()
```

`AddLiveRoots` costs nothing without a limit: nothing measures then but
`Realm().MeasureLive`. The function runs on the goroutine using the runtime,
possibly in the middle of JavaScript (below), so it only calls `add`: it
runs no JavaScript, creates no values and does not wait for a lock held
while the runtime runs JavaScript.

A host function that calls back into JavaScript keeps no JavaScript value
only in Go across that call, or registers it, since a measurement may run
in the JavaScript it calls back. A host function, or a getter that is one,
that builds a large value from Go returns it past the limit: no measurement
sees it while it is being built, and the next measurement, at the latest
the next rebase, finds the program past the limit.

A registered function counts what it adds until it is removed, whether
JavaScript still reaches it or not, so it suits what Go alone holds:
requests in flight, promises only the host will settle. What the Go state of
an object the host creates keeps, such as a stream's queue, a body's bytes
or header values kept as Go strings, reports through the object instead:
when its payload (`Object.SetInternal`) implements `moejs.LiveReporter`, a
measurement that reaches the object calls it once, adds the values it
reports as the object's own and counts the bytes of Go memory it reports.
That state counts while the object is reachable and stops counting at the
next measurement once JavaScript drops it, without waiting for Go's
collector.

```go
type queueState struct {
	chunks []moejs.Value // JavaScript values the stream keeps
	buf    []byte        // Go bytes it keeps
}

func (q *queueState) ReportLive(add func(moejs.Value), addBytes func(n int)) {
	for _, c := range q.chunks {
		add(c)
	}
	addBytes(cap(q.buf))
}

o := rt.Realm().NewObject()
o.SetInternal(&queueState{})
```

`ReportLive` follows the rules of the functions `AddLiveRoots` registers: it
only calls `add` and `addBytes`.

A measurement walks the heap, so it is paced. `RebaseAllocation` measures
only once the allocation since the last measurement has reached the larger
of half of what the limit leaves above it, `(MemoryLimit - LiveBytes) / 2`,
and half of the last measurement, and otherwise returns at once without
allocating. A program that keeps little is measured once every half limit
it allocates; one that nears the limit is measured more often, but not more
than once every half of what it keeps. An allocation that passes the limit
before the next measurement, with or without a measurement before, measures
first, with the module's bindings and the registered roots, and stops only
when the program still keeps too much. It does so once between two calls of
`RebaseAllocation` or `ResetAllocation`, and again each time the allocation
since the last measurement reaches what paces `RebaseAllocation`, so that one
call that makes much garbage runs on as it would across rebases.

The measurement runs where nothing can hold what it made out of the walk's
sight: in plain JavaScript, and in the callbacks of `forEach`, `some`,
`every`, the `find` methods, `map`, `filter`, `flatMap`, `reduce`, `sort`,
`replace` and `replaceAll`, `Array.from`, a `JSON.parse` reviver, a
generator's `next` and a host function, which keep what they build where
the walk finds it. While any other builtin runs, the measurement waits: such
code may hold what it made in Go variables, such as the tree `JSON.parse`
builds or the text `JSON.stringify` writes. Its allocation may then go
past the limit by up to half of what the limit leaves above the last
measurement, plus the one allocation that crosses that (an array's storage
as it grows), and the runtime measures once the builtin returns or at the
next call, or between two promise jobs; a builtin that goes past that
margin stops, so `JSON.parse` of a text too large for the limit stops
within it: the parser charges its own stacks as they grow, where an
array's or object's entries wait until it closes, so an array of millions
of numbers stops as one of objects does. A loop of builtin calls is
measured at each iteration once one passed the limit. A run that ends
with an allocation past the limit (the script's last statement, a host
function that ignored its charge's error) fails itself, after a
measurement where one was pending: its overage never stops the next run
unannounced, which starts clean once the host clears the interrupt and
resets the budget, as after any hit.
`ParseJSON`, `ParseJSONString` and `FromGo` wait in the same way. An
allocation of the host's own Go code outside any call, and its
`ChargeMemory` where no script runs (a loop's task, or a host function
called from Go), measure at once, as `RebaseAllocation` does: what the host
holds between calls, it registers with `AddLiveRoots`. A walk's working
storage goes back to the process for the next measurement of any runtime: a
runtime keeps none of it. Near the limit a program then pays a walk of what it
keeps for every few megabytes it allocates: a callback that allocates 64 KiB
of garbage costs about 1.3 times as much at 75 % of a 64 MiB limit kept in
small objects, 2.5 times at 95 % and 3.7 times at 98 %. When a measurement
passes the limit, `RebaseAllocation` returns the `*MemoryLimitError`, with
`Live` the measurement and `Allocated` 0, and counts a hit; it does not
interrupt the runtime, whose next allocation passes the limit anyway. An
`Interrupt` made while it measures ends the walk, which then changes
nothing, and while an interrupt is pending it does not measure.

`ResetAllocation`, `ReleaseCallData`, `Load` and `SetGlobal` keep the
measurement: what the program keeps still counts after them. A host that
resets per request and keeps the runtime calls `RebaseAllocation` where
each request starts too, and the allocation of all the requests since the
last measurement paces the next one.

Under `eventloop` the loop does this itself. The budget spans the whole
`Run`, or `Start` until `Stop`, and the loop rebases it before each timer
or immediate callback and each task; it registers its pending timers,
immediates and promises of `NewPromise` with `AddLiveRoots`, and `Terminate`
removes them, so that a runtime that gets a new loop does not keep the old
one. A program that
keeps what it allocates stops near the limit however many callbacks or runs
that takes, and one that keeps little runs on however much it allocates in
all. The loop measures without holding its mutex, so goroutines posting
tasks or settling promises do not wait for the walk, and without a limit it
does no work for it. The hit stops the loop as an interrupt does: `Run`
returns an `*InterruptedError`, and `errors.Is(err, moejs.ErrMemoryLimit)`
holds. With `eventloop.Options.ResetMemoryPerTask`, the loop gives each
callback and task a new budget instead, as earlier versions did: the limit
then bounds a macrotask and the promise jobs it queues, not what the program
keeps.

### Counters

`rt.Stats()` returns the runtime's counters, each read in constant time. A
host reads them on the goroutine that uses the runtime, between requests or
from a host function during one:

- `AllocatedBytes`, `Objects`, `Strings`, `Shapes`: what was counted since
  the limit was set, and `RequestAllocatedBytes` since the last reset or
  rebase. They count only while a limit is set; a limit of `math.MaxInt64`
  counts without limiting.
- `LiveBytes`: the last measurement of `RebaseAllocation`, which resets
  keep, 0 while none was made.
- `MemoryLimitHits` and `LastMemoryLimitError`, above.
- `Interrupts`: the calls of `Interrupt`, hits included.
- `ICEntries`: the inline cache entries of the functions that ran.
- `PendingJobs`: the queued promise jobs and microtasks.
- `RegisterStackBytes`: the size of the interpreter's register stack.

A pool can export them when it takes a runtime back:

```go
st := rt.Stats()
metrics.Observe("plugin_request_bytes", st.RequestAllocatedBytes)
rt.ReleaseCallData()
```

A host function can give them to the module:

```go
rt.SetGlobal("memoryUsage", moejs.NativeFunc(func(r *moejs.Realm, this moejs.Value, args []moejs.Value) (moejs.Value, error) {
	return moejs.Int(r.Stats().RequestAllocatedBytes), nil
}))
```

A host function that allocates for JavaScript on its own can count that
too, with `Realm.ChargeMemory(n)`, which returns the interrupt's error once
the limit is passed.

### Objects belong to one runtime

Objects that a runtime's JavaScript creates belong to that runtime. Using an
object of another runtime can run that runtime's code, so pass only Go values
and JSON between runtimes.

`Call`, `CallFunction`, `SetGlobal`, `FromGo` and the settle functions of
`NewPromise` return `ErrForeign` for a function or generator of another
runtime, or for a bound function or proxy of one. Inside the Go containers that `FromGo` converts,
reading such a function throws a `TypeError` with `ErrForeign`'s message.

The check covers the value passed and the members of Go containers. It skips
the properties of JavaScript objects, and it skips a proxy of another runtime
that cannot be called, even inside a Go map. Reading that proxy runs its
traps in the runtime that reads it.

## Code

### Hooks

`Module.Hook(export, members...)` resolves an exported function, or a
function below an exported object, once:
`mod.Hook("protocols", "openai", "decodeRequest")`. The binding stays live.
Each `Call` reads the export in the current runtime and walks the members as
own properties.

A path that leads nowhere gives `ErrHookNotFound`, and a path that ends at a
value other than a function gives `ErrNotCallable`. `Has` returns false for
both.

### Function values

`CallFunction(fn, this, args...)` calls a function value the host got
earlier, such as a callback that a plugin passed to a host function. It
checks what `Call` checks. A value that is not a function gives
`ErrNotCallable`, a function of another runtime gives `ErrForeign`, and an
interrupt stops the call before a host function runs. The jobs the call
queues run before it returns. Called from inside a host function, it leaves
them to the end of the outermost call.

### Module graphs

The host links a module that imports other modules before runtimes load it.
`moejs.Link(entry, resolve)` asks the host's `Resolver` for the module each
specifier names, once per module and specifier, and links the whole graph
once. Every module in the graph comes from the resolver.

`Link` returns the `Module` to load. Like any module, it is immutable and can
be shared. Its `Hook`, `Export` and `Exports` refer to the entry module's
exports, including re-exported names.

The resolver's `referrer` is the importing `*Module`, typed as `Referrer`.
`Referrer` is a sealed interface for the code that requests a module, and
its value is a `*Module` or a `*Script`.

Each runtime instantiates and evaluates the linked graph, each module once.
A module that several graphs share is compiled once if the resolver returns
the same `*Module` for it.

A failed resolution is a `*ResolveError`. An import that does not resolve to
exactly one binding is a `*SyntaxError`. Both carry the position in the
importing module. `Load` returns an error for a module that has imports and
was not linked. A runtime loads a module without imports directly, with no
`Link` step.

```go
entry, err := moejs.Compile("plugin.js", source)
mod, err := moejs.Link(entry, func(referrer moejs.Referrer, specifier string) (*moejs.Module, error) {
	return host.module(specifier) // compiled once per process
})
err = rt.Load(mod) // per runtime
```

### Dynamic import

`Options.Importer` is the host's side of `import()` and `import.meta`. Its
`Resolve` is a `Resolver`, and the `referrer` it receives is the importing
`*Module` or the `*Script` that `RunScript` ran. Its optional `Meta` fills a
module's `import.meta`, which is a null-prototype object created on first
use.

`import(specifier)` calls `Resolve` right away and returns a promise. The
promise resolves to the module's namespace, or rejects with the error from
resolving, linking or evaluating. Without an `Importer`, it rejects with a
`TypeError`.

A runtime evaluates each module once, whether it was imported statically or
dynamically. The graph that `import()` loads is linked once per `Importer`.
Any number of runtimes can share one `Importer`, and each of them only
instantiates and evaluates the graph. Jobs and interrupts behave as they do
for `Load` and `Call`. Only code that uses `import()` or `import.meta` pays
for them.

```go
imp := &moejs.Importer{Resolve: func(referrer moejs.Referrer, specifier string) (*moejs.Module, error) {
	return host.module(specifier)
}}
rt := moejs.NewRuntime(moejs.Options{Importer: imp}) // one imp for every runtime
```

### TypeScript

`CompileTS(name, source)` compiles a TypeScript module. The parser drops the
type syntax as it reads it, as Node's type stripping does, and the module
runs as the JavaScript that is left. Nothing is type-checked.

The parser erases:

- type annotations, type parameters and type arguments, `as`, `satisfies`,
  `x!` and `<T>x`;
- `interface` and `type` declarations, everything after `declare`, function
  overload signatures, and namespaces that hold only types;
- `import type`, `export type` and specifiers marked `type`;
- a `this` parameter, which does not count in the function's `length`;
- the TypeScript modifiers of class members (`public`, `private`,
  `protected`, `readonly`, `override`, `abstract`, `declare`), index
  signatures, and methods without a body. A `declare` or `abstract` field
  produces nothing. A field with only a type stays a field, initialized to
  `undefined`, as tsc emits it for ES2022 and later.

TypeScript that has run-time semantics fails with a `*SyntaxError` that
names it: `enum` and `const enum` outside `declare`, a namespace with
values, constructor parameter properties (`constructor(private x: T)`),
`import x = require("m")`, `export import x = A.B`, an alias
`import x = A.B` used as a value, `export =`, `export as namespace`, and
`accessor` fields. Decorators are not
supported, as in JavaScript. TSX is not supported.

As tsc does, the compiler drops an import specifier whose binding no
expression reads, and an import declaration left without bindings.
`import "m"` stays. A reference in a type, `typeof x` included, does not
count as a read. `export { T }` and `export default T` are dropped when `T`
names only a type. So a module may import types and values from one
package by name, and the host's module for that package needs to export
only the values.

Errors and stack traces refer to the TypeScript text, so no source map is
needed. `Function.prototype.toString` returns the function's TypeScript
source, types included; Node returns it with the types blanked out.

Compiling takes time linear in the length of the source, however deep the
nesting, and nesting deeper than 10,000 levels is a `*SyntaxError`
("Nesting too deep (the parser allows 10000 levels)"). A host that
compiles source it does not trust should still cap its length, and can
bound the compile in time: `CompileWith`, `CompileScriptWith` and the
`Context` of `FunctionOptions` take a `context.Context`, and once it is
done the compile stops and returns its error. The parser, the scope pass and
the compiler look at it every 1,024 statements of a statement list and
every 1,024 items of the other lists (elements of array and object
literals, arguments, parameters, operators of a chain of binary ones,
members of unions, intersections, interfaces, object and tuple types), so
one long statement stops as soon.
`Runtime.Interrupt` does not stop a compile the host calls itself.

```go
ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
defer cancel()
mod, err := moejs.CompileWith("plugin.ts", src, moejs.CompileOptions{TypeScript: true, Context: ctx})
if errors.Is(err, context.DeadlineExceeded) {
	// the compile took too long
}
```

A `Resolver` or an `Importer` picks the compiler per module, for example by
file extension. Modules from `CompileTS` and from `Compile` link with each
other.

```go
resolve := func(referrer moejs.Referrer, specifier string) (*moejs.Module, error) {
	src, err := host.read(specifier)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(specifier, ".ts") {
		return moejs.CompileTS(specifier, src)
	}
	return moejs.Compile(specifier, src)
}
```

### Scripts

`CompileScript(name, source)` compiles a classic script into an immutable
`*Script`. The script is sloppy unless it starts with a `"use strict"`
directive. `Runtime.RunScript` runs it in the runtime's global environment
and returns its completion value.

The script's `var` and function declarations become properties of the global
object. Its `let`, `const` and `class` declarations become global bindings
that later scripts and loaded modules can see. A declaration that conflicts
with an existing global binding throws before any code runs.

`SetGlobal` writes to the global object. After a script declares `let x`,
the global lexical binding `x` shadows a value set with `SetGlobal("x", …)`
(ECMA-262 9.1.1.4.1).

### Functions from source

`CompileFunction(name, params, body, opts)` compiles a function from the
source of its parameter list and its body, as the `Function` constructor
does, and `CompileFunctionTS` does the same for TypeScript:

```go
s, err := moejs.CompileFunctionTS("rule.ts", "input: Input, limit?: number",
	"type N = number;\nreturn input.messages.slice(0, (limit ?? 10) as N);",
	moejs.FunctionOptions{})
fn, err := rt.RunScript(s) // the function
res, err := rt.CallFunction(fn, moejs.Undefined(), arg)
```

`params` is the list without its parentheses, and the source is the text
`function anonymous(params\n) {\nbody\n}`, so a `*SyntaxError` in the body
is on line 3 or later. Neither part may end the other early. The script
declares nothing: `RunScript` creates the function in the global
environment and returns it, and each runtime that runs the script gets a
function of its own. `FunctionOptions` asks for an async function, a
generator function, or both.

`CompileFunction`'s function is sloppy unless its body starts with
`"use strict"`, as the `Function` constructor's is. `CompileFunctionTS`
follows `CompileTS`: types are erased, TypeScript with run-time semantics
is a `*SyntaxError`, `toString` returns the TypeScript, and the function is
strict mode code, as module code is.

### Eval

Importing the `moejs` package installs the compiler that `eval`, the
`Function` constructors and `Realm.EvalScript(name, source)` use. A native
function can call `EvalScript` on the `*Realm` it receives to run a classic
script from a string, like test262's `$262.evalScript`.

A direct `eval` sees the caller's bindings, including a module's imports.
Stack traces show the evaluating script or module as the source of the
evaluated code and of the functions it creates. `import()` in that code also
passes the evaluating script or module to the `Resolver` as `referrer`. The
`referrer` is nil for code from an indirect `eval` or a `Function`
constructor when the calling script or module uses none of `import()`,
`import.meta` and direct `eval` (see [TODO.md](../TODO.md)).

Only code that uses `eval` or `with` pays for them.

### Limits on dynamic code

`Options.MaxDynamicSource` caps the length of the source text that `eval`,
the `Function` constructors and `Realm.EvalScript` compile. The default is
1 MiB of UTF-8, and a negative value removes the cap. Longer text throws a
`RangeError` before it is parsed. Compile time and memory grow linearly with
the text, so the cap bounds both. An interrupt can stop a compile in
progress.

`Options.DisableDynamicCode` turns dynamic code off for a runtime. `eval`,
the `Function` constructors and `EvalScript` then throw an `EvalError`.

`Compile` and `CompileScript`, which the host calls itself, ignore both
options.

### Size limits

The bytecode of one function, or of a script's or module's top level,
holds at most 256 registers, 65,536 constants (names, strings, and numbers
outside -32,768 to 32,767), 65,536 property and global variable accesses
and 65,536 nested functions, and none of its jumps spans more than 32,767
instructions. Code past a limit does not compile: `Compile`,
`CompileScript` and the others return a `*SyntaxError` that names the
limit, and `eval` and the `Function` constructors throw one. A script with
100,000 `var` declarations runs, since the names a script declares are
listed apart from its constants, but 100,000 `var x = …` declarations do
not: each assignment names its global. Code that large compiles when it is
split into functions, scripts or modules, each with limits of its own.

`RunScript`, `Load` and `Link` return an error for a nil `*Script` or
`*Module`, which is what a failed compile returns.

## Values

### Go values in

`FromGo` converts these Go types: `nil`, booleans, numbers, strings,
`json.Number`, `*big.Int` (to a BigInt), `Value`, `NativeFunc`, and the
JSON-shaped containers `map[string]any`, `[]any`, `map[string]string`,
`[]string`, `map[string][]string` and `[]map[string]any`.

`FromGo` converts containers lazily, one level at a time, when JavaScript
first reads them, so a hook pays only for the parts of its arguments it
reads. A JavaScript write changes the JavaScript object and leaves the Go
value as it is. The host must leave the value unchanged while JavaScript can
still read it.

Objects converted from Go maps enumerate their keys in sorted order.

A `[]byte` becomes an `ArrayBuffer` over the same bytes, so JavaScript writes
to it change the Go slice.

A named type whose underlying type is in that list converts as that type:
`type Settings map[string]any` becomes a lazy map, and `type Status string`
a string.

Other types that `encoding/json` writes, such as structs, pointers,
`map[string]int` and `[]map[string]string`, convert from their
`json.Marshal` text, which the engine parses once. Struct tags and
`MarshalJSON` methods apply as they do in `json.Marshal`, and a `[]byte`
field becomes a base64 string. The result is a snapshot taken when `FromGo`
runs, or, for a value inside a Go map or slice, when JavaScript first reads
that container. Later changes to the struct are not seen.

Channels, functions other than `NativeFunc`, complex numbers and values that
`json.Marshal` rejects return an error. So do an `error` and a struct none
of whose fields `json.Marshal` writes, such as a `sync.Mutex` or a
`context.Context`, unless the type has a `MarshalJSON` or `MarshalText`
method: `json.Marshal` would write them as `{}` or as a text that says
nothing about them. Pass `err.Error()` for an error. A struct with no fields
at all is `{}`. Inside a container, reading such a member throws a
`TypeError`.

`SetGlobal(name, v)` converts `v` the same way, so a host can install a
namespace of functions as a `map[string]any` of `NativeFunc` values.
`Function(name, length, fn)` wraps a `NativeFunc` and sets the `name` and
`length` that JavaScript sees.

### JSON in

`ParseJSON(b)` runs `JSON.parse` on the bytes. Use it for arguments the host
holds as JSON, such as stored task data or a request body. It copies the
bytes into a string first, so the host can reuse them once it returns.

`ParseJSONString(s)` parses a string without that copy when it is valid
UTF-8, and strings in the result can share the memory of `s`. A host that
holds the text as a `[]byte` can pass
`unsafe.String(unsafe.SliceData(b), len(b))` if it never modifies the bytes
before `ReleaseCallData`, nor while it or the module keeps values derived
from them. Nothing checks this; the host is responsible for it.

`FromGo` also takes a `json.RawMessage`, alone or as a member of a
`map[string]any` or `[]any`, and JavaScript gets `JSON.parse` of its text.
It is converted from a copy of the text, made when `FromGo` runs or, for a
member, when JavaScript first reads its container. After that the host may
reuse the bytes. The text of an object or an array is checked when it is
converted and parsed when JavaScript first reads the value, so a hook that
never reads it never pays for the parse. Other text is parsed at once.
Invalid text is a `SyntaxError`: `FromGo` returns it for a top-level value,
and reading the member throws it inside a container. A nil
`json.RawMessage` is `null`. A `json.RawMessage` that JavaScript has not
read comes back from `ToGo` as a new `json.RawMessage` with the text.

Typed containers of `json.RawMessage`, such as `[]json.RawMessage`,
`map[string]json.RawMessage`, a `*json.RawMessage` or a struct field, go
through `json.Marshal` like any other type, and are parsed at once.

`ParseJSON` and `ParseJSONString` are the cheaper choice for a text the hook
always reads: they parse the text once, and `ParseJSON` copies it once. A
`json.RawMessage` copies and checks the text, and parses it only when the
hook reads it, so it pays off for a text the hook may not read.

### Go values out

`ToGo` converts:

- integral numbers to `int64`, and other numbers to `float64`
- arrays to `[]any`
- objects to a `map[string]any` of their own enumerable properties
- a `Date` to `time.Time`
- a BigInt to `*big.Int`
- an `ArrayBuffer`, typed array or `DataView` to a `[]byte` copy of the
  bytes it holds or views

An argument that `FromGo` converted comes back as the original Go value as
long as JavaScript has not modified it.

`Get(v, key)` reads one property and runs getters. `Export(name)` reads the
current value of an export of the loaded module.

### JSON out

`AppendJSON` runs `JSON.stringify` and appends the text to a byte slice. Its
output differs from `json.Marshal` of `ToGo`'s result: it omits members
whose value is `undefined`, writes NaN and ±Infinity as `null`, keeps
insertion order and calls `toJSON`.

`AppendJSON` reserves the last output's length plus an eighth and writes
into the slice's spare capacity. A host that passes its previous output back
(`buf, err = rt.AppendJSON(buf[:0], v)`) allocates in two cases: the slice
has less room than that, or the value contains something that can run code.
Those are a `toJSON` method (including `Date`'s), a getter, a proxy, a
BigInt, and an object or array whose prototype is something other than
`Object.prototype` or `Array.prototype`, such as a class instance, a `Map`
or `Object.create(null)`. For those values the output goes to a separate
buffer that grows as it is written, and is appended to the slice at the end.

The output is limited to 2^30−24 bytes.

### Strict JSON out

`AppendJSON` follows `JSON.stringify`, which drops some values without a
word: an object leaves out a member whose value is `undefined`, a function
or a symbol, an array writes such an element as `null`, and NaN and
±Infinity become `null`. A host that would rather fail than send changed
data asks for the strict mode:

```go
out, err := rt.AppendJSONWith(buf[:0], v, moejs.JSONOptions{Strict: true})
var lossy *moejs.LossyJSONError
if errors.As(err, &lossy) {
	// lossy.Path is "$.messages[3].content", lossy.Value "undefined"
}
```

Strict fails on each of those values, on the value itself when it has no
JSON text (`AppendJSON` writes `null` for it), and on what a `toJSON`
returns, so a `toJSON` that returns `undefined` fails too. It also fails on
an object whose data JSON does not see, which `JSON.stringify` writes as
`{}` or with its own enumerable properties only: a `Map`, `Set`, `WeakMap`,
`WeakSet`, `WeakRef`, `Promise`, `RegExp`, an `Error` of any kind (classes
that extend it included), an `ArrayBuffer`, `SharedArrayBuffer` or
`DataView`. Its `Value` is "a Map", "an Error" and so on, `Object` is true,
and the message reads `moejs: $.cache is a Map, whose contents JSON does
not write`. Such an object with a `toJSON` method of its own is written as
that method returns, and typed arrays, whose elements JSON writes, pass. The error's
`Path` starts at `$` and adds `.name` or `["name"]` for a property and
`[i]` for an element; its message reads
`moejs: $.messages[3].content is undefined, which JSON leaves out`. On an
error the slice comes back as it was. -0 is written as `0`, which equals
it, as without Strict. Strict costs `AppendJSON` nothing; with it, a Go
map or slice that `FromGo` converted is written through the values
JavaScript sees rather than straight from Go.

### Into the host's types

`Unmarshal(v, target)` stores a value into a Go value with the same result
as `json.Unmarshal` of `AppendJSON`'s text. Plain objects, arrays and
arguments returned unmodified go in directly, without producing text or
copying strings. Other values, such as a `toJSON`, a getter or a target type
that unmarshals itself, go through the text.

When `AppendJSON` would fail (a BigInt, a cycle, a throwing `toJSON`, a Go
value it cannot write, text past the length limit), `target` keeps its old
contents. When `json.Unmarshal` returns an error, `target` holds what it
wrote before the error.

`ToGoInto(v, target)` gives the result and the error of `ToGo`, then
`json.Marshal`, then `json.Unmarshal` into `target`. A host that already
runs those three steps can switch to it and keep the same behavior. As with
`Unmarshal`, plain objects, arrays and unmodified arguments go in directly,
without building `ToGo`'s Go map or writing text. A getter, a proxy, a
`Date`, a `Map`, a typed array, a BigInt, a function or a target type that
unmarshals itself makes it run the three steps. When `ToGo` or
`json.Marshal` would fail (a throwing getter, NaN or ±Infinity, a cycle),
`target` keeps its old contents.

Where `ToGo` and `AppendJSON` disagree, `ToGoInto` follows `ToGo`. A member
whose value is `undefined` stays, as `null`. A -0 stays -0. NaN and
±Infinity are an error. `toJSON` is not called. As with `ToGo`, an
interrupt stops it only while a getter runs.

A `json.RawMessage` in the target receives the JSON text of its part of the
value, while the rest of the target is still filled directly. It can be a
field, a pointer, a map value, a slice element or the target itself. The
bytes are the ones the round trip gives it: `AppendJSON`'s text for
`Unmarshal`, and for `ToGoInto` the `json.Marshal` text of `ToGo`'s result,
with keys sorted and `<`, `>` and `&` escaped. Like `json.Unmarshal`, it
writes into the `json.RawMessage`'s own array when the text fits. For
`ToGoInto`, a `json.RawMessage` argument that the hook returns unread is not
parsed: the target receives its text as `json.Marshal` compacts it. Any
other type that unmarshals itself still sends the whole value through the
round trip.

### Bounding a result

`UnmarshalWith`, `ToGoIntoWith` and `ToGoWith` take `DecodeOptions` that
limit the JSON text of the result. `MaxBytes` limits its length, and
`MaxNodes` the number of values in it: objects, arrays, strings, numbers,
booleans and null, but not keys. The text is the one the round trip writes,
which is `AppendJSON`'s for `UnmarshalWith` and the `json.Marshal` text of
`ToGo`'s result for the other two. The counts are exact: a result whose text
has `n` bytes passes `MaxBytes: n` and fails `MaxBytes: n-1`. A zero field
sets no limit.

A result past a limit returns an error that wraps `ErrTooLarge`, and the
target keeps its old contents. The value is counted before anything is
decoded, without writing the text, when it can be read without running
JavaScript: plain objects, arrays and arguments from `FromGo`. A
`json.RawMessage` argument the hook returns unread is the one exception for
`UnmarshalWith`: `AppendJSON` writes the text of the value it parses to, so
it is parsed to be counted, unless a quick scan of it already shows a text
past the limit. `ToGoIntoWith` and `ToGoWith` count its text without a
parse. A value that needs JavaScript to be read, such as a `toJSON`, a
getter or a proxy, is counted on the text once the round trip has written
it, or for `ToGoWith` on the result of `ToGo`, but still before anything is
decoded. A cycle, and nesting deeper than 10,000 levels, passes every
limit.

With no limit set, these functions are `Unmarshal`, `ToGoInto` and `ToGo`,
at the same cost.

### Strings kept after the request

Strings that `ToGo`, `Unmarshal` and `ToGoInto` return from a value
`ParseJSON` or `ParseJSONString` produced, or from a `json.RawMessage`
argument, can share memory with the parsed text (for `ParseJSONString`, the
host's string; for a `json.RawMessage`, the runtime's copy of it) and keep
it alive. So can `String()` of a string value. Call `strings.Clone` on the
strings the host keeps.

### Nesting

`ParseJSON`, `ParseJSONString`, `ToGo` and `AppendJSON` return a
`RangeError` when arrays and objects nest deeper than 10,000 levels, and
`JSON.parse` and `JSON.stringify` throw one. Its message says what
happened ("JSON nesting is too deep (more than 10000 levels)"), and
`errors.Is(err, moejs.ErrTooDeep)` reports it.

## Promises and errors

### Promises

`NewPromise` gives a host function a promise to return, along with Go
functions that settle it later. `PromiseResult` reads a promise's state and
result. `SetPromiseRejectionTracker` reports rejections that no handler
caught, like Sobek's tracker.

`ThrownValue(err)` returns the value that a host function returning `err`
throws. A host rejects a promise with it to give JavaScript the same
`Error`, which unwraps to `err` when it comes back as an `*Exception`.

The settle functions run only on the goroutine that uses the runtime. The
`eventloop` package (below) settles promises from other goroutines.

### Errors

A JavaScript throw comes back as an `*Exception`. `Name()` and `Message()`
read the `name` and `message` data properties of the thrown value. For a
thrown primitive they use the value itself. Neither runs JavaScript.

When a host function returns a Go error, JavaScript sees a thrown `Error`
whose message is the error's text. The resulting `*Exception` unwraps to the
original Go error.

An interrupt is an `*InterruptedError`. It unwraps to the interrupt's value
when that value is an error, so a host that calls
`Interrupt(context.Cause(ctx))` finds `context.DeadlineExceeded`, or its own
cause, with `errors.Is` and `errors.As`. A malformed module or script is a
`*SyntaxError` with its position. A Go panic during a call, for example in a
host function, is an `*InternalError`, and the runtime can still be used
afterwards.

A `Call` or `CallFunction` made inside a host function returns the
`*InternalError` of a panic below it to that host function. If the host
function returns the error, JavaScript sees it thrown as an `Error` that it
can catch, like any other Go error.

`Runtime.StackTrace(exc)` returns the V8-format `stack` of a thrown `Error`,
also without running JavaScript.

## Event loop and timers

### Running a loop

A bare `Runtime` has no timers. The `eventloop` package adds them, along
with an event loop that owns the runtime while it runs:

```go
loop, err := eventloop.New(rt, eventloop.Options{})
err = loop.Run(func(rt *moejs.Runtime) error {
	_, err := rt.RunScript(script) // the script may call setTimeout
	return err
})
```

`New` installs `setTimeout`, `clearTimeout`, `setInterval`,
`clearInterval`, `setImmediate` and `clearImmediate` as globals. `Run`
calls the function, then runs timers, immediates and posted tasks on the
same goroutine. It returns when nothing keeps the loop alive: no ref'd
timer or immediate, no `Hold` that is not done, no promise of `NewPromise`
that is not settled and no task that has not run.

A module whose top-level `await` waits for a timer makes `Load` return
`ErrModulePending`. The loop runs the timer later and the module finishes
then, so the function passed to `Run` treats that error as success.

`Start` runs the loop on a new goroutine instead. That loop stays alive
while idle, until `Stop` or `Terminate`.

### Phases

Each iteration has three phases, like Node's timers, poll and check phases:

1. The timers due when the phase starts, in order of expiry, then of
   scheduling.
2. The tasks posted before the phase starts: `RunOnLoop`, `Post`, the
   `done` function of `Hold` and the settle function of `NewPromise`.
3. The immediates set before the phase starts.

Work that a phase adds waits for a later iteration. A delay is at least
1 ms, so a timer that a timer callback sets runs in a later iteration. The
promise jobs and `queueMicrotask` callbacks that a callback queues run when
it returns, before the next callback. With nothing to run, the loop sleeps
until the next timer is due or a task arrives.

### Node compatibility

The timers follow Node.js, because the code that needs them is written for
Node:

- `setTimeout` and `setInterval` return a `Timeout` object with `ref`,
  `unref`, `hasRef`, `refresh`, `close` and `[Symbol.toPrimitive]`, which
  returns a numeric id. `setImmediate` returns an `Immediate` with `ref`,
  `unref` and `hasRef`.
- A callback gets the extra arguments, and the `Timeout` or `Immediate` as
  `this`. A callback that is not a function throws a `TypeError` with the
  code `ERR_INVALID_ARG_TYPE`. A string is not evaluated.
- The delay goes through `ToNumber` and is truncated to whole milliseconds.
  NaN, a delay below 1 and a delay above 2147483647 become 1 ms.
- An interval is rescheduled for its delay after the time its callback
  started. `refresh` restarts a timer's delay from now, and runs a timeout
  that already fired again.
- `clearTimeout` and `clearInterval` take a `Timeout`, or its id as a
  number or a string once the id has been read. `clearImmediate` takes an
  `Immediate`. All three ignore anything else, and work inside the timer's
  own callback.
- An unref'd timer or immediate does not keep `Run` alive. It runs only
  while something else does.

Some details differ from Node. The `Timeout` and `Immediate` objects have
no own properties: `JSON.stringify` gives `{}`, and `constructor.name` is
`"Object"`. Their methods throw a `TypeError` when called on another object,
including a `Proxy` of one, and a `Timeout` has no `Symbol.dispose` method.
Timers expire in the order of their exact due times, where Node compares
whole milliseconds. `timers/promises`, the `AbortSignal` options and
`util.promisify` are not provided.

### Asynchronous host functions

While the loop runs, only its goroutine uses the runtime. Other goroutines
hand it work in three ways:

- `RunOnLoop(fn)` posts `fn`. It returns false once the loop is
  terminated. `eventloop.Post(loop, task)` does the same for a task that
  returns an error. The loop handles that error as a callback's (`OnError`,
  or the loop stops), so a task that settles promises itself returns what
  their jobs throw instead of losing it.
- `Hold()` keeps the loop alive until its `done(fn)` is called, which posts
  `fn`.
- `NewPromise()`, which a host function calls on the loop, returns a
  promise and a `settle` function. `settle(f)` runs `f` on the loop. A
  value fulfills the promise, and a Go error rejects it with the `Error` a
  host function's error throws (`ThrownValue`).

`done` and `settle` can be called from any goroutine. The first call counts,
and later calls do nothing. Take a hold on the loop, in a host function or a
callback. A hold taken on another goroutine races with a `Run` that has
nothing else left, and almost always loses: it then keeps only the next
`Run` alive.

```go
rt.SetGlobal("fetchJSON", moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
	url := moejs.Arg(args, 0).String()
	p, settle := loop.NewPromise()
	go func() {
		body, err := download(url) // blocking work runs off the loop
		settle(func(rt *moejs.Runtime) (moejs.Value, error) {
			if err != nil {
				return moejs.Undefined(), err
			}
			return rt.ParseJSON(body)
		})
	}()
	return p, nil
}))
```

### Errors

These errors stop the loop, and `Run` returns them: a callback's exception,
the first exception of the jobs after it (from a `queueMicrotask` callback,
for example), a Go panic in a host function, and a failure of `settle`. An
uncaught exception ends a Node.js process too. With `Options.OnError` set,
the loop passes these errors to it and keeps running. An error from `Run`'s
own function ends `Run` at once.

An interrupt always stops the loop, before it takes the next timer, task or
immediate off its queues, which keep them. `Run` returns the
`*InterruptedError`. `Loop.Interrupt(v)` interrupts the runtime and wakes
the loop; `Runtime.Interrupt` alone leaves a sleeping loop asleep until its
next timer. The interrupt stays pending, so the host calls `ClearInterrupt`
before it runs the loop again.

A panic in a host task (the functions of `RunOnLoop`, `Post`, `done` and
`settle`, `OnError`, `OnIdle` or `OnWake`) is not recovered. It unwinds
through `Run`, and ends the program for a loop started with `Start`. The
tasks and immediates of the phase that had not run stay queued for the next
`Run`. The task that panicked is gone, and a promise whose settle function
panicked stays pending.

A rejection that no handler catches is the host's business, through
`SetPromiseRejectionTracker`.

### Stopping and reuse

`Stop` ends the loop after the callback it is running and waits for the loop
to end. It returns what ended that run, as `Run` would: `nil`, the error
that stopped it, the `*InterruptedError` of an interrupt or
`ErrTerminated`. That is how a host learns of an error that stopped a loop
started with `Start`. `StopNoWait` does not wait, so a callback on the loop
can call it. `Stop` and `Terminate` must not be called on the loop, which
includes the function passed to `Run`, because they would wait for
themselves.

When `Run` returns or the loop stops, the runtime is the host's again. What
is still pending stays pending, such as an unref'd timer, or everything a
stop, an error or an interrupt left. The next `Run` or `Start` continues it. JavaScript
that the host runs in between, with `Call` for example, can set more timers.

`Terminate` ends the loop for good. It interrupts the runtime with
`ErrTerminated`, which stops the running JavaScript, drops every pending
timer, immediate and task, and waits for the loop to end. Afterwards `Run`
returns `ErrTerminated`, `RunOnLoop` returns false and the timer functions
throw. The interrupt stays pending even when no JavaScript was running, so
the host calls `ClearInterrupt` before it uses the runtime again. Another
`New` gives the runtime a fresh loop.

### Idle and wake

A host that bounds the CPU time of a run, or shares a few execution slots
among many runtimes, needs to know when the loop waits instead of running
JavaScript: for a timer, a task from another goroutine, a `node:fs` result
or a `fetch`. `Options.OnIdle` and `Options.OnWake` tell it:

```go
loop, err := eventloop.New(rt, eventloop.Options{
	// Called when the loop has nothing to run and is about to block; no JavaScript runs after it until OnWake.
	OnIdle: func() { /* pause the watchdog, release the execution slot */ },
	// Called after the loop wakes, before it runs anything; an error stops the loop as an interrupt does.
	OnWake: func() error { /* take an execution slot (may wait), start the watchdog */ return nil },
})
```

The loop calls `OnIdle` right before it blocks, when nothing is to run now
(no task, no immediate, no timer due) and something keeps it alive. It calls
`OnWake` once it woke with something to run, before it runs it. A loop that
never has to wait calls neither. A wake that finds nothing to run, such as
a `Hold` released while another is still held, blocks again without a call.
A wake that ends the loop (`Stop`, `StopNoWait`, `Terminate`, an interrupt,
or for `Run` nothing left that keeps it alive) ends the run without
`OnWake`, and `Run` returns what it would return without the hooks.

So the calls alternate, `OnIdle` first, and a run can end after an `OnIdle`
with no `OnWake`. The host knows which state it is in from the hook it saw
last. The hooks run on the loop, so they are done when `Run` returns, or
`Stop` for a loop started with `Start`. A host with slots takes one before
`Run` and releases it after `Run` unless the last hook was `OnIdle`:

```go
slots <- struct{}{} // take a slot
awake = true        // OnIdle sets it to false, OnWake to true
watchdog.Resume()
err = loop.Run(fn)
if awake {
	watchdog.Pause()
	<-slots
}
```

An error from `OnWake` stops the loop as an interrupt does, before it takes
anything off its queues: `Run` returns the error unchanged, or `Stop` for a
loop started with `Start`, and the timers, immediates and tasks stay queued
for the next run. The run ends idle. The runtime is not interrupted, since
no JavaScript was running.

`OnWake` may block, for example while it waits for a slot. `Stop` and
`Terminate` wait for the loop and so for `OnWake`; a host whose `OnWake`
waits ends that wait when it stops the loop, for example through a context
it cancels before `Stop` or `Terminate`. When `OnWake` returns nil after a
stop, `Terminate` or interrupt came meanwhile, the run ends before the loop
runs anything, and it ends awake. The time `OnWake` takes counts as time
passed for the timers, so a timer that fell due meanwhile runs late.

The hooks run without the loop's lock and may call `RunOnLoop`, `Post`,
`Hold`, `Interrupt` and `StopNoWait`, but not `Stop` or `Terminate`. A panic
in a hook is not recovered, as one in a host task is not. With both hooks
nil the loop behaves as without them.

## Web APIs

### Installing

The `webapi` package installs the Web APIs that fetch-based SDKs need:
`fetch`, `Headers`, `Request`, `Response`, `ReadableStream`,
`WritableStream`, `TransformStream`, `TextDecoderStream`,
`TextEncoderStream`, the queuing strategies, `AbortController`,
`AbortSignal`, `EventTarget`, `Event`, `DOMException`, `URL`,
`URLSearchParams` and `performance`. They run on an event loop, and only
Go's standard library does the I/O. A bare runtime has none of them. The
host installs them explicitly:

```go
rt := moejs.NewRuntime(moejs.Options{})
loop, err := eventloop.New(rt, eventloop.Options{})
api, err := webapi.Install(loop, webapi.Options{
	Client: &http.Client{Timeout: 10 * time.Minute},
	Allow:  webapi.AllowHosts("api.openai.com", "*.anthropic.com"),
})
defer api.Close()
err = loop.Run(func(rt *moejs.Runtime) error {
	_, err := rt.RunScript(script) // the SDK's code calls fetch
	return err
})
```

Each global is created the first time JavaScript reads it, so a runtime
pays only for the names of the ones it never uses. A runtime without the
package is unchanged. The globals are writable and configurable properties
of the global object, as Web IDL defines them. This holds also when the
runtime shares the frozen builtins, so a host can delete or replace any of
them.

`Install` is called once per runtime. `Close` stops every fetch in flight,
closes every response body that was not read to its end and stops the
timers of `AbortSignal.timeout`. It can be called from any goroutine; the
promises it rejects settle the next time the loop runs. `Close` is final,
and a fetch after it rejects: call it when the runtime is dropped.
`loop.Terminate` does not stop the fetches in flight, so call `Close` as
well.

A host that pools runtimes ends one run's network activity with a context
instead:

```go
ctx, cancel := context.WithCancel(requestCtx)
api.SetContext(ctx) // the parent of the fetches of this run
err = loop.Run(...)
cancel() // fails this run's fetches still in flight, closes their bodies
// the runtime goes back to the pool and fetches again in its next run
```

Canceling the context fails the fetches it started that are still in
flight, as a network error does, with the context's cause as the error's
`cause`. It closes their response bodies at once and releases what they
kept the loop alive with. `Options.Context` stays the parent of every
request.

### Permissions and the client

A runtime reaches only the network the host gives it:

- `Options.Client` sends the requests. Without one, every `fetch` rejects
  with a `TypeError` (deny all). The client's `Transport` decides how a
  request travels: a custom `http.RoundTripper` can serve requests from Go
  without a network, record them, or add credentials the script never sees.
- `Options.Allow` is called before each request is sent, and again for
  each redirect. An error rejects the fetch. The hook sees the request as
  it is sent: `req.Host` is the `Host` header when the script set one.
  `webapi.AllowHosts` returns one for a list of hosts: `*.example.com`
  matches the subdomains of example.com but not example.com itself, and
  `*` matches every host. When the script sets a `Host` header, that host
  must be allowed too, as well as the URL's. The hook runs on the
  request's goroutine, so concurrent fetches call it concurrently.
- A nil `Allow` checks nothing. Every address the client can reach is
  allowed, including the intranet, loopback (`127.0.0.1`, `localhost`)
  and the cloud metadata address `169.254.169.254`. Give scripts you do
  not trust a host list.
- `Options.Context` is the parent of every request's context. Canceling
  it fails the fetches in flight, for example when the host shuts down.
  `api.SetContext` adds a parent for the fetches of one run, for a host
  that pools runtimes (see Installing).

The client's `Timeout`, `Jar` and `CheckRedirect` apply. The package copies
the client to follow each request's `redirect` mode: `"follow"` (at most 20
redirects), `"manual"` (the 3xx response itself, as Node.js gives it) and
`"error"`.

### Promises on the event loop

`fetch`, `text()`, `json()`, `arrayBuffer()`, `bytes()` and every read of a
response body return promises that settle on the loop, in a task of its
tasks phase. The promise jobs a task queues run before the next task, as
in a browser. A fetch waiting for its response and a body read waiting for
its chunk keep `Run` alive. A response body that nobody reads does not.
Like Node.js, `AbortSignal.timeout` does not keep the loop alive by itself;
a fetch that uses the signal does.

### Streaming

A response body is a `ReadableStream`. From the stream's first read on, a
goroutine reads the `http.Response` body at most one chunk ahead of what
JavaScript took: it reads a chunk, which waits until the stream takes it,
and reads the next one while JavaScript works on that one, so that the
wait for the network overlaps JavaScript's work. A read takes the chunk
read ahead at once, or waits for the read that runs. A slow reader
therefore still holds the server back through TCP flow control, and
memory stays bounded by the socket buffers and one chunk per body.

```js
const response = await fetch(url, { method: "POST", body, signal });
const reader = response.body.getReader();
for (;;) {
  const { done, value } = await reader.read(); // value is a Uint8Array
  if (done) break;
  onText(decoder.decode(value, { stream: true }));
}
```

Each chunk is a `Uint8Array` over the bytes that one read of the body
returned: what had arrived, up to the read buffer's size. A chunk that
fills at least half of the read buffer is handed over without a copy; a
smaller one is copied into a buffer of its own size on the goroutine, and
the read buffer serves the next read. The read
buffer starts at 16 KiB and grows to 256 KiB while reads fill it; under a
`MemoryLimit` it grows to an eighth of the limit at most, and no less than
4 KiB, so that a script that keeps one chunk at a time stays well under
the limit.
`for await (const chunk of response.body)`, `pipeThrough` and `pipeTo` read
the same way. `text()`, `json()` and the other body methods of a response
whose stream JavaScript has not touched read the whole body in one go on
the Go side. A body they read is used: its stream is locked and disturbed,
and a second read, `clone()` or `text()` is a `TypeError`.

Every byte handed to JavaScript is charged to the runtime's
`MemoryLimit` before it is allocated, on the loop; under `eventloop` the
limit bounds what the run keeps and allocates across its tasks, as the
memory limit section above describes. A streamed chunk is charged in the
task that hands it over. Until then, the chunk read ahead and the read
buffer count in every measurement of what the run keeps while JavaScript
reaches the stream, though only Go holds them. A body read whole is charged as it arrives: a
goroutine reads up to a window past what was charged (the largest read
buffer), or to the body's end, and the loop charges those bytes before
the next window is read. Go holds at most one window more of the body than
was charged, and a body smaller than a window costs one task. The bytes
read so far count in every measurement of what the run keeps, though only
Go holds them, and the body is charged once. A charge the package makes on
the loop is the host's own: when it would pass the limit, it measures at
once, as `RebaseAllocation` does. A charge that fails closes the body, rejects
the read with a `RangeError` that carries the limit's message, and stops
the loop (`errors.Is(err, moejs.ErrMemoryLimit)`), as an allocation of
JavaScript past the limit does. Whatever the budget, a body larger than
the limit can never be taken: a body whose `Content-Length` is larger
than the limit is rejected before a byte is read, and otherwise the
window that reaches the limit reads one byte more, so the read rejects
with a `RangeError` before that window is charged, as a builtin that
knows its result does not fit throws, and Go holds little more of the
body than the limit. With
`eventloop.Options.ResetMemoryPerTask`, whose budget is per task, that is
the bound of a body read whole. An announced `Content-Length` preallocates
at most 1 MiB, and no more than the limit. A gzip or deflate body counts its decoded bytes, so a body that
expands is bounded the same way. A host without a `MemoryLimit` can cap bodies in
its `Transport`, for example with a `RoundTripper` that wraps each
response's `Body` in an `io.LimitReader`. The branches of `tee()` and
`clone()` hold the chunks one branch read and the other did not yet, as
in browsers, and those chunks were charged when they were handed over; the
second branch of a stream of bytes, a response body included, gets copies,
which are charged too.

Much of what the package keeps for JavaScript is Go state the measurements
of the memory limit cannot walk: the queued chunks, source, sink and
promises of a stream, the listeners and reason of an `AbortSignal` or
`EventTarget`, header values, URLs and search params kept as Go strings,
the bytes of a body built from a string or a buffer. Each object's state
reports what it keeps when a measurement reaches the object
(`moejs.LiveReporter`), so it counts while JavaScript can reach the object
and stops counting once it cannot, without waiting for Go's collector, and
a program cannot keep memory out of the limit's sight by parking it there.
The work no object leads to, a fetch in flight with the async functions
awaiting it, a body read whole and the timer of `AbortSignal.timeout`, is
reported through `Runtime.AddLiveRoots` while it runs, with one
registration per `Install` that `Close` ends; without a limit the package
registers nothing. The Go strings are charged as the package keeps them,
and two objects that share them, a `Headers` and its copy, both count
them.

A gzip or deflate body is decoded. Go's transport does it when the request
did not set `Accept-Encoding` itself; the package does it otherwise. Brotli
is not decoded.

### Cancellation and errors

- `reader.cancel()`, `response.body.cancel()` and breaking out of
  `for await` close the Go body and cancel the request's context.
  The body is closed at once, also while a read of it is blocked. A
  custom `RoundTripper`'s response body must therefore allow `Close`
  concurrently with `Read` and make the `Read` return, as net/http's
  bodies and an `io.Pipe` do.
- The request's `AbortSignal` (`AbortController`, `AbortSignal.timeout`,
  `AbortSignal.any`) cancels the context too. Before the response arrives,
  the fetch rejects with the signal's reason. After it, the body stream
  errors with that reason, so a pending read rejects with it. The reason is
  an `AbortError` `DOMException` by default and a `TimeoutError` for
  `AbortSignal.timeout`.
- A network error before the response, a denied request or a redirect
  error rejects with `TypeError: fetch failed`. Its `cause` is an `Error`
  with the Go error's text and, for the common cases, Node's `code`
  (`ECONNREFUSED`, `ECONNRESET`, `ETIMEDOUT`, `ENOTFOUND`).
- An error while the body streams, such as a connection reset, errors the
  stream with `TypeError: terminated`, which pending and later reads
  reject with.
- The end of the body, an error, a cancel, an abort and `Close` each close
  the body and end the request's context at once, without waiting for the
  garbage collector; the goroutine reading the body stops, and a chunk it
  read ahead is dropped. As a last resort, the body of a response that
  JavaScript drops, unread or partly read, is closed when the collector
  frees it. That can
  happen much later, so a host should not rely on it: read or cancel
  bodies, and call `Close`.

### Frozen builtins and polyfills

The package's globals live on the global object, which stays extensible,
so they install alongside the shared frozen builtins. Their prototypes
belong to the runtime and are not frozen.

A polyfill bundle that only adds missing globals works with the frozen
builtins. One that reassigns or patches a builtin does not, because the
global bindings of the builtins are locked. For example, assigning
`TextEncoder`, `TextDecoder`, `structuredClone` or `queueMicrotask`
throws `Cannot assign to read only property` in strict code. Such a
polyfill should check first and keep moejs's own:

```js
if (typeof globalThis.TextEncoder === "undefined") {
  globalThis.TextEncoder = TextEncoderPolyfill;
}
```

- Polyfills that work with frozen builtins: those that only define what is
  missing, such as URL, AbortController, Streams, fetch and Headers
  polyfills of the `if (!globalThis.X)` kind, event-target shims and
  `web-streams-polyfill/ponyfill`. With the `webapi` package installed,
  they find the globals present and do nothing.
- Polyfills that need `Options{MutableIntrinsics: true}`: those that
  replace or extend a builtin, such as a `TextEncoder` installed
  unconditionally, `Promise` or `Symbol` replacements, `core-js` in its
  global (non-pure) mode, and anything that adds methods to
  `Array.prototype`, `Object.prototype` or `String.prototype`. Each runtime
  then gets its own mutable builtins, which costs about 25 times as much to
  create.

With `MutableIntrinsics`, `Install` takes the builtins the package calls
(`Promise` with its `then` and `resolve`, `Uint8Array`, `TextDecoder` and
`String.prototype.normalize`) before a script runs, so a script that
replaces or deletes them later does not reach the package. One that was
missing before `Install` makes what needs it throw a `TypeError`. One hook
remains: `Promise.prototype.then` looks up the species constructor of the
promise it is called on, so a `Promise[Symbol.species]` getter, or a
promise's `constructor`, that a script defines runs inside the package's
own promise reactions (a `pipeTo` makes about a dozen such calls). The
shared frozen builtins have no such hook.

### Limits

- `Blob`, `File` and `FormData` are not provided. A body of one of those
  types is a `TypeError`, and `blob()` and `formData()` reject.
- Only `http:` and `https:` URLs are fetched. `data:`, `blob:` and
  `file:` URLs reject.
- There are no CORS, no cookies beyond the client's `Jar`, no cache and no
  forbidden headers. A request's `Host` header sets the request's host,
  which `Allow` sees and `AllowHosts` checks. `mode`, `credentials`,
  `cache`, `referrer` and `integrity` are kept on the `Request` object but
  have no effect.
- A request method longer than 4 KiB is a `TypeError`; no real method comes
  near it.
- Redirects follow Go's method rules: a 301, 302 or 303 turns any method
  other than `GET` and `HEAD` into `GET`. The Fetch Standard does that for
  `POST` only on 301 and 302. A redirect to another origin (another
  scheme, host or port) drops `Authorization`, `Cookie` and
  `Proxy-Authorization`, as undici does.
- Streams: `type: "bytes"` streams take `ArrayBufferView` chunks and
  have no `byobRequest`. `getReader({ mode: "byob" })` throws a
  `TypeError`. A source's `pull` that returns no promise counts as done at
  once rather than a microtask later.
- `URL` follows the current URL Standard, internationalized domain names
  included, and does not report validation errors that do not fail the
  parse.
- `performance` has `now()`, `timeOrigin` and `toJSON()`, without marks,
  measures or entries.

## Node.js modules

### Installing

The `node` package gives a runtime `node:fs`, `node:fs/promises` and
`node:buffer`, also under their bare names `fs`, `fs/promises` and
`buffer`, for programs that read and write the files of a workspace, such
as the programs a coding agent writes for a sandbox. It runs on an event
loop, and only Go's standard library does the I/O.

```go
imp := node.Importer(hostImporter) // nil when the host has none
rt := moejs.NewRuntime(moejs.Options{Importer: imp, MemoryLimit: 256 << 20})
loop, err := eventloop.New(rt, eventloop.Options{})
api, err := node.InstallLoop(loop, node.Options{
	FS: &node.FSOptions{
		Root:  "/srv/sandboxes/42", // the host directory
		Mount: "/workspace",        // where JavaScript sees it
		Cwd:   "/workspace/app",    // what relative paths resolve against
		Write: false,               // the default: read-only
		Allow: func(op, path string) error {
			if filepath.Base(path) == ".env" {
				return fs.ErrNotExist
			}
			return nil
		},
	},
	Buffer: true, // the global Buffer
})
defer api.Close()
```

`node.Importer` wraps the host's `Importer`: it resolves the package's
specifiers to its modules and passes every other specifier to the host's
`Resolve`, so `import()` of the host's own modules works as before.
`node.Resolver` does the same for a `Resolver` given to `moejs.Link`, for
programs that import the modules statically. The modules are compiled
once per process and read what they export from their `import.meta`,
which `Install` fills for its own runtime: a runtime without `Install`, or
whose options have no `FS`, fails `import("node:fs")` with an `Error`.
`node:buffer` works without `FS`.

`InstallLoop` installs the package on a loop of package `eventloop`;
`Install` takes any `node.Loop` (see below). Either is called once per
runtime, before the loop runs or on it. `Close` closes the root directory
and fails the operations still waiting for a worker with `EBADF`; the
file system functions fail with `EBADF` after it.

### Your own loop

A host that drives its own event loop, rather than package `eventloop`'s,
gives `Install` a `node.Loop`:

```go
type Loop interface {
	Runtime() *moejs.Runtime
	Post(task func(*moejs.Runtime) error) bool
	Hold() (release func())
}
```

- `Post` queues a task, from any goroutine. The loop runs it on the
  runtime's goroutine, between its other callbacks, as it runs a callback,
  and handles its error as a callback's: an interrupt (the memory limit's)
  and an exception (one that a callback of `fs.readFile` threw) end the
  program, as an uncaught exception ends Node.js. A task runs the promise
  jobs it queues before it returns, so the loop runs nothing for it. `Post`
  reports false once the loop is over, and the task is dropped.
- `Hold` keeps the loop from ending while an operation is in flight: a
  loop with nothing else to do waits while a hold is out. It is called on
  the runtime's goroutine; `release` comes from any goroutine, once, after
  the operation's `Post`.

Each promise and callback operation takes one hold when it starts and
posts one task, which settles its promise or calls its callback, when its
work is done. The synchronous functions need neither. A loop that wakes on
a channel implements both in a few lines: `Post` appends to a queue under
a mutex and signals the channel, `Hold` counts, and the loop's wait for
work treats a count above zero as work to come. The package's tests run
it on such a loop.

### Sandbox

Every path resolves inside `FSOptions.Root`:

- A relative path joins `Cwd`. An absolute path must lie under `Mount`
  (by default the root's own host path). A path outside it fails with
  `EACCES`, so does the root's host path when `Mount` differs from it.
- The rest of the path goes to the operating system's resolution,
  confined to the root through `os.Root` (and on Linux on amd64 and arm64,
  `openat2` with `RESOLVE_BENEATH`, which resolves a path in one call).
  `..` and symbolic links are resolved as the system resolves them, after
  the links before them, and one that leaves the root fails with `EACCES`.
  A link inside the root works, also one with an absolute target under
  the root. `realpath`, `glob` and `readlink` return paths under `Mount`.
- Without `Write`, every function that changes the file system fails with
  `EROFS`, as does `access` with `W_OK` and `readFile` with a writing
  `flag`.
- `Allow(op, path)` sees each access before it is made: `op` is `"read"`
  or `"write"`, as in Node.js's permission model, and `path` is absolute
  and under `Mount`. An error that `errors.Is` matches with
  `fs.ErrNotExist` fails the access with `ENOENT`, a `syscall.Errno` with
  that errno, any other error with `EACCES`. See the next section for
  what it is asked and what it can guarantee.
- The package creates no links: `cp` copies what a link leads to.
- Confinement is by path. A hard link that the host made inside the root
  to a file outside it is a path inside the root, and the program reads
  the file through it. Programs cannot make links.
- On Windows, paths take the forms Node.js's `path.win32` reads, with `\`
  and `/` both separating names: a drive (`C:\work`, `c:/work`), a share
  (`\\server\share\work`), a rooted path (`\x`, on the volume of `Cwd`),
  a drive-relative path (`C:x`, relative to `Cwd` on its drive, to the
  drive's root on another) and a relative path. `Mount` and `Cwd` take
  the same forms; a `Mount` without a volume, such as `/workspace`, works
  as on Linux. `.` and `..` are cleaned before the path is checked against
  `Mount`, as Windows does, and the check ignores case. A `\\?\` or `\\.\`
  path is the drive's or share's path it names (`\\?\C:\x`,
  `\\?\UNC\server\share\x`), any other device path fails with `EACCES`.
  The paths the package makes (`realpath`, `readlink`, the names of a
  recursive `readdir`, a `Dirent`'s `parentPath`, `glob`'s matches, what
  `mkdir` with `recursive` returns, the paths the hook gets) have `\` and
  `Mount`'s spelling; those the program gave stay as it gave them. A
  `file:` URL with a host names a share, one without a host needs a drive.
  The error codes are those libuv gives Windows' errors: `rmdir` of a file
  is `ENOENT`, `unlink` of a directory and `rename` onto a directory are
  `EPERM`, and `errno` has libuv's Windows numbers.
- On Linux, a stat, an open or a listing of a name in a directory the
  package resolved within the last millisecond takes one system call: the
  package keeps a descriptor of the directory, opened beneath the root,
  and uses it only with that one name, without following a link (a link
  as the name is followed from the root, beneath it). The package's own
  `rename`, `rm`, `rmdir` and `unlink` drop the descriptors before they
  return, so the program's own changes, which cannot leave the root,
  are seen at once. What the cache adds is a window: for up to a
  millisecond after a directory was resolved, a descriptor follows the
  directory itself, not its path. If another process renames or replaces
  the directory meanwhile, the program still reaches it at its new place,
  outside the root too: another process that moves a directory out of the
  root, writes into it there and moves it back can have the program read
  what it wrote. Without the cache, a path is resolved anew each time,
  and the only window is the one between the resolution of a path and its
  use, which the os.Root path has as well. Set
  `FSOptions.NoDescriptorCache` when other processes may rearrange the
  tree while the program runs; a `stat` below the root then takes three
  system calls rather than one.

### The Allow hook

The hook is asked about the path as the program wrote it, one directory
at a time, and then about each name the resolution of the path reaches,
the names a link leads to included, each before anything about it is
looked up: the directories as `"read"`, the path itself as the
operation's `op`. A denied directory fails the whole operation with the
hook's error, so a program learns nothing about what it holds: not
which names exist in it, not their kinds, not where its links lead. A
link is followed only where the hook allows both the link and its
target. `realpath` asks the same.

The hook also guards what lies below a directory that an operation
changes as a whole. `rm` with `recursive` asks `"write"` about every
entry it would remove and removes nothing when one is denied; `rename`
of a directory asks about each entry below it, at its old path and at its
new one; `mkdir` with `recursive` asks about each directory it creates.

What the hook cannot guarantee:

- It guards paths, not files. A file has the path a rename gives it, and
  a policy written as paths holds for the program's renames only as far
  as the paths it names: a rename of an ancestor moves what a prefix
  policy denies to a path it may allow, unless the policy also denies the
  rename (the hook is asked about every entry the rename moves). Where a
  policy must hold whatever the program renames, give it `Write: false`,
  or a `Root` narrowed to the area it may write.
- It guards names, not listings: `readdir` and `glob` of an allowed
  directory list every name in it, the denied ones too.
- The path is resolved before the access is made. A program that renames
  a link into place between the two, from another operation in flight,
  can race the hook; the root holds either way.
- On a file system that ignores case, as macOS's and Windows's do by
  default, the hook gets the program's spelling of each name, which the
  system resolves whatever its case: compare names case-insensitively
  there.
- The hook is called often, once for each directory of a path and more
  for a recursive `rm` or a `rename`: keep it cheap.

### Errors

The errors are Node.js's: an `Error` whose own properties are `errno`,
`code`, `syscall`, `path` and, for `rename`, `copyFile` and `cp`, `dest`,
with Node's message, such as
`ENOENT: no such file or directory, open 'a.txt'`. `path` is the path as
the program gave it. The syscall names and the messages follow Node.js 24
case by case (`readFile` of a directory is `EISDIR` for `read`, without a
path; `realpathSync` names the component that failed): the package's
tests run the same cases in Node.js and compare. Bad arguments throw
Node's `TypeError` and `RangeError` with `code`s such as
`ERR_INVALID_ARG_TYPE`. A function of Node's `node:fs` that the package
does not implement throws `ERR_METHOD_NOT_IMPLEMENTED`.

### What is there

- Reading: `readFile`, `readdir` (`withFileTypes`, `recursive`, `encoding`),
  `stat` and `lstat` (`Stats` with its fields, the lazy dates and the
  `is*` methods; `throwIfNoEntry`), `access` and `fs.constants`,
  `exists` and `existsSync`, `realpath`, `readlink`, and `glob` with
  `globSync` (`cwd`, `exclude` as patterns or a function, `withFileTypes`,
  `followSymlinks`). Each has the synchronous, the callback and the
  promise form; `fs.promises.glob` returns an async iterator.
- Writing, with `Write`: `writeFile`, `appendFile`, `mkdir` (`recursive`),
  `rm` (`recursive`, `force`), `rmdir`, `unlink`, `rename`, `copyFile` and
  `cp` (`recursive`, `force`, `errorOnExist`).
- `Buffer`, a subclass of `Uint8Array`: `from` (a string in `utf8`, `hex`,
  `base64`, `base64url`, `latin1`/`binary`, `ascii` or `utf16le`/`ucs2`, an
  array, an array-like, an `ArrayBuffer` whose memory it shares, a
  Buffer's JSON), `alloc`, `allocUnsafe`, `concat`, `isBuffer`,
  `isEncoding`, `byteLength`, `compare`, and the methods `toString`,
  `write`, `slice` and `subarray` (both share memory), `equals`, `compare`,
  `indexOf`, `lastIndexOf`, `includes`, `toJSON`, `fill`, `copy`,
  `swap16/32/64`, the reads and writes of 8, 16 and 32-bit integers and of
  floats and doubles in both byte orders, and of 1 to 6-byte integers.
  Decoding follows the WHATWG UTF-8 decoder, as Node.js does.

### Order

`readdir` returns the names sorted by their bytes, as Node.js does on
Unix, where libuv sorts them. A recursive listing follows Node's order:
breadth first for the synchronous and callback forms, and Node's stack
order for the promise form; links to directories are followed, as
Node.js follows them. `glob` returns its matches sorted; Node.js
returns them in the order of its walk, which it does not document.
`fs.promises.glob` returns an async iterator, as Node's async generator
does, and a bad pattern or option rejects its `next()` rather than throw.
The patterns are minimatch's, as Node's: `*`, `?`, classes, `**`, braces and
extglobs, without dotfiles unless a pattern names the dot, and `**`
follows no link unless `followSymlinks` is set; a backslash separates
names, as in Node.js, rather than escape a character. A segment without
extglobs is matched in time linear in the name; one with them is
compiled to a Go regular expression, as minimatch compiles a RegExp, which
matches in linear time and bounded memory. `!(...)`, which a regular
expression of RE2 cannot express, is matched by trying the splits of the
name, with a bound on the work and the runtime's interrupt checked: a
segment may hold one, outside any other extglob, and a name that takes
too long fails the walk. A pattern is at most 64 KiB, as minimatch's,
and so are all the distinct strings of one call's patterns and `exclude`
patterns together, each copied and compiled once however many times an
array repeats it, and at most 65536 of them. The braces of a pattern
expand to its first 4096 patterns, and those of one call's patterns to
at most 1 MiB in all: the expansion takes time in proportion to what it
produces, stops as soon as it passes 1 MiB, nests braces at most 1024
deep, and checks the runtime's interrupt as it goes. A segment is parsed
in time linear in its length, a run of literal characters as one token,
with extglobs nested at most 256 deep and at most 65536 tokens a call.
The expressions of a segment and of the whole call are bounded in size. What the expansion and the expressions take is
charged to the memory limit, and the runtime's interrupt is checked
between the expressions. The work of matching, the bytes of the
names times the size of the expressions run over them, is counted: the
runtime's interrupt and `Close` are checked as it grows, and a call whose
work passes its bound fails with a `TypeError`. A pattern or a call past
these bounds is a `TypeError`.

### Event loop and limits

The synchronous functions run on the loop, as in Node.js. The promise and
callback forms do their file system work on a pool of
`FSOptions.Workers` goroutines (4 by default, the size of libuv's pool);
the operations beyond wait in a queue, not on goroutines of their own.
They settle on the loop in a task of its tasks phase, and an operation in
flight keeps `Run` alive. An exception in a callback stops the loop as a
timer callback's does.

What the package hands to JavaScript, file contents, names and paths, is
charged to the memory limit (`moejs.Options.MemoryLimit`) on the loop,
before it is created. A file larger than the limit fails before it is
read, with a `RangeError` whose code is `ERR_FS_FILE_TOO_LARGE`, as Node
fails one larger than 2 GiB; a program that keeps more than the limit of
what it reads stops with the limit's error. The workers hold at most the
limit at once of what they read and list for JavaScript: a worker takes
a file's size from that budget before it reads the file. A listing,
recursive or a glob's, is charged (or taken from the budget) as it
grows, not once it is done. A result whose work is done holds its part
until the loop settles it. An operation whose growth does not fit waits,
as backpressure, for what will be released without it: the results the
loop is to settle, and the operations that began before it. So none waits
on itself and no two wait on each other; the oldest operation goes over
the budget rather than wait on younger ones, and it alone does, once the
older ones and the results have released what they held, so the workers
hold at most the limit and that one operation's growth. A wait ends at
the runtime's interrupt and at `Close`. An operation that alone needs more than the
limit, such as a listing larger than it, fails as that operation with a
`RangeError` the program can catch, in either form, rather than stop the
program. The budget is released on every path: when the loop settles a
result, when the work fails, when an interrupt stopped the work (the
result is dropped and its promise stays pending, as an interrupt leaves
promises), when the loop is terminated and refuses the settlement, and
at `Close`. The operations of an interrupted program that have not
started on a worker when the interrupt comes are dropped too, rather than
run for a program the host stopped, so a runtime that the host clears
and runs again does not wait for them.

The long walks, a recursive `readdir`, `glob`, `cp`, a recursive `rm` and
the hook's walks, and the long loops of `Buffer`, stop at the runtime's
interrupt (`Interrupt`, `Terminate`, the memory limit) and at `Close`,
in the synchronous forms as in the others.

### Deviations

- No streams (`createReadStream` and friends), no file handles
  (`fs.promises.open`, `fs.open`), no `watch`, no `bigint` stats. TODO.md
  lists them.
- `birthtimeMs` is `ctimeMs` on Linux, as libuv gives it without `statx`.
- `cp` has no `filter`, and it copies a link's target rather than the
  link.
- A Buffer has an `ArrayBuffer` of its own: there is no pool, so
  `byteOffset` is 0 but for a slice, and `allocUnsafe` returns zeroed
  memory. `buffer.kMaxLength` is moejs's largest `ArrayBuffer`, 2 GiB - 1.
- `fs.constants` holds Linux's values.
- On Windows and macOS, `glob` matches case as written, where Node.js
  ignores it.
- On Windows, `mkdir` with `recursive` returns `\\?\C:\...` as Node.js
  does, but for a `Mount` without a volume, where it returns the plain
  path.
- Base64 runs at about a gigabyte a second; Node.js, with SIMD, is about
  four times faster on large inputs.

## Isolation

### Shared frozen builtins

By default all runtimes share one deeply frozen set of builtins, so every
plugin sees the same builtins. This is the Hardened JavaScript model (SES
`lockdown()`). Writing to
`Array.prototype` throws a `TypeError` in strict code and fails silently in
sloppy code, as it does for any frozen object.

The global bindings of the builtins are locked too. `Promise = MyPromise` is
ignored in sloppy code and throws in strict code, and
`delete globalThis.Array` returns false. A plugin that wants its own
`Promise` declares a binding with that name in its module.

A write to a shared builtin fails before any setter runs. This also applies
to the legacy `RegExp.input = v` and `RegExp.$_ = v`. `RegExp.$1` and the
other legacy static properties read the runtime's own last match.

`Options{MutableIntrinsics: true}` gives each runtime its own mutable copy of
the builtins, for hosts whose plugins patch them. Creating such a runtime
costs about 25 times as much.

### Time zone per runtime

`Options.TimeZone` sets the local time zone of `Date`, which is also the
default time zone of `Intl.DateTimeFormat`. nil means `time.Local`.
`Options.Locale` sets the default locale of `Intl` ("Default locale"
below).

## Intl

### What is there

`Intl.getCanonicalLocales`, `Intl.supportedValuesOf`, `Intl.Locale` with its
locale information methods (`getWeekInfo` and the like), `Intl.Segmenter`,
`Intl.NumberFormat`, `Intl.DateTimeFormat` and `Intl.PluralRules`, and the
`toLocaleString` methods of numbers, BigInts, dates and arrays, which format
with them. `Intl.Collator`, `Intl.RelativeTimeFormat`, `Intl.ListFormat`,
`Intl.DisplayNames` and `Intl.DurationFormat` are not there yet;
[TODO.md](../TODO.md) lists them. `String.prototype.localeCompare` uses the
CLDR root collation order whatever the locale.

### Locales

The default locale is `en-US` on every host, whatever the environment of
the process, unless the host sets another one with `Options.Locale`, so a
program gives the same results wherever it runs. Language tags are
canonicalized as ECMA-402 requires, with the alias data of CLDR 48, and
`Intl.Locale`'s `maximize()` and `minimize()` use CLDR 48's likely subtags.
The available locales are CLDR's, with the language-region form of each
language-script-region locale (`zh-TW` along with `zh-Hant-TW`). The
`"best fit"` locale matcher matches as `"lookup"` does.

`Intl.NumberFormat` and `Intl.DateTimeFormat` carry the CLDR 48 data of
eleven locales, `en`, `zh-Hans`, `zh-Hant`, `ja`, `ko`, `de`, `fr`, `es`,
`pt`, `ru` and `it`, and of the regional locales of these languages, each
inheriting by CLDR's parent locales as in ICU: `en-GB` and `en-AU` from
`en-001`, `es-MX` from `es-419`, `pt-AO` from `pt-PT`. Their available
locales are the CLDR locales of these languages; `resolvedOptions().locale`
is the locale matched, as in Node.js, and it formats with its own data:
`en-GB` writes 11 October 2026 as `11/10/2026`, `en-IN` groups
`12,34,567`, `de-CH` writes `1'234.5`. `zh-HK` is `zh-Hant-HK` and `zh-SG`
`zh-Hans-SG`, as in ICU. A locale of another language falls back to the
default locale: `sv-SE` formats as `en-US`. `Intl.PluralRules` has the
rules of every CLDR language.

Digits follow the numbering system (`-u-nu-arab`, the `numberingSystem`
option) when it has a simple digit mapping, as the systems of
`Intl.supportedValuesOf("numberingSystem")` do; any other is ignored, as
ECMA-402 allows. `Intl.DateTimeFormat` has the Gregorian calendar only:
another calendar (`-u-ca-japanese`, the `calendar` option) is ignored and
`resolvedOptions().calendar` is `"gregory"`.

Where Node.js differs, moejs follows ECMA-402 and UTS #35:

- A keyword whose value is `true` loses the value:
  `new Intl.Locale("en-u-ca").calendar` and the `collation` of
  `en-u-co-true` are `""`, where Node.js gives `"true"`.
- An option value is canonicalized with the aliases of its keyword only:
  `{calendar: "yes"}` stays `"yes"` (`en-u-ca-yes`), where Node.js makes it
  `true`.
- `maximize()` replaces the unknown script and region codes: `zh-Zzzz-ZZ`
  becomes `zh-Hans-CN`, which Node.js leaves as it is.
- `Intl.Locale` has the `variants` option and getter, which Node.js 24
  lacks.
- `resolvedOptions().timeZone` is the identifier given, in the database's
  case: `Asia/Kolkata` stays itself, which Node.js reports as
  `Asia/Calcutta`. `Intl.supportedValuesOf("timeZone")` lists the primary
  identifiers, `UTC` and the `Etc/GMT±n` zones included.
- `hour12: true` in Japanese is the 0-11 cycle (`h11`) in every region;
  Node.js uses it for `ja-JP` only.
- `Intl.PluralRules` takes BigInts, which Node.js rejects, and
  `selectRange` of two values that format alike gives the start's
  category.
- `getCalendars()` is `["gregory"]` (the calendars `Intl.DateTimeFormat`
  has), and `getNumberingSystems()` is `["latn"]` for a locale without
  number data, where Node.js gives `arab` for `ar-EG`.

As in Node.js, `format` writes a plain space where `formatToParts` has
U+202F, the narrow no-break space before `AM` and `PM` in English.

### Default locale

`Options.Locale` sets the default locale of a runtime: the locale of the
`Intl` constructors and of the `toLocaleString` methods when the script
names none. It is a BCP 47 language tag such as `"zh-CN"`, canonicalized
when the runtime is created, without its Unicode extension (`-u-` keywords,
which ECMA-402's default locale never has). An empty string or a tag that
is not well-formed means `"en-US"`. `Realm.SetLocale` changes it later.

```go
rt := moejs.NewRuntime(moejs.Options{Locale: "zh-CN", TimeZone: shanghai})
// (1234.5).toLocaleString() is "1,234.5"; new Date(0).toLocaleString() is
// "1970/1/1 08:00:00".
```

### Time zones

The default time zone of `Intl.DateTimeFormat` and of `Date`'s
`toLocaleString` methods is the runtime's `Options.TimeZone`. For `nil`,
`time.Local`, its name comes from `TZ`, `/etc/localtime` or
`/etc/timezone`, and is `UTC` when none tells.

The `timeZone` option takes the IANA names in any case (`asia/tokyo`), the
offsets (`+05:30`) and `UTC`. moejs loads named zones with Go's
`time.LoadLocation` and does not embed the time zone database: Go finds the
host's (`/usr/share/zoneinfo`, `$ZONEINFO`). A host on a minimal image
without one (scratch, distroless static) adds Go's copy to its binary with
`import _ "time/tzdata"` (about 450 KB). Without a database, `UTC`, the
`Etc/GMT±n` zones and the offsets keep working, and a named zone throws a
`RangeError` that says the host has none and how to add one.
`Intl.supportedValuesOf("timeZone")` lists the zones that load on the host.
It and `Intl.DateTimeFormat` try a zone that failed to load again after 10
seconds, so a database installed while the program runs is picked up.

### Segmenter

`Intl.Segmenter` splits text into grapheme clusters, words and sentences by
the rules of Unicode Standard Annex #29 for Unicode 17, without locale
tailoring. Grapheme clusters are what users see as one character: an emoji
ZWJ sequence, a flag, a letter with combining marks, an Indic conjunct.

Word segmentation uses no dictionaries. Text written without spaces between
words (Chinese, Japanese, Thai, Lao, Khmer, Myanmar) gets the default rules
of the annex: each ideograph is a word of its own, and a run of Thai letters
splits per character. Browsers and Node.js segment such text into
dictionary words. `isWordLike` is true for a segment of letters, digits,
kana or ideographs, as in Node.js.

### Cost

A runtime is created without `Intl`. The first runtime of the process that
names it builds it, and its data (CLDR aliases, likely subtags and locales,
the Unicode break properties, each locale's number and date formats, the
time zone names) is decoded on first use, once per process; a locale's
tables are decoded when it is first used. `toLocaleString()` without
arguments reuses one formatter per runtime, so it costs only its result.
The number, date and time zone data take about 460 KB of a program's
binary (85 KB of it the regional locales'), and a program that links
moejs grows by about 1 MB with the code of `Intl.NumberFormat`,
`Intl.DateTimeFormat` and `Intl.PluralRules`.

## Implementation

### 16-byte values

A `Value` is 16 bytes: an `unsafe.Pointer` and a `uint64`. Numbers,
booleans, `undefined` and `null` need no allocation. The pointer word always
holds a valid pointer or nil, so Go's garbage collector can scan it safely.

### Shapes and inline caches

Objects share an immutable tree of shape transitions, ordered by property
insertion. Property access instructions carry inline cache slots, and one
counter tells whether prototype chains are still valid. Objects converted
from Go maps share a shape per key set, so plugin code that reads `ctx.xxx`
stays monomorphic across calls.

### Register bytecode VM

The VM runs fixed-width 32-bit register instructions on one contiguous
register stack per runtime. Calls allocate nothing. Exceptions unwind
through handler tables.

### Strings

A string is ASCII (a Go string, used without copying), UTF-16, a rope that
is flattened when needed, or a lazy string that keeps the UTF-8 or JSON
text it came from and decodes it once, when its content is first read.
Builtin property names are static atoms.

### Locks

Property access, calls, strings, host conversion and regular expression
matching run without locks. Runtimes on different goroutines share only the
garbage collector.

### Regular expressions

A regular expression runs on Go's `regexp` (RE2, linear time) when moejs can
translate it exactly, and on a pure-Go backtracking engine otherwise. The
backtracking engine has a bounded stack and checks for an interrupt every
4,096 steps.

### Bounded caches

A pooled runtime's caches stay within fixed limits, even when every request
brings objects keyed by data (ids, user values, host map keys). Each intern
cache holds at most 4,096 names. The host shape cache holds at most 1,024
key sets with 8,192 keys in total. When a cache is full, the runtime empties
it, and it refills with what is in use. A shape holds its first 8 transitions
strongly and the rest weakly, so the garbage collector frees the shapes of
data-keyed objects along with the objects.
