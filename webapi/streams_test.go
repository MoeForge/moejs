package webapi_test

// The Streams (readable.go, writable.go, transform.go) against the WHATWG
// Streams Standard: each case's want is what Node.js v22.23.1 returns for
// the same body. A Go panic out of loop.Run fails the case with the top of
// its stack.

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/webapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type streamCase struct {
	name    string
	mutable bool   // patches Object.prototype: needs MutableIntrinsics
	want    string // Node.js v22.23.1's result for body
	body    string
}

// runStream runs body as the body of an async function on a fresh runtime
// and returns its result ("UNFINISHED" when it never settles), the Go
// panic that escaped loop.Run with the webapi frames of its stack, and
// loop.Run's error.
func runStream(t *testing.T, mutable bool, body string) (res, panicked string, runErr error) {
	t.Helper()
	h := newHarnessRT(t, moejs.NewRuntime(moejs.Options{MutableIntrinsics: mutable}), webapi.Options{})
	src := `(async () => {` + body + `
})().then(v => done(typeof v === "object" && v !== null ? JSON.stringify(v) : String(v)),
          e => done("REJECTED " + (e && e.name ? e.name + ": " + e.message : String(e))));`
	s, err := moejs.CompileScript("streams.js", src)
	require.NoError(t, err)
	defer func() {
		if p := recover(); p != nil {
			panicked = fmt.Sprintf("%v\n%s", p, webapiFrames(string(debug.Stack())))
		}
	}()
	runErr = h.loop.Run(func(rt *moejs.Runtime) error {
		_, err := rt.RunScript(s)
		return err
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.done {
		return "UNFINISHED", "", runErr
	}
	return h.result, "", runErr
}

// webapiFrames keeps the frames of package webapi (not its tests) of a stack.
func webapiFrames(stack string) string {
	var out []string
	lines := strings.Split(stack, "\n")
	for i := 0; i+1 < len(lines) && len(out) < 12; i++ {
		if strings.Contains(lines[i+1], "/webapi/") && !strings.Contains(lines[i+1], "_test.go") {
			out = append(out, strings.TrimSpace(lines[i]), "  "+strings.TrimSpace(lines[i+1]))
			i++
		}
	}
	return strings.Join(out, "\n")
}

func TestStreamsMatchNode(t *testing.T) {
	for _, c := range streamCases {
		t.Run(c.name, func(t *testing.T) {
			got, panicked, err := runStream(t, c.mutable, c.body)
			if panicked != "" {
				t.Errorf("Go panic (expected %s):\n%s", c.want, panicked)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
}

var streamCases = []streamCase{
	// ---- Go panics ---------------------------------------------------------------

	// reader.read() takes a queued chunk, and the pull it starts releases
	// the reader.
	{name: "ReadFastPathPullReleasesLock", want: `["a",false,false]`, body: `
let reader;
const rs = new ReadableStream({
  start(c) { c.enqueue('a'); },
  pull(c) { reader.releaseLock(); },
});
reader = rs.getReader();
await null; await null;
const r = await reader.read();
return [r.value, r.done, rs.locked];
`},

	// return() while a next() is pending waits for it: the stream is
	// canceled after the chunk is read.
	{name: "IteratorReturnWithPendingNext", want: `["x",false,"bye",true,"bye"]`, body: `
let cr = null;
const rs = new ReadableStream({
  start(c) { setTimeout(() => c.enqueue('x'), 10); },
  cancel(r) { cr = r; },
});
const it = rs.values();
const p1 = it.next();
const p2 = it.return('bye');
const [a, b] = await Promise.all([p1, p2]);
return [a.value, a.done, b.value, b.done, cr];
`},

	// The same two, run from the jobs of a task of the package (the one
	// of AbortSignal.timeout).
	{name: "ReadFastPathPullReleasesLockInTask", want: `["a",false,false]`, body: `
const keep = setTimeout(() => {}, 200);
await new Promise(r => AbortSignal.timeout(1).addEventListener('abort', r));
let reader;
const rs = new ReadableStream({
  start(c) { c.enqueue('a'); },
  pull(c) { reader.releaseLock(); },
});
reader = rs.getReader();
await null; await null;
const r = await reader.read();
clearTimeout(keep);
return [r.value, r.done, rs.locked];
`},
	{name: "IteratorReturnWithPendingNextInTask", want: `[{"done":true},null]`, body: `
const keep = setTimeout(() => {}, 200);
await new Promise(r => AbortSignal.timeout(1).addEventListener('abort', r));
let cr = null;
const rs = new ReadableStream({ cancel(r) { cr = r; } });
const it = rs.values();
const p1 = it.next();
it.return('bye');
clearTimeout(keep);
const v = await Promise.race([p1, new Promise(r => setTimeout(() => r({ done: true }), 20))]);
return [v, cr];
`},

	// ---- async iterator ----------------------------------------------------------

	{name: "IteratorConcurrentNextThenClose", want: `[{"done":true},{"done":true}]`, body: `
let ctrl;
const rs = new ReadableStream({ start(c) { ctrl = c; } });
const it = rs[Symbol.asyncIterator]();
const p1 = it.next(), p2 = it.next();
ctrl.close();
const s = await Promise.allSettled([p1, p2]);
return s.map(x => x.status === 'fulfilled' ? x.value : x.reason.name + ': ' + x.reason.message);
`},
	{name: "IteratorConcurrentNextThenError", want: `["boom",{"done":true}]`, body: `
let ctrl;
const rs = new ReadableStream({ start(c) { ctrl = c; } });
const it = rs[Symbol.asyncIterator]();
const p1 = it.next(), p2 = it.next();
ctrl.error(new Error('boom'));
const s = await Promise.allSettled([p1, p2]);
return s.map(x => x.status === 'fulfilled' ? x.value : x.reason.message);
`},
	{name: "IteratorConcurrentNextValues", want: `[{"value":1,"done":false},{"done":true},{"done":true}]`, body: `
let ctrl;
const rs = new ReadableStream({ start(c) { ctrl = c; } });
const it = rs[Symbol.asyncIterator]();
const ps = [it.next(), it.next(), it.next()];
ctrl.enqueue(1); ctrl.close();
const s = await Promise.allSettled(ps);
return s.map(x => x.status === 'fulfilled' ? x.value : x.reason.name);
`},
	{name: "IteratorReturnPreventCancel", want: `[false,false,2]`, body: `
let canceled = false;
const rs = new ReadableStream({ start(c) { c.enqueue(1); c.enqueue(2); }, cancel() { canceled = true; } });
for await (const x of rs.values({ preventCancel: true })) break;
const locked = rs.locked;
const x = await rs.getReader().read();
return [canceled, locked, x.value];
`},
	{name: "IteratorBreakCancels", want: `["undefined",false]`, body: `
let reason = 'none';
const rs = new ReadableStream({ start(c) { c.enqueue(1); c.enqueue(2); }, cancel(r) { reason = r; } });
for await (const x of rs) break;
return [String(reason), rs.locked];
`},
	{name: "IteratorOnLocked", want: `"TypeError"`, body: `
const rs = new ReadableStream(); rs.getReader();
try { rs.values(); return 'ok'; } catch (e) { return JSON.stringify(e.name); }
`},

	// ---- backpressure: readable -----------------------------------------------------

	{name: "PullNotReentrantNorWhilePending", want: `[1,2,false]`, body: `
let inPull = false, reentered = false, pulls = 0, resolvePull;
const rs = new ReadableStream({
  pull(c) { if (inPull) reentered = true; inPull = true; pulls++; c.enqueue(pulls); inPull = false; return new Promise(r => resolvePull = r); },
}, { highWaterMark: 3 });
await new Promise(r => setTimeout(r, 10));
const a = pulls;
resolvePull(); await new Promise(r => setTimeout(r, 10));
return [a, pulls, reentered];
`},
	{name: "SyncPullFillsToHWM", want: `[3,0,4]`, body: `
let pulls = 0, ctrl;
const rs = new ReadableStream({ start(c) { ctrl = c; }, pull(c) { pulls++; c.enqueue(pulls); } }, { highWaterMark: 3 });
await new Promise(r => setTimeout(r, 10));
const a = pulls, d = ctrl.desiredSize;
await rs.getReader().read();
await new Promise(r => setTimeout(r, 10));
return [a, d, pulls];
`},
	{name: "HWM0OnePullPerRead", want: `[0,3]`, body: `
let pulls = 0;
const rs = new ReadableStream({ pull(c) { pulls++; c.enqueue(pulls); } }, { highWaterMark: 0 });
await new Promise(r => setTimeout(r, 10));
const a = pulls;
const r = rs.getReader();
await r.read(); await r.read(); await r.read();
await new Promise(r => setTimeout(r, 10));
return [a, pulls];
`},
	{name: "SizeStrategyDesiredSize", want: `[10,7,3,-2,1]`, body: `
let c;
const rs = new ReadableStream({ start(x) { c = x; } }, { highWaterMark: 10, size: ch => ch.length });
const out = [c.desiredSize];
c.enqueue('abc'); out.push(c.desiredSize);
c.enqueue('abcd'); out.push(c.desiredSize);
c.enqueue('abcde'); out.push(c.desiredSize);
await rs.getReader().read(); out.push(c.desiredSize);
return out;
`},
	{name: "SizeBadValues", want: `["Error","read Error","RangeError","read RangeError","RangeError","read RangeError","RangeError","read RangeError","RangeError","read RangeError"]`, body: `
const out = [];
for (const f of [() => { throw new Error('sz'); }, () => NaN, () => Infinity, () => -1, () => 'x']) {
  let c; const rs = new ReadableStream({ start(x) { c = x; } }, { size: f, highWaterMark: 5 });
  try { c.enqueue('a'); out.push('ok:' + c.desiredSize); } catch (e) { out.push(e.name); }
  try { await rs.getReader().read(); out.push('read ok'); } catch (e) { out.push('read ' + e.name); }
}
return out;
`},
	{name: "SizeReentrantEnqueue", want: `["b","a"]`, body: `
let c;
const rs = new ReadableStream({ start(x) { c = x; } }, { highWaterMark: 10, size(ch) { if (ch === 'a') c.enqueue('b'); return 1; } });
c.enqueue('a'); c.close();
const r = rs.getReader(), out = [];
for (;;) { const x = await r.read(); if (x.done) break; out.push(x.value); }
return out;
`},
	{name: "SizeCallsError", want: `["none","in size"]`, body: `
let c;
const rs = new ReadableStream({ start(x) { c = x; } }, { highWaterMark: 10, size() { c.error(new Error('in size')); return 1; } });
let thrown = 'none';
try { c.enqueue('a'); } catch (e) { thrown = e.message; }
try { await rs.getReader().read(); return 'read ok'; } catch (e) { return [thrown, e.message]; }
`},
	{name: "SizeCallsReadOrder", want: `["read:b","next:a"]`, body: `
let c, r; const out = [];
const rs = new ReadableStream({ start(x) { c = x; } }, { highWaterMark: 10, size(ch) { if (ch === 'a') r.read().then(v => out.push('read:' + v.value)); return 1; } });
r = rs.getReader();
await null; await null;
c.enqueue('a'); c.enqueue('b');
await new Promise(res => setTimeout(res, 0));
const x = await r.read();
out.push('next:' + x.value);
return out;
`},
	{name: "SizeCallsReleaseLock", want: `[false,"a"]`, body: `
let c, r;
const rs = new ReadableStream({ start(x) { c = x; } }, { highWaterMark: 10, size() { r.releaseLock(); return 1; } });
r = rs.getReader();
c.enqueue('a');
const locked = rs.locked;
const x = await rs.getReader().read();
return [locked, x.value];
`},

	// ---- backpressure: writable ------------------------------------------------------

	{name: "WriterReadyTransitions", want: `[2,1,0,false,true,1]`, body: `
const log = []; let resolveWrite;
const ws = new WritableStream({ write() { return new Promise(r => resolveWrite = r); } }, { highWaterMark: 2 });
const w = ws.getWriter();
await null;
log.push(w.desiredSize);
w.write('a'); log.push(w.desiredSize);
w.write('b'); log.push(w.desiredSize);
let readyResolved = false; w.ready.then(() => readyResolved = true);
await new Promise(r => setTimeout(r, 0));
log.push(readyResolved);
resolveWrite(); await new Promise(r => setTimeout(r, 0));
log.push(readyResolved, w.desiredSize);
return log;
`},
	{name: "WritableSizeInfinity", want: `["RangeError","RangeError",null]`, body: `
const ws = new WritableStream({}, { size: () => Infinity });
const w = ws.getWriter();
const a = await w.write('x').then(() => 'ok', e => e.name);
const b = await w.closed.then(() => 'ok', e => e.name);
return [a, b, w.desiredSize];
`},
	{name: "WritableSizeThrows", want: `["sz","sz"]`, body: `
const ws = new WritableStream({}, { size() { throw new Error('sz'); } });
const w = ws.getWriter();
const a = await w.write('x').then(() => 'ok', e => e.message);
const b = await w.closed.then(() => 'ok', e => e.message);
return [a, b];
`},
	// size() releases the writer: the spec's WritableStreamDefaultWriterWrite
	// step 5 rejects with a TypeError ("stream is not writer.[[stream]]").
	{name: "WritableSizeReleasesWriter", want: `["TypeError",[]]`, body: `
let w; const got = [];
const ws = new WritableStream({ write(ch) { got.push(ch); } }, { size() { w.releaseLock(); return 1; } });
w = ws.getWriter();
await null;
const a = await w.write('a').then(() => 'resolved', e => e.name);
await new Promise(r => setTimeout(r, 0));
return [a, got];
`},
	// The controller's signal is aborted by WritableStreamAbort only, not by
	// controller.error().
	{name: "WritableSignalAfterControllerError", want: `[false,false]`, body: `
let c1, c2;
const ws1 = new WritableStream({ start(c) { c1 = c; } });
const ws2 = new WritableStream({ start(c) { c2 = c; c.signal; } });
await null;
c1.error(new Error('e')); c2.error(new Error('e'));
return [c1.signal.aborted, c2.signal.aborted];
`},
	{name: "WritableAbortDuringWrite", want: `["write","resolve write","abort:R","write ok","abort resolved"]`, body: `
const log = []; let resolveWrite;
const ws = new WritableStream({ write() { log.push('write'); return new Promise(r => resolveWrite = r); }, abort(r) { log.push('abort:' + r); } });
const w = ws.getWriter();
const wp = w.write('a').then(() => log.push('write ok'), e => log.push('write rej'));
await null;
const ap = w.abort('R').then(() => log.push('abort resolved'));
await new Promise(r => setTimeout(r, 0));
log.push('resolve write');
resolveWrite();
await ap; await wp;
return log;
`},
	{name: "WritableCloseAfterWrites", want: `["w:a","w:b","close","closed"]`, body: `
const log = [];
const ws = new WritableStream({ write(x) { log.push('w:' + x); return new Promise(r => setTimeout(r, 2)); }, close() { log.push('close'); } });
const w = ws.getWriter();
w.write('a'); w.write('b');
await w.close(); log.push('closed');
return log;
`},

	// ---- pipeTo ----------------------------------------------------------------------

	// A sink whose write never settles: how far does the pipe read ahead?
	{name: "PipeToBackpressureSlowSink", want: `[2,1]`, body: `
let pulls = 0, writes = 0;
const rs = new ReadableStream({ pull(c) { c.enqueue(++pulls); } });
const ws = new WritableStream({ write() { writes++; return new Promise(() => {}); } });
rs.pipeTo(ws);
await new Promise(r => setTimeout(r, 30));
return [pulls, writes];
`},
	{name: "PipeThroughBackpressureUnreadReadable", want: `[2,0]`, body: `
let pulls = 0, transforms = 0;
const rs = new ReadableStream({ pull(c) { c.enqueue(++pulls); } });
const out = rs.pipeThrough(new TransformStream({ transform(ch, c) { transforms++; c.enqueue(ch); } }));
await new Promise(r => setTimeout(r, 30));
return [pulls, transforms];
`},
	{name: "PipeToHWM0Dest", want: `["timeout",[]]`, body: `
const rs = new ReadableStream({ start(c) { c.enqueue('a'); c.enqueue('b'); c.close(); } });
const got = [];
const ws = new WritableStream({ write(ch) { got.push(ch); } }, { highWaterMark: 0 });
const t = new Promise(r => setTimeout(() => r('timeout'), 50));
const res = await Promise.race([rs.pipeTo(ws).then(() => 'done'), t]);
return [res, got];
`},
	{name: "PipeToSourceErrorAbortsDest", want: `["rej:src","src",false,false]`, body: `
let rc, aborted = 'no';
const rs = new ReadableStream({ start(c) { rc = c; } });
const ws = new WritableStream({ abort(r) { aborted = r.message; } });
const p = rs.pipeTo(ws);
rc.error(new Error('src'));
const res = await p.then(() => 'ok', e => 'rej:' + e.message);
return [res, aborted, rs.locked, ws.locked];
`},
	{name: "PipeToSourceErrorPreventAbort", want: `["rej:src","no","w:b"]`, body: `
let rc, aborted = 'no'; const log = [];
const rs = new ReadableStream({ start(c) { rc = c; } });
const ws = new WritableStream({ write(x) { log.push('w:' + x); }, abort(r) { aborted = r.message; } });
const p = rs.pipeTo(ws, { preventAbort: true });
rc.error(new Error('src'));
const res = await p.then(() => 'ok', e => 'rej:' + e.message);
await ws.getWriter().write('b');
return [res, aborted, ...log];
`},
	{name: "PipeToDestErrorCancelsSource", want: `["rej:dest","Error: dest",false]`, body: `
let canceled = 'no', wc;
const rs = new ReadableStream({ cancel(r) { canceled = String(r); } });
const ws = new WritableStream({ start(c) { wc = c; } });
const p = rs.pipeTo(ws);
await null; await null;
wc.error(new Error('dest'));
const res = await p.then(() => 'ok', e => 'rej:' + e.message);
return [res, canceled, rs.locked];
`},
	{name: "PipeToDestErrorPreventCancel", want: `["rej:dest","no",false]`, body: `
let canceled = 'no', wc;
const rs = new ReadableStream({ cancel(r) { canceled = String(r); } });
const ws = new WritableStream({ start(c) { wc = c; } });
const p = rs.pipeTo(ws, { preventCancel: true });
await null; await null;
wc.error(new Error('dest'));
const res = await p.then(() => 'ok', e => 'rej:' + e.message);
return [res, canceled, rs.locked];
`},
	// The dest starts erroring while a write is in flight: the spec waits
	// for the dest to become errored (the write settles first) before it
	// cancels the source and settles the pipe.
	{name: "PipeToDestErroringWaitsInFlightWrite", want: `["write:a","resolve write","cancel:E","rejected:E"]`, body: `
const log = []; let resolveWrite, wc;
const rs = new ReadableStream({ start(c) { c.enqueue('a'); }, cancel(r) { log.push('cancel:' + r.message); } });
const ws = new WritableStream({ start(c) { wc = c; }, write(x) { log.push('write:' + x); return new Promise(r => resolveWrite = r); } });
const p = rs.pipeTo(ws).then(() => log.push('resolved'), e => log.push('rejected:' + e.message));
await new Promise(r => setTimeout(r, 5));
wc.error(new Error('E'));
await new Promise(r => setTimeout(r, 5));
log.push('resolve write');
resolveWrite();
await p;
return log;
`},
	{name: "PipeToPreventClose", want: `["a","b"]`, body: `
const rs = new ReadableStream({ start(c) { c.enqueue('a'); c.close(); } });
const got = [];
const ws = new WritableStream({ write(x) { got.push(x); }, close() { got.push('closed'); } });
await rs.pipeTo(ws, { preventClose: true });
await ws.getWriter().write('b');
return got;
`},
	{name: "PipeToClosesAfterWrites", want: `["w:1","w:2","w:3","close","piped"]`, body: `
const log = [];
const rs = new ReadableStream({ start(c) { c.enqueue(1); c.enqueue(2); c.enqueue(3); c.close(); } });
const ws = new WritableStream({ write(x) { log.push('w:' + x); return new Promise(r => setTimeout(r, 2)); }, close() { log.push('close'); } });
await rs.pipeTo(ws); log.push('piped');
return log;
`},
	{name: "PipeToSignal", want: `["rej:AbortError","AbortError","AbortError"]`, body: `
const ac = new AbortController();
let canceled, aborted;
const rs = new ReadableStream({ cancel(r) { canceled = r.name; } });
const ws = new WritableStream({ abort(r) { aborted = r.name; } });
const p = rs.pipeTo(ws, { signal: ac.signal });
ac.abort();
const res = await p.then(() => 'ok', e => 'rej:' + e.name);
return [res, canceled, aborted];
`},
	{name: "PipeToSignalAlreadyAborted", want: `["rej:why","why","why",false,false]`, body: `
const ac = new AbortController(); ac.abort('why');
let canceled, aborted;
const rs = new ReadableStream({ cancel(r) { canceled = r; } });
const ws = new WritableStream({ abort(r) { aborted = r; } });
const res = await rs.pipeTo(ws, { signal: ac.signal }).then(() => 'ok', e => 'rej:' + e);
return [res, canceled, aborted, rs.locked, ws.locked];
`},
	{name: "PipeToSignalPreventAbortCancel", want: `["rej:why","no","no",false,false]`, body: `
const ac = new AbortController();
let canceled = 'no', aborted = 'no';
const rs = new ReadableStream({ cancel(r) { canceled = r; } });
const ws = new WritableStream({ abort(r) { aborted = r; } });
const p = rs.pipeTo(ws, { signal: ac.signal, preventAbort: true, preventCancel: true });
ac.abort('why');
const res = await p.then(() => 'ok', e => 'rej:' + e);
return [res, canceled, aborted, rs.locked, ws.locked];
`},
	{name: "PipeToDestClosedAtStart", want: `["rej:TypeError","TypeError"]`, body: `
const ws = new WritableStream();
const w = ws.getWriter(); await w.close(); w.releaseLock();
let canceled;
const rs = new ReadableStream({ cancel(r) { canceled = r.name; } });
const res = await rs.pipeTo(ws).then(() => 'ok', e => 'rej:' + e.name);
return [res, canceled];
`},
	{name: "PipeToLocked", want: `["TypeError","TypeError","TypeError",false]`, body: `
const rs = new ReadableStream(); const ws = new WritableStream();
rs.getReader();
const a = await rs.pipeTo(ws).then(() => 'ok', e => e.name);
const rs2 = new ReadableStream(); ws.getWriter();
const b = await rs2.pipeTo(ws).then(() => 'ok', e => e.name);
let c; try { new ReadableStream().pipeThrough({ readable: new ReadableStream(), writable: ws }); c = 'ok'; } catch (e) { c = e.name; }
return [a, b, c, rs2.locked];
`},

	// ---- tee -------------------------------------------------------------------------

	{name: "TeeBothBranches", want: `[[1,2,3,4,5,6,7,8,9,10],[1,2,3,4,5,6,7,8,9,10]]`, body: `
let pulls = 0;
const rs = new ReadableStream({ pull(c) { c.enqueue(++pulls); if (pulls === 10) c.close(); } });
const [a, b] = rs.tee();
const read = async s => { const r = s.getReader(), out = []; for (;;) { const x = await r.read(); if (x.done) return out; out.push(x.value); } };
const out1 = await read(a), out2 = await read(b);
return [out1, out2];
`},
	{name: "TeeUnreadBranchPulls", want: `7`, body: `
let pulls = 0;
const rs = new ReadableStream({ pull(c) { c.enqueue(++pulls); } });
const [a, b] = rs.tee();
const r = a.getReader();
for (let i = 0; i < 5; i++) await r.read();
await new Promise(res => setTimeout(res, 10));
return pulls;
`},
	{name: "TeeCancelBothComposite", want: `[true,["r1","r2"],"undefined"]`, body: `
let reason;
const rs = new ReadableStream({ cancel(r) { reason = r; } });
const [a, b] = rs.tee();
const p1 = a.cancel('r1'); const p2 = b.cancel('r2');
const v = await Promise.all([p1, p2]);
return [Array.isArray(reason), reason, String(v[0])];
`},
	{name: "TeeCancelOneKeepsOther", want: `["none",[1,2]]`, body: `
let reason = 'none';
const rs = new ReadableStream({ start(c) { c.enqueue(1); c.enqueue(2); c.close(); }, cancel(r) { reason = r; } });
const [a, b] = rs.tee();
const pc = a.cancel('r1');
const r = b.getReader(), out = [];
for (;;) { const x = await r.read(); if (x.done) break; out.push(x.value); }
await pc;
return [reason, out];
`},
	{name: "TeeSourceError", want: `["rejected:boom","rejected:boom"]`, body: `
let c; const rs = new ReadableStream({ start(x) { c = x; } });
const [a, b] = rs.tee();
c.error(new Error('boom'));
const out = await Promise.allSettled([a.getReader().read(), b.getReader().closed]);
return out.map(x => x.status + ':' + (x.reason && x.reason.message));
`},
	// A byte stream's tee gives branch2 copies (ReadableByteStreamTee,
	// CloneAsUint8Array).
	{name: "TeeByteStreamSharesChunks", want: `[false,1]`, body: `
const rs = new ReadableStream({ type: 'bytes', start(c) { c.enqueue(new Uint8Array([1, 2, 3])); c.close(); } });
const [a, b] = rs.tee();
const x = await a.getReader().read(); x.value[0] = 99;
const y = await b.getReader().read();
return [x.value === y.value, y.value[0]];
`},

	// ---- locks and states ------------------------------------------------------------

	{name: "LockedAndClosedRules", want: `["TypeError","TypeError","TypeError","TypeError","TypeError"]`, body: `
const out = [];
let c; const rs = new ReadableStream({ start(x) { c = x; } });
rs.getReader();
try { rs.getReader(); } catch (e) { out.push(e.name); }
c.close();
try { c.enqueue(1); } catch (e) { out.push(e.name); }
try { c.close(); } catch (e) { out.push(e.name); }
let c2; new ReadableStream({ start(x) { c2 = x; } }); c2.error(new Error('x'));
try { c2.enqueue(1); } catch (e) { out.push(e.name); }
try { c2.close(); } catch (e) { out.push(e.name); }
return out;
`},
	{name: "ReleaseLockRejectsPendingReads", want: `["TypeError","TypeError","TypeError",false]`, body: `
const rs = new ReadableStream();
const r = rs.getReader();
const ps = [r.read(), r.read()];
r.releaseLock();
const s = await Promise.allSettled(ps);
const closed = await r.closed.then(() => 'ok', e => e.name);
return [...s.map(x => x.status === 'rejected' ? x.reason.name : 'ok'), closed, rs.locked];
`},
	{name: "CloseAfterQueuedDrained", want: `[1,"closed",2,"done"]`, body: `
let c; const rs = new ReadableStream({ start(x) { c = x; } });
const r = rs.getReader(), out = [];
c.enqueue(1); c.enqueue(2); c.close();
r.closed.then(() => out.push('closed'));
out.push((await r.read()).value);
out.push((await r.read()).value);
if ((await r.read()).done) out.push('done');
await null;
return out;
`},
	{name: "CancelResolvesPendingReads", want: `[true,true,"x"]`, body: `
let reason;
const rs = new ReadableStream({ cancel(r) { reason = r; } });
const r = rs.getReader();
const p = r.read();
await r.cancel('x');
const v = await p;
return [v.done, v.value === undefined, reason];
`},
	// A newTarget whose prototype getter throws: the spec creates the
	// object (reading the prototype) before the constructor steps lock the
	// stream.
	{name: "ReaderCtorLocksBeforePrototype", want: `["nt",false,"nt",false]`, body: `
const nt = new Proxy(function () {}, { get(t, k) { if (k === 'prototype') throw new Error('nt'); return t[k]; } });
const rs = new ReadableStream(), ws = new WritableStream();
let e1, e2;
try { Reflect.construct(ReadableStreamDefaultReader, [rs], nt); } catch (e) { e1 = e.message; }
try { Reflect.construct(WritableStreamDefaultWriter, [ws], nt); } catch (e) { e2 = e.message; }
return [e1, rs.locked, e2, ws.locked];
`},

	{name: "CancelWithCloseRequestedCallsSourceCancel", want: `["x",true]`, body: `
let called = 'no';
const rs = new ReadableStream({ start(c) { c.enqueue(1); c.close(); }, cancel(r) { called = r; } });
const r = rs.getReader();
await r.cancel('x');
return [called, (await r.read()).done];
`},
	{name: "WritableAbortSignalReason", want: `["AbortError","AbortError",true,true]`, body: `
let c1, c2;
const ws1 = new WritableStream({ start(c) { c1 = c; c.signal; } });
const ws2 = new WritableStream({ start(c) { c2 = c; }, write() { return new Promise(() => {}); } });
await null;
const w2 = ws2.getWriter(); w2.write('x').catch(() => {});
await null;
ws1.abort();
w2.abort().catch(() => {});
return [c1.signal.reason && c1.signal.reason.name, c2.signal.reason && c2.signal.reason.name, c1.signal.aborted, c2.signal.aborted];
`},

	// ---- TransformStream ----------------------------------------------------------------

	{name: "TransformCancelFromReadable", want: `["stop","rejected:stop"]`, body: `
let cancelReason;
const ts = new TransformStream({ cancel(r) { cancelReason = r; } });
const w = ts.writable.getWriter();
await ts.readable.cancel('stop');
const res = await w.closed.then(() => 'closed', e => 'rejected:' + e);
return [cancelReason, res];
`},
	{name: "TransformThrows", want: `["T","T","T"]`, body: `
const ts = new TransformStream({ transform() { throw new Error('T'); } });
const w = ts.writable.getWriter(); const r = ts.readable.getReader();
const rp = r.read().then(() => 'ok', e => e.message);
const a = await w.write('x').then(() => 'ok', e => e.message);
const b = await rp;
const c = await w.closed.then(() => 'ok', e => e.message);
return [a, b, c];
`},
	{name: "TransformFlushEnqueues", want: `[2,4,"end"]`, body: `
const ts = new TransformStream({ transform(ch, c) { c.enqueue(ch * 2); }, flush(c) { c.enqueue('end'); } });
const out = [];
const rp = (async () => { for await (const x of ts.readable) out.push(x); })();
const w = ts.writable.getWriter(); w.write(1); w.write(2); await w.close(); await rp;
return out;
`},
	{name: "TransformBackpressure", want: `[0,1,-1]`, body: `
let transforms = 0;
const ts = new TransformStream({ transform(ch, c) { transforms++; c.enqueue(ch); } });
const w = ts.writable.getWriter();
w.write(1); w.write(2); w.write(3);
await new Promise(r => setTimeout(r, 10));
const before = transforms;
const r = ts.readable.getReader();
await r.read();
await new Promise(r => setTimeout(r, 10));
return [before, transforms, w.desiredSize];
`},
	{name: "TransformTerminate", want: `[["a"],"TypeError"]`, body: `
const ts = new TransformStream({ transform(ch, c) { c.enqueue(ch); c.terminate(); } });
const w = ts.writable.getWriter();
w.write('a').catch(() => {});
const out = [];
for await (const x of ts.readable) out.push(x);
const e = await w.closed.then(() => 'ok', e => e.name);
return [out, e];
`},
	{name: "TransformWritableAbort", want: `["A","A"]`, body: `
let cr;
const ts = new TransformStream({ cancel(r) { cr = r; } });
const r = ts.readable.getReader();
await ts.writable.abort('A');
const e = await r.closed.then(() => 'ok', e => e);
return [cr, e];
`},

	{name: "TransformCancelWhileWriteWaits", want: `["rej:stop","rej:stop","stop"]`, body: `
let cr;
const ts = new TransformStream({ cancel(r) { cr = r; } });
const w = ts.writable.getWriter();
const wp = w.write('a').then(() => 'ok', e => 'rej:' + e);
await new Promise(r => setTimeout(r, 0));
await ts.readable.cancel('stop');
const a = await wp;
const b = await w.closed.then(() => 'ok', e => 'rej:' + e);
return [a, b, cr];
`},
	{name: "TextEncoderStreamSurrogates", want: `[[240,159,152,128],[97,239,191,189]]`, body: `
const enc = async chunks => {
  const ts = new TextEncoderStream(); const w = ts.writable.getWriter();
  const out = []; const rp = (async () => { for await (const x of ts.readable) out.push(...x); })();
  for (const c of chunks) w.write(c);
  await w.close(); await rp; return out;
};
return [await enc(['\uD83D', '\uDE00']), await enc(['a\uD83D'])];
`},
	{name: "TextDecoderStreamSplit", want: `["€x","TypeError"]`, body: `
const dec = async (chunks, opts) => {
  const ts = new TextDecoderStream('utf-8', opts); const w = ts.writable.getWriter();
  let out = ''; const rp = (async () => { for await (const x of ts.readable) out += x; })();
  for (const c of chunks) w.write(new Uint8Array(c)).catch(() => {});
  await w.close().catch(() => {});
  try { await rp; } catch (e) { return e.name; }
  return out;
};
return [await dec([[0xE2, 0x82], [0xAC, 0x78]]), await dec([[0xE2, 0x82]], { fatal: true })];
`},

	// ---- then interception (WPT piping/then-interception.any.js) --------------------------

	{name: "ThenInterceptionPipe", mutable: true, want: `[[],["a"]]`, body: `
const intercepted = [];
let callCount = 0;
const rs = new ReadableStream({ start(c) { c.enqueue('a'); c.close(); } });
const got = [];
const ws = new WritableStream({ write(x) { got.push(x); } });
Object.prototype.then = function (resolver) {
  if (!this.done) intercepted.push(this.value);
  const retval = Object.create(null);
  retval.done = ++callCount === 3;
  retval.value = callCount;
  resolver(retval);
  if (retval.done) delete Object.prototype.then;
};
try { await rs.pipeTo(ws); } finally { delete Object.prototype.then; }
return [intercepted, got];
`},
	{name: "ThenInterceptionTee", mutable: true, want: `[[],["a"]]`, body: `
const intercepted = [];
let callCount = 0;
const rs = new ReadableStream({ start(c) { c.enqueue('a'); c.close(); } });
const got = [];
const ws = new WritableStream({ write(x) { got.push(x); } });
const [b1, b2] = rs.tee();
Object.prototype.then = function (resolver) {
  if (!this.done) intercepted.push(this.value);
  const retval = Object.create(null);
  retval.done = ++callCount === 3;
  retval.value = callCount;
  resolver(retval);
  if (retval.done) delete Object.prototype.then;
};
try { await b1.pipeTo(ws); } finally { delete Object.prototype.then; }
b2.cancel();
return [intercepted, got];
`},
	// A then getter releases the reader while the stream closes. The
	// reference implementation takes all read requests before running their
	// close steps (all three done); Node.js rejects the rest: want is
	// Node's.
	{name: "ThenGetterReleaseDuringClose", mutable: true, want: `["{\"done\":true}","TypeError","TypeError"]`, body: `
let c; const rs = new ReadableStream({ start(x) { c = x; } });
const r = rs.getReader();
const ps = [r.read(), r.read(), r.read()];
let n = 0;
Object.defineProperty(Object.prototype, 'then', { configurable: true, get() { if (this.done === true && ++n === 1) r.releaseLock(); return undefined; } });
try { c.close(); } finally { delete Object.prototype.then; }
const out = await Promise.allSettled(ps);
return out.map(x => x.status === 'fulfilled' ? JSON.stringify(x.value) : x.reason.name);
`},
	{name: "ThenGetterEnqueueDuringFulfill", mutable: true, want: `["a","b"]`, body: `
let c; const rs = new ReadableStream({ start(x) { c = x; } });
const r = rs.getReader();
const p1 = r.read(), p2 = r.read();
let once = false;
Object.defineProperty(Object.prototype, 'then', { configurable: true, get() { if (!once && this.value === 'a') { once = true; c.enqueue('b'); } return undefined; } });
try { c.enqueue('a'); } finally { delete Object.prototype.then; }
const v = await Promise.all([p1, p2]);
return v.map(x => x.value);
`},

	// ---- ReadableStream.from -------------------------------------------------------------

	{name: "FromAsyncGeneratorCancel", want: `[[1,2],true]`, body: `
let finalized = false;
async function* g() { try { yield 1; yield 2; yield 3; } finally { finalized = true; } }
const out = [];
for await (const x of ReadableStream.from(g())) { out.push(x); if (x === 2) break; }
return [out, finalized];
`},
	{name: "FromString", want: `["a","b"]`, body: `
const out = [];
for await (const x of ReadableStream.from('ab')) out.push(x);
return out;
`},
	{name: "FromSyncIterablePromises", want: `[1,2,3]`, body: `
const out = [];
for await (const x of ReadableStream.from([1, Promise.resolve(2), 3])) out.push(x);
return out;
`},
}

// ---- unhandled rejections ----------------------------------------------------------

// unhandledCases are scenarios whose script handles every promise it gets; the
// tracker counts the promises still rejected without a handler when the
// loop ends (Node.js v22: its 'unhandledRejection' events, want).
var unhandledCases = []streamCase{
	{name: "PipeSourceError", want: `0`, body: `
let c; const rs = new ReadableStream({ start(x) { c = x; } });
const p = rs.pipeTo(new WritableStream()).catch(() => {});
c.error(new Error('e')); await p;
`},
	{name: "PipeDestWriteRejects", want: `0`, body: `
const rs = new ReadableStream({ start(c) { c.enqueue(1); } });
await rs.pipeTo(new WritableStream({ write() { throw new Error('w'); } })).catch(() => {});
`},
	{name: "PipeThroughTransformThrows", want: `0`, body: `
const rs = new ReadableStream({ start(c) { c.enqueue(1); c.enqueue(2); } });
const out = rs.pipeThrough(new TransformStream({ transform() { throw new Error('t'); } }));
await out.getReader().read().catch(() => {});
await new Promise(r => setTimeout(r, 5));
`},
	{name: "TeeSourceError", want: `0`, body: `
let c; const rs = new ReadableStream({ start(x) { c = x; } });
const [a, b] = rs.tee();
const ps = [a.getReader().read().catch(() => {}), b.getReader().read().catch(() => {})];
c.error(new Error('e')); await Promise.all(ps);
`},
	{name: "WriterAbortQueuedWrites", want: `0`, body: `
const ws = new WritableStream({ write() { return new Promise(r => setTimeout(r, 5)); } });
const w = ws.getWriter();
const ps = [w.write(1).catch(() => {}), w.write(2).catch(() => {}), w.close().catch(() => {})];
await w.abort('x'); await Promise.all(ps);
`},
	{name: "SinkWriteRejects", want: `0`, body: `
const w = new WritableStream({ write() { return Promise.reject(new Error('w')); } }).getWriter();
await w.write(1).catch(() => {});
await new Promise(r => setTimeout(r, 5));
`},
	{name: "SourceCancelThrows", want: `0`, body: `
const rs = new ReadableStream({ cancel() { throw new Error('c'); } });
await rs.cancel().catch(() => {});
`},
	{name: "TransformerCancelThrows", want: `0`, body: `
const ts = new TransformStream({ cancel() { throw new Error('c'); } });
await ts.readable.cancel('r').catch(() => {});
await new Promise(r => setTimeout(r, 5));
`},
	{name: "FromDoneGetterThrows", want: `0`, body: `
const it = { [Symbol.asyncIterator]() { return { next() { return Promise.resolve({ get done() { throw new Error('d'); } }); } }; } };
await ReadableStream.from(it).getReader().read().catch(() => {});
await new Promise(r => setTimeout(r, 5));
`},
	{name: "ForAwaitSourceError", want: `0`, body: `
const rs = new ReadableStream({ start(c) { c.enqueue(1); c.error(new Error('e')); } });
try { for await (const x of rs) {} } catch (e) {}
`},
	{name: "PipeSignalAbort", want: `0`, body: `
const ac = new AbortController();
const p = new ReadableStream().pipeTo(new WritableStream(), { signal: ac.signal }).catch(() => {});
ac.abort(); await p;
`},
}

func TestStreamsNoUnhandledRejections(t *testing.T) {
	for _, c := range unhandledCases {
		t.Run(c.name, func(t *testing.T) {
			rt := moejs.NewRuntime(moejs.Options{})
			pending := map[*moejs.Object]string{}
			rt.SetPromiseRejectionTracker(func(p moejs.Value, op moejs.PromiseRejectionOperation) {
				o := p.AsObject()
				if op == moejs.PromiseRejectionReject {
					_, v, _ := moejs.PromiseResult(p)
					pending[o] = v.String()
				} else {
					delete(pending, o)
				}
			})
			h := newHarnessRT(t, rt, webapi.Options{})
			h.run(c.body + "\nreturn 'ok'")
			var reasons []string
			for _, r := range pending {
				reasons = append(reasons, r)
			}
			assert.Equal(t, c.want, fmt.Sprint(len(pending)), "unhandled rejections: %q", reasons)
		})
	}
}

// ---- memory: the readable queue ------------------------------------------------------

// A readable queue that never empties (a high-water mark above 1 and a
// source that keeps it full), and read requests that never drain, stay
// small: 1e6 reads leave the heap where it was.
func TestStreamQueuesCompact(t *testing.T) {
	measure := func() uint64 {
		var m runtime.MemStats
		runtime.GC()
		runtime.GC()
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	for _, c := range []struct{ name, setup, loop string }{
		{"chunks-hwm1", `globalThis.rs = new ReadableStream({ pull(c) { c.enqueue(1); } }, { highWaterMark: 1 });
globalThis.rd = rs.getReader(); await rd.read();`, `for (let i = 0; i < 1000000; i++) await rd.read();`},
		{"chunks-hwm4", `globalThis.rs = new ReadableStream({ pull(c) { c.enqueue(1); } }, { highWaterMark: 4 });
globalThis.rd = rs.getReader(); await rd.read();`, `for (let i = 0; i < 1000000; i++) await rd.read();`},
		// The reader's read requests: a consumer that keeps two reads
		// outstanding never lets rsReader.reqs drain.
		{"two-pending-reads", `let ctrl; globalThis.rs = new ReadableStream({ start(c) { ctrl = c; } }, { highWaterMark: 0 });
globalThis.ctrl = ctrl; globalThis.rd = rs.getReader(); globalThis.ps = [rd.read(), rd.read()];`,
			`for (let i = 0; i < 1000000; i++) { ctrl.enqueue(i); await ps.shift(); ps.push(rd.read()); }`},
	} {
		h := newHarness(t, webapi.Options{})
		h.run(c.setup + ` return 1`)
		before := measure()
		h.run(c.loop + ` return 1`)
		after := measure()
		h.run(`return rs.locked`) // keep the stream alive past the measure
		t.Logf("%s: heap %d KiB -> %d KiB after 1e6 reads (+%d KiB)", c.name, before>>10, after>>10, (int64(after)-int64(before))>>10)
		assert.Less(t, int64(after)-int64(before), int64(4<<20), "%s: dequeued slots are retained", c.name)
	}
}
