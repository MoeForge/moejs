# Not supported yet

moejs implements the parts of ECMAScript that real plugin code uses, and
grows from there. The features below are not implemented. Each one fails with
an error that names it, at compile time where the syntax allows, otherwise
when it is used; none of them runs silently with the wrong result.

## Language

- Import attributes (`with { type: "json" }`, and the options argument of
  `import(specifier, options)`) and JSON modules, `import defer`, and source
  phase imports (`import source`)
- Explicit resource management: `using` and `await using` declarations,
  `DisposableStack`, `AsyncDisposableStack`, `SuppressedError` and
  `Symbol.dispose`
- Decorators

## TypeScript

`CompileTS` erases TypeScript's type syntax and runs what is left (see
docs/guide.md). It does not check types. TypeScript that has run-time
semantics fails with a `SyntaxError` that names it:

- `enum` and `const enum` declarations, outside `declare`
- Namespaces and modules with values (a namespace that holds only types is
  erased, as tsc erases it)
- Constructor parameter properties (`constructor(private x: T)`)
- `import x = require("m")`, `export import x = A.B`, and an alias
  `import x = A.B` used as a value
- `export =` and `export as namespace`
- `accessor` fields (part of the decorators proposal; decorators are listed
  under Language)
- TSX

## Builtins

- Iterator helpers (`Iterator.from`, `Iterator.prototype.map` and friends)
  and the `getOrInsert` upsert methods
- `RegExp.escape`, `Atomics.pause`, `ShadowRealm` and `Temporal`
- JSON source text access: `JSON.rawJSON`, `JSON.isRawJSON` and the third
  (`context`) argument of a `JSON.parse` reviver
- The legacy `caller` and `arguments` properties of sloppy functions:
  reading `f.caller` or `f.arguments` throws a TypeError, as it does for a
  strict function
- `Intl.Collator`, `Intl.RelativeTimeFormat`, `Intl.ListFormat`,
  `Intl.DisplayNames` and `Intl.DurationFormat`
- Locale data beyond eleven languages: `Intl.NumberFormat` and
  `Intl.DateTimeFormat` carry the CLDR data of `en`, `zh-Hans`, `zh-Hant`,
  `ja`, `ko`, `de`, `fr`, `es`, `pt`, `ru` and `it` (one line of the
  generator each) and of their regional locales (`en-GB`, `pt-PT`,
  `zh-Hant-HK`); a locale of another language formats as the default
  locale
- Calendars other than the Gregorian in `Intl.DateTimeFormat`
  (`-u-ca-japanese`, `chinese`, ...), and numbering systems without a
  simple digit mapping (`hans`, `roman`, ...)
- Locale-tailored `localeCompare` and `toLocaleUpperCase`/`toLocaleLowerCase`
  (`localeCompare` uses the CLDR root collation order)
- Dictionary word segmentation in `Intl.Segmenter`: words of text written
  without spaces (Chinese, Japanese, Thai, Lao, Khmer, Myanmar) follow the
  default rules of UAX #29, one segment per ideograph, where ICU uses
  dictionaries. Segmentation has no locale tailoring (`-u-ss`)
- `FinalizationRegistry`
- Timers on a bare `Runtime`, which has none: `setTimeout` is a
  ReferenceError there. The `eventloop` package installs Node.js's
  `setTimeout`, `setInterval`, `setImmediate` and their clear functions,
  without `timers/promises`, the `AbortSignal` options, `util.promisify`
  of the timer functions, the `Symbol.dispose` method of a `Timeout`, and
  Node's warning for a delay above 2147483647 ms
- Web APIs on a bare `Runtime`, which has none: `fetch` is a
  ReferenceError there. The `webapi` package installs `fetch`, `Headers`,
  `Request`, `Response`, the Streams, `AbortController`, `URL` and
  `performance` on a runtime with an event loop, without `Blob`, `File`,
  `FormData`, BYOB readers, fetch of `data:`, `blob:` and `file:` URLs, and
  Brotli decoding
- Node.js modules on a bare `Runtime`, which has none: `import("node:fs")`
  fails there. The `node` package serves `node:fs`, `node:fs/promises` and
  `node:buffer` on a runtime with an event loop, without streams
  (`createReadStream`, `createWriteStream`), file handles and descriptors
  (`fs.promises.open`, `FileHandle`, `fs.open`, `read`, `write`), `watch`
  and `watchFile`, `bigint` stats, `opendir`, `mkdtemp`, `chmod`, `chown`,
  `utimes`, `truncate`, `link` and `symlink`, the `filter` option of `cp`,
  Buffer's BigInt reads and writes, `transcode` and `Blob`, and the other
  `node:` modules

