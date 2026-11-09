# The race video

A pixel-art race, about two minutes long, in Chinese (`zh`) and English
(`en`): moejs against the other JavaScript engines a Go program can embed,
with C QuickJS as a marked reference car. This directory holds everything
that made it: the runners, the measurement, the renderer, the checks, and
the record of every number on screen (`claims.<lang>.json`).

## What the video shows

1. **Hook and pure Go.** A terminal runs `git ls-files '*.s' '*.c' | wc -l`
   in this repository and prints 0; then 0 assembly, 0 cgo, and the
   systems the engine library builds for (from `port.sh`).
2. **General JS.** The V8 benchmark suite v7 as an eight-leg relay, one
   benchmark per leg, each leg an eighth of the track. The cars are
   moejs, go-quickjs, goant, modernc, goja, Sobek, C QuickJS and moejs's
   previous release (v0.1.0-alpha.11); the last two are marked reference
   cars (white outline, hatched flanks, under their own curb). The relay
   plays at ×0.8 from GO to the freeze, and the clock shows recorded
   seconds. Then the suite's own score as a drag strip, under the
   headline *General JS: not behind*.
3. **Plugins.** An AI gateway runs a JS plugin on every request. One
   five-leg relay in real time: small requests, AI agent chat, a big
   image, many images, and eight cores at once. Then each leg's moejs
   speed against C QuickJS, under the headline *Plugins: far ahead*.
4. **Methods page and end card.**

Both headlines are checked by `video.py` before anything is drawn:
*General JS: not behind* needs moejs first of the eight cars in the V8
relay and a score at least C QuickJS's; *Plugins: far ahead* needs moejs
first in every plugin leg and every other car at least twice moejs's relay
time. "1st" means first of the cars shown.

Car positions come only from recorded timelines (`data.py`), never from a
synthetic curve. When moejs finishes a relay, the race freezes with every
car where its own timeline puts it, under a "frozen" badge, and a column
gives every car's own time. Every bar is proportional to its number.

### Numbers in the published cut

| | moejs | go-quickjs | goant | modernc | goja | Sobek | C QuickJS |
|---|---:|---:|---:|---:|---:|---:|---:|
| V8 relay, finish (s) | 14.289 | 20.261 | 94.126 | 67.167 | 142.343 | 147.175 | 21.983 |
| V8 score (run.js) | 1429 | 999 | 222 | 323 | 163 | 160 | 1058 |
| Plugin relay, total (s) | 11.858 | 61.173 | 53.393 | 170.476 | 152.065 | 154.387 | 54.842 |

moejs v0.1.0-alpha.11 finished the V8 relay in 41.547 s (moejs 2.9× faster).
moejs's speed against C QuickJS per plugin leg: small 1.058×, AI agent chat
2.914×, big image 8.023×, many images 8.190×, eight cores 2.770× (requests
done in 10 s: 56,892 against 20,540).

## How every number was measured

