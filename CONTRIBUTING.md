# Contributing to moejs

Issues and pull requests can be written in English or Chinese.

## Reporting a bug

Open an issue with the bug report form. The most useful reports have:

- the smallest JavaScript that shows the problem
- the Go code that runs it, when the bug is in how moejs takes or returns
  values
- the moejs version (`go list -m github.com/MoeForge/moejs`), the Go
  version (`go version`) and your OS and architecture
- what Node prints for the same JavaScript, when it differs from moejs

Features listed in [TODO.md](TODO.md) fail with an error that names the
feature and says it "is not supported yet". To ask for one of them, use
the feature request form and describe the plugin code that needs it. Code that
runs and gives a different result than Node is a bug, unless TODO.md lists
it under "Known wrong results".

Report security problems privately, as [SECURITY.md](SECURITY.md)
describes.

## Running the tests

The root module needs Go 1.25 or later. The bench module needs Go 1.26.6
or later and a C compiler, because its QuickJS and V8 baselines use cgo.

Some test inputs are downloaded separately. Tests that need them skip
until they are there:

```sh
bench/testdata/plugins/fetch.sh   # new-api's task plugins
bench/test262/fetch.sh            # the pinned test262 revision
bench/tscorpus/fetch.sh           # TypeScript sources for the CompileTS tests
```

Then:

```sh
go vet ./...
go test -race ./...                 # the engine's unit, audit and fuzz-corpus tests
cd bench && go test -timeout 30m ./... # differential tests, test262, benchmarks
```

[bench/test262/README.md](bench/test262/README.md) shows how to run one
test262 directory and how to update the baseline.

## Where things are

- The root package is the public API: `Runtime`, `Module`, values,
  `Unmarshal` and the rest that [docs/guide.md](docs/guide.md) describes.
- `engine` has the values, objects, builtins and the interpreter. Most
  builtins live in an `engine/builtin_*.go` file, with their tests next to
  it.
- `syntax` is the lexer and parser, `compiler` turns the syntax tree into
  bytecode, and `bytecode` defines the instructions.
- `eventloop` adds Node.js timers and an event loop.
- `bench` holds the differential tests against Sobek and esbuild, the
  test262 runner and the benchmarks.

## Pull requests

- Keep a pull request to one change, with tests. An engine test can run a
  snippet of JavaScript and compare its result: `TestErrorIsError` in
  `engine/builtin_error_test.go` is a short example.
- Run `gofmt`, `go vet` and the tests above before you open it.
- When a pull request implements a feature that test262 tags, add the tag
  to `bench/test262/features.go` and regenerate the baseline with
  `-update`, which runs the whole suite, in the same pull request.
- When it changes what runs, update TODO.md and, for the Go API,
  docs/guide.md.
- For a performance change, include the benchmark numbers before and after
  and the command that produced them. A change to strings, JSON or the
  interpreter loop should also show the plugin scenarios, which
  `bench/scripts/plugin_ab.sh` runs with moejs before and after the change
  ([docs/performance.md](docs/performance.md#reproducing) shows how to set
  them up).

We squash a pull request into one commit when we merge it, so the branch's
commit history can stay as it is.

## Where to start

Issues labelled `good first issue` are small builtins with their own
test262 tests, so you know when you are done. Issues labelled `help wanted`
are larger. Leave a comment on an issue before you start, so others can see
that someone is on it.