## Known wrong results

These run, with a result that differs from the specification:

- Decimal text to number: a number with more than 800 significant digits
  before its point or exponent, or an exponent of 100000 or more offset by a
  long run of zeros, is misrounded by Go's `strconv`. An exact conversion is
  only needed for text longer than 800 characters, which both cases need.
  Sobek's result is the same.
- In strict code, an undeclared name assigned by destructuring
  (`[x] = iterable`, `({a: x} = o)`), or by a strict function inside the
  scope of a sloppy direct `eval` or of a `with` statement, is resolved
  when the value is stored, not when the target is evaluated: if the
  iterator, a getter or the right-hand side creates the global in between,
  the value is stored instead of throwing a ReferenceError. Elsewhere, a
  plain assignment (`x = (globalThis.x = 1)`) throws.
- `super[k] += v` and the other compound and logical assignments, and
  `super[k]++` and `++super[k]`, convert `k` to a property key before they
  read `super[k]`. In a method whose home object's prototype is null,
  `k`'s `toString` runs before the TypeError, as in V8; the specification
  throws first, when the read converts the null base to an object, as
  moejs does for a plain read or assignment.
- `import()` in the code of an indirect `eval` or of a `Function`
  constructor passes a nil `referrer` to the `Resolver` when the script or
  module calling it uses neither `import()`, `import.meta` nor a direct
  `eval` itself (the specification passes that script or module): only
  such code keeps a record of its script or module, so that other code
  pays nothing. With `engine` alone, a module run by `EvaluateModule`
  without a graph has no record either. The `referrer` is nil too, and the
  code's stack frames are named `<anonymous>`, when a job calls `eval` or a
  constructor directly, with no code of a script or module running
  (`Promise.resolve(s).then(eval)`): that is the specification's default
  host, while a browser passes the script or module that called `then`
  (HostMakeJobCallback).

- `Function.prototype.toString` of a function compiled by `CompileTS`
  returns its TypeScript source, types included. Node replaces the types
  with blanks before it runs the code, so its `toString` returns the text
  with blanks where the types were.
- `CompileTS` decides which imports are types without type information, as
  esbuild and tsc with `isolatedModules` do: `export { X }` of an imported
  `X` keeps the import even when the exporting module declares `X` as a type
  only (write `export { type X }`), and a name that a computed property key
  in a type reads (`{ [k.x]: T }`) does not keep its import, where tsc keeps
  it.

## Limits

- One realm per runtime: JavaScript cannot create another realm, so the
  specification's cross-realm behaviour (a constructor falling back to the
  realm of its `newTarget`, for one) never arises, and a function or
  generator runs only in the runtime that created it (the host API returns
  `ErrForeign` for one of another runtime).
- A promise `then` returned keeps its handlers while the promise it came
  from is pending: the derived promise shares an allocation with the
  reaction that settles it. A promise derived from one that nothing will
  settle and nothing else reaches stays alive with its handlers as long as
  it is held, where the collector would free the handlers otherwise.
- A function that needs more than 256 registers, its parameters (and a
  rest parameter or `arguments` after them) included, is a SyntaxError:
  register operands are 8 bits.
- A function, or a script's or module's top level, that needs more than
  65,536 constants, 65,536 property and global variable accesses or 65,536
  nested functions, or a jump over more than 32,767 instructions, is a
  SyntaxError, and so is a block whose closures capture more than 65,535
  of its bindings: those operands are 16 bits. A script's top level reaches
  them first, since each global it reads or writes is a name constant and
  an access: 100,000 `var x = …` declarations, a `switch` of some 10,000
  cases. Wider operands would take a second extra word, or wide variants of
  the property and global opcodes in the interpreter loop.
- Listing the keys of a typed array of more than 2^24 elements
  (`Object.keys`, `for`-`in`, `Reflect.ownKeys`) throws a RangeError, where
  V8 lists them. Reading and writing its elements is not limited.
