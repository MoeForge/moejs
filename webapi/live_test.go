package webapi_test

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/webapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLiveValuesReachLimit checks that what a script parks where only the
// package's Go state holds it counts for the memory limit, which bounds
// what the run keeps however many tasks it took to keep it: each case
// parks about 19 MiB twice, a task apart, under a limit of 32 MiB, and the
// second passes the limit. The measurement between the two sees the first
// only through what the package reports. The value is many small objects,
// whose allocation past the limit stops the loop; a typed array of that
// size would throw a RangeError the script can catch instead. Each park
// runs in a function of its own, whose registers go with it. The controls
// park it in a global (it counts) and drop it (it does not).
func TestLiveValuesReachLimit(t *testing.T) {
	srv := newServer(t)
	for _, c := range []struct {
		name, park string
		fits       bool
	}{
		{"dropped", `big()`, true},
		{"global", `keep.push(big())`, false},
		{"queued chunk", `keep.push(new ReadableStream({ start(c) { c.enqueue(big()) } }))`, false},
		{"queued chunk of a tee branch", `const [a, b] = new ReadableStream({ start(c) { c.enqueue(big()) } }).tee();
keep.push(a); a.getReader().read()`, false},
		{"source", `const b = big(); keep.push(new ReadableStream({ pull() { return b.length } }))`, false},
		{"iterator of ReadableStream.from", `const b = big(); keep.push(ReadableStream.from((function* () { yield b })()))`, false},
		{"stored error", `keep.push(new ReadableStream({ start(c) { c.error(big()) } }))`, false},
		{"pending read", `const rd = new ReadableStream().getReader(); keep.push(rd);
(async () => { const b = big(); await rd.read(); return b.length })()`, false},
		{"pending async iteration", `const s = new ReadableStream(); keep.push(s);
(async () => { const b = big(); for await (const x of s) return b.length })()`, false},
		{"reader closed", `const rd = new ReadableStream().getReader(); keep.push(rd);
(async () => { const b = big(); await rd.closed; return b.length })()`, false},
		{"sink", `const b = big(); keep.push(new WritableStream({ write() { return b.length } }))`, false},
		{"queued write", `const w = new WritableStream({ write() { return new Promise(() => {}) } }).getWriter(); keep.push(w);
w.write(1); w.write(big())`, false},
		{"pending write", `const w = new WritableStream({ write() { return new Promise(() => {}) } }).getWriter(); keep.push(w);
(async () => { const b = big(); await w.write(1); return b.length })()`, false},
		{"transformer", `const b = big(); keep.push(new TransformStream({ transform() { return b.length } }))`, false},
		{"pending pipe", `const s = new ReadableStream(); keep.push(s);
(async () => { const b = big(); await s.pipeTo(new WritableStream()); return b.length })()`, false},
		{"listener", `const ac = new AbortController(); const b = big(); ac.signal.addEventListener("abort", () => b.length); keep.push(ac)`, false},
		{"handler", `const ac = new AbortController(); const b = big(); ac.signal.onabort = () => b.length; keep.push(ac)`, false},
		{"event target", `const et = new EventTarget(); const b = big(); et.addEventListener("x", () => b.length); keep.push(et)`, false},
		{"abort reason", `keep.push(AbortSignal.abort(big()))`, false},
		{"timeout signal", `const b = big(); AbortSignal.timeout(60000).addEventListener("abort", () => b.length)`, false},
		{"dependent signal", `const ac = new AbortController(); keep.push(ac); const b = big();
AbortSignal.any([ac.signal]).addEventListener("abort", () => b.length)`, false},
		{"pending fetch", `(async () => { const b = big(); await fetch(BASE + "/slow"); return b.length })().catch(() => {})`, false},
		{"pending text", `const r = await fetch(BASE + "/stall");
(async () => { const b = big(); await r.text(); return b.length })().catch(() => {})`, false},
		{"pending cancel", `const s = new ReadableStream({ cancel() { return new Promise(r => keep.push(r)) } }); keep.push(s);
(async () => { const b = big(); await s.cancel(); return b.length })()`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := limitHarness(t, srv, 32<<20)
			// Without the limit's stop, the fetches would keep the loop.
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			h.api.SetContext(ctx)
			got, err := h.runErr(`globalThis.keep = [];
const big = () => { const a = []; for (let i = 0; i < 131072; i++) a.push({ i }); return a };
for (let i = 0; i < 2; i++) {
  await (async () => {
    ` + c.park + `;
  })();
  await new Promise(r => setTimeout(r, 1));
}
return "parked"`)
			if c.fits {
				assert.NoError(t, err)
				assert.Equal(t, "parked", got)
			} else {
				assert.ErrorIs(t, err, moejs.ErrMemoryLimit, "result %q", got)
			}
		})
	}
}