**Engines and builds.** All Go runners are built with Go 1.27.2,
`-trimpath` and `CGO_ENABLED=0` by `build.sh`. moejs is built from the
commit this kit was added on, with `-tags purego`; the kit adds no engine
code. v0.1.0-alpha.11 is the release tag, built the same way without tags.
goant runs its interpreter (its compiled tier off, the default) and
go-quickjs its default build. C QuickJS is release 2026-06-04: its `qjs`
shell runs the V8 programs, and a small C host runs the plugins (see
[What is not in the repository](#what-is-not-in-the-repository)).

**Workloads.**

- V8 relay (`fixed`): the suite's files at the revision pinned in
  `bench/v8suite/REVISION`, unchanged, with one shared driver
  (`make_suite.py`). Each benchmark runs a fixed number of iterations, the
  same for every engine: Richards 533, DeltaBlue 213, Crypto 16, RayTrace
  33, EarleyBoyer 13, RegExp 23, Splay 356, NavierStokes 28 (1,215, about
  13 s of moejs). An iteration runs every component of the benchmark
  (Encrypt and Decrypt; Earley and Boyer). The driver records each
  iteration's completion time with `Date`'s 1 ms resolution; setup and
  teardown stay on the clock but never move a car. Parsing precedes the
  clock.
- V8 score (`score`): `BenchmarkSuite.RunSuites`, as the suite's `run.js`
  does it, measured as its own run.
- Plugins: new-api's task plugins and the scenarios of
  `bench/scenarios/gen.py`. `pluginrace` (and the C host) serve one
  request at a time on one runtime, cycling through the scenario, and
  record the time after each request: small 100,000 requests, agent100k
  1,600, big8m 160, multi8x1m 160. JSON goes in as bytes and comes out as
  bytes, the way each engine's host API allows.
- Eight cores (`par8`): agent100k on eight single-threaded processes, one
  per CPU, each with 16 s of requests at its engine's recorded serial rate
  and its own clock. The tile compares the requests the eight finished
  within 10 s of their own start; a try counts only if every worker was
  still running 0.5 s after the window, start skew included. The relay's
  fifth leg runs to the most whole thousands of requests every engine
  finished in the window (7,000).

**Hash checks.** Before its clock starts, every plugin run executes every
request of its scenario once and hashes the results in the plugin
benchmark's check format (`hook length\nresult\n` per call). The C host
writes that output to a file and `measure.py` hashes it; the C host's hash
is the reference (`refs.json`), and a Go run with a different hash fails.
The references of the recording:

| scenario | SHA-256 of the results |
|---|---|
| small | `949933c64ca61dacd4348b022227180c63bd87b03030689b0ddf2286734e152c` |
| agent100k | `359020ec7233d589748f09211e2b1466114f733796c5ee7ea4c86b149b97343b` |
| big8m | `439b247997cf18362c0a3449ae47821a76d8cbf230ccae19dde16d310cecdc4b` |
| multi8x1m | `17fd1f965a5a698abdb01f16be819b6ea303ad6a28bce16e2ccbbe1687781dd2` |

The scenario files themselves are checked against
`bench/scenarios/SHA256SUMS` and the V8 programs are recorded in
`suite.sha256`.

**Machine.** One recording, about an hour and a half, on a KVM guest with
24 vCPUs (Intel Xeon E5-2699A v4 host). The recording used CPUs 0-19 and
left CPUs 20-23 to other work. Every process is pinned to one CPU with
`GOMAXPROCS=1`. The CPU numbers are settings (`RACE_CPUS`, `RACE_SPREAD`,
`RACE_PAR_CPUS`, `RACE_ORCH_CPU`): pin to idle cores.

**Quiet-CPU check and canaries.** Before every batch of runs and every
eight-core try, the recording's CPUs must be quiet for 3 s (at most 0.3
busy cores in total, little steal), then two throughput canaries on the
eight-core CPUs must reach 95 % and 93 % of the recording's own base (the
median of three readings at the start): a CPU-bound one (small) and a
memory-bandwidth one (big8m), eight workers each. The recording's canaries
were the plugin benchmark's C host and runner; the kit's defaults run the
same scenarios with eight `pluginrace-moejs` processes. A try during which
a foreign process showed up on the recording's CPUs (busy time on its own
CPU beyond its own, or on the idle CPUs more than 0.1 busy cores on
average or a second above 30 %) is kept in `raw/runs.jsonl` and retried.

**Parallel lanes after the interference check.** Before each scene, moejs
and C QuickJS are timed alone and beside K = 3, 5 and 7 lanes running the
same scene, the four configurations interleaved (`check/<scene>.json`).
The scene then runs on K + 1 lanes for the largest K at which, and at
every smaller K, both lose at most 3 % against alone and their ratio
moves at most 2 % (K = 0: one at a time). In a batch, every lane stays
busy with the scene's work until the last timed run ends, so a fast
engine and a slow one see the same neighbours, and engines rotate over
lanes and rounds. In the recording, the V8 relay and score ran on 8 lanes
and agent100k on 4; small, big8m and multi8x1m ran one at a time, and the
eight-core race ran alone. The methods page says which scenes shared the
machine.

**Rounds, median and outliers.** Three rounds per engine. A round more than
5 % from both others is rerun (up to three passes, under the same load);
the median round's own run is selected (`raw/<scene>/selected.json`), with
its whole timeline. With `pluginrace-ctl` and `suite-ctl` in `RACE_BIN`, an
off-screen control (another moejs build) is measured beside the others in
every scene and never shown; the recording had one.

## Files

| file | purpose |
|---|---|
| `cmd/suite` | runs a V8 program in an embedded Go engine |
| `cmd/pluginrace` | times the plugin scenarios one request at a time in a Go engine |
| `go.mod`, `go.sum` | the runners' own module (Go 1.27.2, moejs replaced by this checkout) |
| `build.sh` | builds the runners |
| `port.sh` | builds the engine library for every Go target, default and `-tags purego` |
| `make_suite.py` | assembles the V8 relay and score programs |
| `measure.py` | records every scene (interference checks, lanes, canaries, rounds, selection) |
| `data.py` | turns the selected timelines into car positions |
| `video.py` | renders the video, the stills sheet, a single still and `claims.json`; `--check` checks captions |
| `text.py` | the on-screen text in `zh` and `en`, with each language's reading-time rule |
| `pix.py`, `audio.py` | pixel-art helpers and the chiptune and sound effects |
| `render.sh` | runs the caption check, stills, claims, video, `verify.py` and a contact sheet |
| `verify.py` | decodes the whole file and re-derives every number from the selected runs |
| `compare.py` | a side-by-side sheet of two cuts at matching moments |
| `claims.zh.json`, `claims.en.json` | every number and caption of the published cuts, with sources |
| `verify.zh.json`, `verify.en.json` | `verify.py`'s report on the published cuts |