- `MemoryLimit`: a few builtins keep what they build only in Go while they
  call back into JavaScript, so a measurement waits for them to return:
  `toSorted`, `Object.groupBy` and `Map.groupBy`, the `Map`, `Set`,
  `WeakMap` and `WeakSet` constructors, `Object.fromEntries` and the
  `Promise` combinators over an iterator, `JSON.stringify` with a replacer
  or a `toJSON`, `join` and `toLocaleString` over objects with a
  `toString`, and `%TypedArray%.from` with a map function and
  `%TypedArray%.prototype.map`. Garbage made in their callbacks stops the
  program once it passes half of what the limit leaves above the last
  measurement, where the same garbage in a plain loop or `forEach` runs on:
  these stop early, and never count less than the program keeps. The
  measurement of an `Intl.DateTimeFormat` misses its cached bound `format`
  function and its pattern's parts, so it counts less than such an object
  keeps. A name atom of compiled code that data still uses after the code
  is freed stays a constant the measurement skips, at most 64 bytes for
  each such name. A parse for `FromGo` (a `json.RawMessage` it
  materializes) charges its stacks but cannot stop. `Object.keys`,
  `values` and `entries` allocate their list before it is charged, at
  most the size of the object, which is counted already.
- `webapi`: under the memory limit a whole-body read such as `text()`
  never takes a body larger than the limit; without a limit nothing
  bounds such a body but the host's `Transport`. A charge it makes that
  would pass the limit measures at once. Chunks a stream or a `tee()` branch keeps
  across tasks count as JavaScript's own allocations do, once each. Bytes
  several objects share in Go count once for each of them, an overcount
  that errs on the safe side: a body's bytes and its clones', a header
  copied into other `Headers` or `Request`s, a URL's serialization and the
  `href` string or the search params read from it.
  `loop.Terminate` does not stop the fetches in flight (`Close` does). A
  nil `Allow` approves every address the client reaches, the intranet,
  loopback and `169.254.169.254` included. With `MutableIntrinsics`, a
  `Promise[Symbol.species]` a script defines runs inside the package's own
  promise reactions, because `Promise.prototype.then` reads it.
- `node`: `birthtimeMs` is `ctimeMs` on Linux (no `statx`). The Allow hook
  sees a path resolved before the access, so a program that renames a link
  into place between the two races it; the root holds either way. On a
  file system that ignores case it gets the program's spelling of a name,
  not the system's. A confined `stat` below the root takes one system call
  (`fstatat` on a cached directory descriptor) where Node.js takes one
  (`statx`), plus the descriptor's `openat2` once a millisecond (three with
  `NoDescriptorCache`). Base64
  runs at over a gigabyte a second, a quarter of Node's SIMD speed. `glob`
  returns its matches sorted, where Node.js returns its walk's order. A
  glob segment may hold one `!(...)`, outside any other extglob: Node.js
  takes any number, nested too. A
  `..` that leaves the mount after other names (`/w/a/../../w/b.txt`) is
  EACCES, where Node.js resolves it outside and back in; one right after
  the mount (`/w/../w/b.txt`) is read lexically.
- `node` Buffer, against Node.js 24: no `readBigInt64LE` and the other
  BigInt reads and writes, no `copyBytesFrom`, no `asciiSlice`,
  `utf8Write` and the other `*Slice` and `*Write` methods, no `Blob`,
  `File`, `SlowBuffer`, `transcode` or `resolveObjectURL`; `buffer.atob`
  and `btoa` are the global ones, which check their input as the HTML
  Standard does rather than as Node's do; a method called on a buffer that
  a script detached or shrank reads what is left rather than throwing; a
  receiver that is not a Uint8Array (a Uint16Array, a plain object) is
  refused or read otherwise than by Node's generic code; `copy` converts
  odd offsets (`-0.5`, beyond `2**53`) and reads a source of another typed
  array by bytes, not by elements; there is no pool, so a small Buffer has
  an ArrayBuffer of its own (`byteOffset` 0, `buffer` not shared); the
  slices of a subclass of Buffer are of the subclass; a few methods have
  another `length`; the Uint8Array constructor's message for a bad offset
  is the engine's.

## Host API

- `ParseJSON` straight from the bytes (it copies them into a string first)
- Source maps
- The outcome of a module's top-level `await` that settles after `Load`
  returned `ErrModulePending`, as with the `eventloop` package: the
  module's exports show its progress, and a rejection reaches the host only
  through the rejection tracker
- A per-realm heap or allocation budget, which would also bound what a
  result costs the host: a value that repeats one object many times, whose
  text and `Unmarshal` copies are as large as if it did not, and the buffer
  `AppendJSON` reserves from its last output