// TestLiveValuesClose checks what reaches a measurement and when: what
// an object keeps reports through the object while JavaScript reaches it,
// limit or not; the work in flight (a fetch an async function waits for,
// holding 8 MiB) is registered under a memory limit only, and Close ends
// its registration.
func TestLiveValuesClose(t *testing.T) {
	srv := newServer(t)
	for _, limit := range []int64{64 << 20, 0} {
		rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
		h := newHarnessRT(t, rt, webapi.Options{Client: &http.Client{Transport: &http.Transport{}}})
		require.NoError(t, h.rt.SetGlobal("BASE", srv.URL))
		var parked, inFlight, closed int64
		require.NoError(t, h.rt.SetGlobal("measure", moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
			n := rt.Realm().MeasureLive(nil)
			switch moejs.Arg(args, 0).String() {
			case "parked":
				parked = n
			case "in flight":
				inFlight = n
			case "closed":
				closed = n
			}
			return moejs.Undefined(), nil
		})))
		require.NoError(t, h.rt.SetGlobal("closeAPI", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
			h.api.Close()
			return moejs.Undefined(), nil
		})))
		h.run(`globalThis.s = new ReadableStream({ start(c) { c.enqueue(new Uint8Array(8 << 20)) } });
measure("parked"); s = undefined;
const p = (async () => { const b = new Uint8Array(8 << 20); await fetch(BASE + "/slow"); return b.length })().catch(() => 0);
await new Promise(r => setTimeout(r, 10));
measure("in flight"); closeAPI(); measure("closed");
await p; return 1`)
		t.Logf("limit %d: parked %d, in flight %d, closed %d", limit, parked, inFlight, closed)
		assert.Greater(t, parked, int64(8<<20), "limit %d: a stream's queued chunk", limit)
		if limit > 0 {
			assert.Greater(t, inFlight, int64(8<<20), "a fetch in flight")
		} else {
			assert.Less(t, inFlight, int64(8<<20), "without a limit nothing is registered")
		}
		assert.Less(t, closed, int64(8<<20), "limit %d: after Close", limit)
	}
}

// TestLiveDroppedStatesUnreported checks that what Responses and streams
// JavaScript dropped keep stops counting in the next measurement, with
// Go's collector held off: in the task that dropped them, after an awaited
// function, in the next task and after the run.
func TestLiveDroppedStatesUnreported(t *testing.T) {
	old := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(old)
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: 1 << 30})
	h := newHarnessRT(t, rt, webapi.Options{})
	var log []string
	var base int64
	require.NoError(t, h.rt.SetGlobal("probe", moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		n := rt.Realm().MeasureLive(nil)
		if base == 0 {
			base = n
		}
		log = append(log, fmt.Sprintf("%s: %+d", moejs.Arg(args, 0).String(), n-base))
		assert.Less(t, n-base, int64(1<<20), "%s: 10 MiB dropped", moejs.Arg(args, 0).String())
		return moejs.Undefined(), nil
	})))
	h.run(`const drop = () => { for (let i = 0; i < 5; i++) {
  new Response("x".repeat(1 << 20)); new ReadableStream({ start(c) { c.enqueue(new Uint8Array(1 << 20)) } }) } };
probe("start");
(() => drop())(); probe("same task, after the function returned");
await (async () => drop())(); probe("after an awaited async function");
await new Promise(r => setTimeout(r, 1)); probe("next task");
return 1`)
	n := rt.Realm().MeasureLive(nil)
	log = append(log, fmt.Sprintf("after the run: %+d", n-base))
	assert.Less(t, n-base, int64(1<<20), "after the run")
	for _, l := range log {
		t.Log(l)
	}
}

// parkLive parks what park makes n times, a task apart, under a limit of
// 32 MiB, after a warm-up whose garbage makes the runtime measure once, so
// that what a park allocates and drops is not counted against the next.
// between runs before each task's end.
func parkLive(t *testing.T, srv *server, n int, park, between string) (string, error) {
	h := limitHarness(t, srv, 32<<20)
	require.NoError(t, h.rt.SetGlobal("gc", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		runtime.GC()
		runtime.GC()
		return moejs.Undefined(), nil
	})))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	h.api.SetContext(ctx)
	return h.runErr(`globalThis.keep = [];
const s4 = () => "x".repeat(4 << 20);
await (async () => { "w".repeat(17 << 20) })();
await new Promise(r => setTimeout(r, 1));
for (let i = 0; i < ` + strconv.Itoa(n) + `; i++) {
  await (async () => {
` + park + `
  })();
  ` + between + `
  await new Promise(r => setTimeout(r, 1));
}
return "fitted"`)
}