## Build, measure, render, verify

```sh
# inputs
bench/testdata/plugins/fetch.sh && python3 bench/scenarios/gen.py
bench/v8suite/fetch.sh
# runners (Go 1.27.2), then put qjs and racehost into race-bin
bench/race/build.sh
# portability, for the systems scene and the methods page
bench/race/port.sh
# the recording, scene by scene (resumable: finished rounds are kept)
bench/race/measure.py
# both cuts
bench/race/render.sh zh && bench/race/render.sh en
# a single still: the plugin relay's results by default, another time with --at
uv run --no-project --with pillow==13.0.0 --with numpy==2.4.6 --with imageio-ffmpeg==0.7.0 \
  python bench/race/video.py race-out unused.mp4 --lang en --still race-out/still.en.png
```

The scripts read `RACE_BIN` (default `race-bin`), `RACE_OUT` (the
recording, default `race-out`), `RENDER_OUT` (default `race-render`) and
`RACE_FONT`; `measure.py`'s docstring lists the rest. Hold a lock around
every timed run if other benchmarks share the machine (`RACE_LOCK`), and
render only when nothing is being timed.

`render.sh` stops if a caption had to shrink or stays up for less than
its reading time: in `zh` 2.5 s plus 0.25 s per word (a Chinese character
or a run of Latin letters and digits), in `en` 2.5 s plus 0.06 s per
character. All text is the 12 px font at 4× or larger (48 px at 1080p).
`verify.py` checks 1920×1080 at 60 fps, decodes video and audio to the
end, and re-derives every measured number in `claims.<lang>.json` from
the selected runs, their hashes and their rounds. To check a render
against the published cut, compare `claims.<lang>.json`; rendering is
deterministic, so the same recording gives the same frames.

The published cuts: `moejs-race.zh.mp4`, 113.67 s, 6,820 frames;
`moejs-race.en.mp4`, 110.77 s, 6,646 frames.

## What is not in the repository

- **Go 1.27.2.**
- **C QuickJS, release 2026-06-04** (bellard.org/quickjs), built with its
  own Makefile: the `qjs` shell, which `measure.py` runs as
  `qjs suite/fixed-cli.js` (and `score-cli.js`), and a plugin host,
  `racehost`, that you
  build against the release's objects (`quickjs.o`, `libregexp.o`,
  `libunicode.o`, `cutils.o`, `dtoa.o`) with `gcc -O2 ... -lm -lpthread`.
  The repository holds no C, so the host is described here, not shipped.
  `measure.py` runs it as

  ```
  racehost -engine qjs -dir DIR -scenario NAME -n N -check FILE
  ```

  It reads `DIR/plugins.js` and `DIR/scn/NAME.bin` (the format is in
  `bench/scenarios/gen.py`), creates one runtime and context, evaluates
  `plugins.js` as a strict global script and takes the global `__call`.
  A call parses its argument bytes with `JS_ParseJSON`, calls
  `__call(hook, args)`, serializes the result with `JS_JSONStringify` and
  copies the text into a buffer the host owns. First it runs every
  request of the scenario once and writes `hook length\nresult\n` per
  call to `FILE`; then it times `N` requests, cycling through the
  scenario, and records the milliseconds since the start
  (`CLOCK_MONOTONIC`) after each one. It prints one line:
  `{"engine":"qjs","scenario":NAME,"requests":N,"ms":[...]}`, three
  decimals per time.
- **The plugins and scenarios**, written by `bench/testdata/plugins/fetch.sh`
  and `bench/scenarios/gen.py` into `bench/testdata/scenarios`.
- **The V8 benchmark suite v7**, downloaded by `bench/v8suite/fetch.sh`.
- **Python 3 with uv**; `render.sh` runs pillow 13.0.0, numpy 2.4.6 and
  imageio-ffmpeg 0.7.0 (which brings ffmpeg with libx264) through
  `uv run`.
- **The font**: Fusion Pixel 12px proportional, zh_hans, by TakWolf
  (github.com/TakWolf/fusion-pixel-font), release 2026.10.25, from the
  release's 12px proportional TTF archive:
  `fusion-pixel-12px-proportional-zh_hans.ttf`, SHA-256
  `58a0ac8784bfb0ed70b788a9cf815d00fcad9daa9e0517b8d1c244b4f912e3d6`.
  It is licensed under the SIL Open Font License 1.1. Put it in
  `bench/race/fonts/` (git-ignored) or point `RACE_FONT` at it.