// counted reports whether the limit counted what was parked: a RangeError
// of an allocation the script made, or the limit's stop.
func counted(got string, err error) bool { return err != nil || strings.HasPrefix(got, "REJECTED") }

// TestLiveGoStrings checks that the Go strings the package keeps for a
// script (header names and values, URLs, search params, statusText, an
// exception's message, a listener's type) count for the memory limit: 4
// MiB kept ten times under 32 MiB does not fit, while each park, whose
// string JavaScript and Go both hold for a moment, does.
func TestLiveGoStrings(t *testing.T) {
	srv := newServer(t)
	for _, c := range []struct {
		name, park string
		fits       bool
	}{
		{"control dropped", `s4()`, true},
		{"control global", `keep.push(s4())`, false},
		{"Headers value", `keep.push(new Headers([["x", s4()]]))`, false},
		{"URL", `keep.push(new URL("http://a/?" + s4()))`, false},
		{"URLSearchParams", `keep.push(new URLSearchParams({ a: s4() }))`, false},
		{"URL's searchParams", `const u = new URL("http://a/?" + s4()); u.searchParams; keep.push(u)`, false},
		{"Request url", `keep.push(new Request("http://a/?" + s4()))`, false},
		{"Request header", `keep.push(new Request("http://a/", { headers: { x: s4() } }))`, false},
		{"Response statusText", `keep.push(new Response(null, { statusText: s4() }))`, false},
		{"Response header", `keep.push(new Response(null, { headers: { x: s4() } }))`, false},
		{"DOMException message", `keep.push(new DOMException(s4()))`, false},
		{"listener type", `const t = new EventTarget(); t.addEventListener(s4(), () => {}); keep.push(t)`, false},
		{"dropped Headers", `new Headers([["x", s4()]])`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var m0, m1 runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&m0)
			got, err := parkLive(t, srv, 10, c.park, "")
			runtime.GC()
			runtime.ReadMemStats(&m1)
			t.Logf("%q, %v; Go heap after: %+d MiB", got, err, (int64(m1.HeapAlloc)-int64(m0.HeapAlloc))>>20)
			assert.Equal(t, !c.fits, counted(got, err), "counted %v", counted(got, err))
		})
	}
}

// TestLiveDroppedStatesFit checks that a state JavaScript dropped no longer
// counts for the memory limit, also before Go's collector frees it: six
// parks of 8 MiB, a task apart, under 32 MiB, each dropping what it made,
// fit with the collector held off (a large heap, where collections are
// rare) and with one before each park.
func TestLiveDroppedStatesFit(t *testing.T) {
	srv := newServer(t)
	for _, gcBetween := range []bool{false, true} {
		for _, c := range []struct{ name, park string }{
			{"control: dropped typed array", `new Uint8Array(8 << 20)`},
			{"dropped stream with a queued chunk", `const b = new Uint8Array(8 << 20); new ReadableStream({ start(c) { c.enqueue(b) } })`},
			{"dropped Response with a body", `new Response(new Uint8Array(8 << 20))`},
			{"dropped clone, other branch read", `const r = new Response(new ReadableStream({ start(c) { c.enqueue(new Uint8Array(8 << 20)); c.close() } }));
const c2 = r.clone(); await r.arrayBuffer()`},
			{"completed fetch with a request body", `await fetch(BASE + "/status/204", { method: "POST", body: new Uint8Array(6 << 20) })`},
			{"dropped signal with a listener", `const b = new Uint8Array(8 << 20); new AbortController().signal.addEventListener("abort", () => b.length)`},
			{"dropped Headers", `new Headers([["x", "x".repeat(8 << 20)]])`},
		} {
			name := c.name
			between := ""
			if gcBetween {
				name += ", collected between"
				between = "gc();"
			}
			t.Run(name, func(t *testing.T) {
				old := debug.SetGCPercent(-1)
				defer debug.SetGCPercent(old)
				runtime.GC()
				got, err := parkLive(t, srv, 6, c.park, between)
				t.Logf("%q, %v", got, err)
				assert.False(t, counted(got, err), "the script keeps nothing")
			})
		}
	}
}
