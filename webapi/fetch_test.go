package webapi_test

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/eventloop"
	"github.com/MoeForge/moejs/webapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// server is an httptest server with the handlers the fetch tests use, and
// a count of its open connections.
type server struct {
	*httptest.Server
	open     atomic.Int64
	written  atomic.Int64  // bytes /infinite wrote
	canceled chan struct{} // closed when /stall's request context ends
	once     sync.Once
}

func newServer(t *testing.T) *server {
	t.Helper()
	s := &server{canceled: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Add("X-Multi", "a")
		w.Header().Add("X-Multi", "b")
		json.NewEncoder(w).Encode(map[string]any{
			"method": r.Method, "body": string(b), "type": r.Header.Get("Content-Type"),
			"x": r.Header.Get("X-Test"), "accept": r.Header.Get("Accept"), "length": r.ContentLength,
			"te": r.TransferEncoding, "query": r.URL.RawQuery, "host": r.Host,
		})
	})
	mux.HandleFunc("/chunks", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		size, _ := strconv.Atoi(r.URL.Query().Get("size"))
		for i := range n {
			w.Write([]byte(strings.Repeat(string(rune('a'+i%26)), size)))
			w.(http.Flusher).Flush()
		}
	})
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		w.Header().Set("Content-Type", "text/event-stream")
		for i := range n {
			fmt.Fprintf(w, "data: {\"i\":%d,\"text\":\"tök%d\"}\n\n", i, i)
			w.(http.Flusher).Flush()
		}
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	})
	mux.HandleFunc("/stall", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("first"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		s.once.Do(func() { close(s.canceled) })
	})
	mux.HandleFunc("/infinite", func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 32<<10)
		for {
			n, err := w.Write(buf)
			s.written.Add(int64(n))
			if err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	})
	mux.HandleFunc("/reset", func(w http.ResponseWriter, r *http.Request) {
		conn, rw, _ := w.(http.Hijacker).Hijack()
		rw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 1000\r\n\r\npartial")
		rw.Flush()
		time.Sleep(20 * time.Millisecond)
		conn.(*net.TCPConn).SetLinger(0)
		conn.Close()
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/echo?from=redirect", http.StatusFound)
	})
	mux.HandleFunc("/status/", func(w http.ResponseWriter, r *http.Request) {
		code, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/status/"))
		w.WriteHeader(code)
		w.Write([]byte("status body"))
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		if r.URL.Query().Get("cl") != "" {
			w.Header().Set("Content-Length", strconv.Itoa(n))
		}
		buf := bytes.Repeat([]byte("x"), 32<<10)
		for n > 0 {
			k := min(n, len(buf))
			if _, err := w.Write(buf[:k]); err != nil {
				return
			}
			n -= k
		}
	})
	mux.HandleFunc("/huge-length", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.FormatInt(math.MaxInt64, 10))
		w.Write([]byte("x"))
	})
	mux.HandleFunc("/zlib", func(w http.ResponseWriter, r *http.Request) {
		var b bytes.Buffer
		zw := zlib.NewWriter(&b)
		zw.Write([]byte("hello"))
		zw.Close()
		w.Header().Set("Content-Encoding", "deflate")
		w.Write(b.Bytes())
	})
	mux.HandleFunc("/deflate", func(w http.ResponseWriter, r *http.Request) {
		var b bytes.Buffer
		fw, _ := flate.NewWriter(&b, flate.DefaultCompression)
		fw.Write([]byte("hello"))
		fw.Close()
		w.Header().Set("Content-Encoding", "deflate")
		w.Write(b.Bytes())
	})
	mux.HandleFunc("/gzip-bomb", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		zero := make([]byte, 64<<10)
		for ; n > 0; n -= len(zero) {
			if _, err := zw.Write(zero[:min(n, len(zero))]); err != nil {
				return
			}
		}
		zw.Close()
	})
	mux.HandleFunc("/gzip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		// "hello" gzipped.
		w.Write([]byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 0xff, 0xca, 0x48, 0xcd, 0xc9, 0xc9, 0x07, 4, 0, 0, 0xff, 0xff, 0x86, 0xa6, 0x10, 0x36, 5, 0, 0, 0})
	})
	s.Server = httptest.NewUnstartedServer(mux)
	s.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		switch st {
		case http.StateNew:
			s.open.Add(1)
		case http.StateClosed, http.StateHijacked:
			s.open.Add(-1)
		}
	}
	s.Start()
	t.Cleanup(s.Close)
	return s
}

// fetchHarness is a harness whose fetch reaches srv through its own
// transport, checked for leaked goroutines and connections at the end.
func fetchHarness(t *testing.T, srv *server, opts webapi.Options) *harness {
	t.Helper()
	tr := &http.Transport{}
	if opts.Client == nil {
		opts.Client = &http.Client{Transport: tr}
	}
	noLeaks(t, tr)
	t.Cleanup(func() {
		tr.CloseIdleConnections()
		deadline := time.Now().Add(5 * time.Second)
		for srv.open.Load() > 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		assert.Zero(t, srv.open.Load(), "server connections still open")
	})
	h := newHarness(t, opts)
	require.NoError(t, h.rt.SetGlobal("BASE", srv.URL))
	return h
}

func TestFetch(t *testing.T) {
	srv := newServer(t)
	cases := []jsCase{
		{"json", `const r = await fetch(BASE + "/echo?a=1", { method: "POST", body: JSON.stringify({ x: 1 }), headers: { "content-type": "application/json", "X-Test": "t" } });
const j = await r.json();
return [r.status, r.ok, r.statusText, r.headers.get("content-type"), r.headers.get("x-multi"), r.url === BASE + "/echo?a=1", r.redirected, r.type, j.method, j.body, j.type, j.x, j.accept, j.length, j.query]`,
			`[200,true,"OK","application/json","a, b",true,false,"basic","POST","{\"x\":1}","application/json","t","*/*",7,"a=1"]`},
		{"text and headers iteration", `const r = await fetch(new URL("/echo", BASE)); const keys = [...r.headers.keys()];
return [keys.includes("content-type"), keys.includes("x-multi"), (await r.text()).startsWith("{")]`, `[true,true,true]`},
		{"request object", `const req = new Request(BASE + "/echo", { method: "PUT", body: "b", headers: { "x-test": "r" } });
const j = await (await fetch(req)).json(); return [j.method, j.body, j.x, req.bodyUsed]`, `["PUT","b","r",true]`},
		{"uint8array body", `const j = await (await fetch(BASE + "/echo", { method: "POST", body: new Uint8Array([104, 105]) })).json();
return [j.body, j.type, j.length]`, `["hi","",2]`},
		{"form body", `const j = await (await fetch(BASE + "/echo", { method: "POST", body: new URLSearchParams("a=1&b=2") })).json();
return [j.body, j.type]`, `["a=1&b=2","application/x-www-form-urlencoded;charset=UTF-8"]`},
		{"stream body", `let i = 0; const body = new ReadableStream({ pull(c) { if (i < 3) c.enqueue(new TextEncoder().encode("p" + i++)); else c.close(); } });
const j = await (await fetch(BASE + "/echo", { method: "POST", body, duplex: "half" })).json();
return [j.body, j.length, j.te]`, `["p0p1p2",-1,["chunked"]]`},
		{"host header", `const j = await (await fetch(BASE + "/echo", { headers: { host: "other.example" } })).json(); return j.host`, `other.example`},
		{"chunks getReader", `const r = await fetch(BASE + "/chunks?n=5&size=1000"); const rd = r.body.getReader(); let n = 0, chunks = 0;
for (;;) { const { done, value } = await rd.read(); if (done) break; if (!(value instanceof Uint8Array)) throw new Error("chunk"); n += value.length; chunks++; }
return [n, chunks > 0, r.bodyUsed]`, `[5000,true,true]`},
		{"for await body", `let n = 0; for await (const c of (await fetch(BASE + "/chunks?n=4&size=100")).body) n += c.length; return n`, `400`},
		{"sse", `const r = await fetch(BASE + "/sse?n=50"); let buf = "", events = [];
for await (const s of r.body.pipeThrough(new TextDecoderStream())) {
  buf += s; let i; while ((i = buf.indexOf("\n\n")) >= 0) { events.push(JSON.parse(buf.slice(6, i))); buf = buf.slice(i + 2); }
}
return [events.length, events[49].text]`, `[50,"tök49"]`},
		{"status 500", `const r = await fetch(BASE + "/status/500"); return [r.status, r.ok, await r.text()]`, `[500,false,"status body"]`},
		{"204 null body", `const r = await fetch(BASE + "/status/204"); return [r.body, await r.text()]`, `[null,""]`},
		{"HEAD", `const r = await fetch(BASE + "/echo", { method: "HEAD" }); return [r.status, r.body]`, `[200,null]`},
		{"redirect follow", `const r = await fetch(BASE + "/redirect"); const j = await r.json(); return [r.redirected, r.url === BASE + "/echo?from=redirect", j.query]`,
			`[true,true,"from=redirect"]`},
		{"redirect manual", `const r = await fetch(BASE + "/redirect", { redirect: "manual" }); await r.text(); return [r.status, r.headers.get("location")]`,
			`[302,"/echo?from=redirect"]`},
		{"redirect error", `try { await fetch(BASE + "/redirect", { redirect: "error" }) } catch (e) { return [e.name, e.message] }`,
			`["TypeError","fetch failed"]`},
		{"gzip with accept-encoding", `return await (await fetch(BASE + "/gzip", { headers: { "accept-encoding": "gzip" } })).text()`, `hello`},
		{"cancel body", `const r = await fetch(BASE + "/chunks?n=1000&size=1000"); const rd = r.body.getReader(); await rd.read(); await rd.cancel(); return "ok"`, `ok`},
		{"clone", `const r = await fetch(BASE + "/chunks?n=3&size=10"); const c = r.clone(); return [(await r.text()).length, (await c.text()).length]`, `[30,30]`},
		{"unread body", `await fetch(BASE + "/chunks?n=3&size=10"); return "ok"`, `ok`},
		{"bad scheme", `try { await fetch("ftp://a.example/") } catch (e) { return [e.name, e.cause.message] }`,
			`["TypeError","webapi: the URL scheme ftp is not supported"]`},
		{"invalid url", `try { await fetch("nope") } catch (e) { return e.name }`, `TypeError`},
		{"reset", `const r = await fetch(BASE + "/reset"); const rd = r.body.getReader(); const a = await rd.read();
try { await rd.read() } catch (e) { return [new TextDecoder().decode(a.value), e.name, e.message, typeof e.cause.message] }`,
			`["partial","TypeError","terminated","string"]`},
		{"reset text", `const r = await fetch(BASE + "/reset"); try { await r.text() } catch (e) { return [e.name, e.message] }`,
			`["TypeError","terminated"]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := fetchHarness(t, srv, webapi.Options{})
			got, err := h.try(c.src)
			if err != nil {
				got = "REJECTED " + err.Error()
			}
			assert.Equal(t, c.want, got)
		})
	}
}

func TestFetchConnectionRefused(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	l.Close()
	tr := &http.Transport{}
	noLeaks(t, tr)
	h := newHarness(t, webapi.Options{Client: &http.Client{Transport: tr}})
	got := h.run(`try { await fetch("http://` + addr + `/") } catch (e) { return [e.name, e.message, e.cause.code] }`)
	assert.Equal(t, `["TypeError","fetch failed","ECONNREFUSED"]`, got)
}

func TestFetchPermissions(t *testing.T) {
	srv := newServer(t)
	h := newHarness(t, webapi.Options{})
	require.NoError(t, h.rt.SetGlobal("BASE", srv.URL))
	got := h.run(`try { await fetch(BASE + "/echo") } catch (e) { return [e.name, e.message, e.cause.message] }`)
	assert.Equal(t, `["TypeError","fetch failed","webapi: fetch has no network (Options.Client is nil)"]`, got)

	h = fetchHarness(t, srv, webapi.Options{Allow: webapi.AllowHosts("allowed.example", "*.ok.example")})
	got = h.run(`try { await fetch(BASE + "/echo") } catch (e) { return [e.name, e.cause.message] }`)
	assert.Equal(t, `["TypeError","webapi: host 127.0.0.1 is not allowed"]`, got)

	// A redirect is checked too.
	var seen []string
	h = fetchHarness(t, srv, webapi.Options{Allow: func(r *http.Request) error {
		seen = append(seen, r.URL.Path)
		if r.URL.Path == "/echo" {
			return errors.New("no echo")
		}
		return nil
	}})
	got = h.run(`try { await fetch(BASE + "/redirect") } catch (e) { return e.cause.message.includes("no echo") }`)
	assert.Equal(t, `true`, got)
	assert.Equal(t, []string{"/redirect", "/echo"}, seen)
}

func TestFetchAbort(t *testing.T) {
	srv := newServer(t)
	cases := []jsCase{
		{"before headers", `const ac = new AbortController(); const p = fetch(BASE + "/slow", { signal: ac.signal });
setTimeout(() => ac.abort(), 20); try { await p } catch (e) { return [e.name, e instanceof DOMException] }`, `["AbortError",true]`},
		{"already aborted", `try { await fetch(BASE + "/echo", { signal: AbortSignal.abort("x") }) } catch (e) { return e }`, `x`},
		{"timeout", `try { await fetch(BASE + "/slow", { signal: AbortSignal.timeout(20) }) } catch (e) { return e.name }`, `TimeoutError`},
		{"mid body", `const ac = new AbortController(); const r = await fetch(BASE + "/stall", { signal: ac.signal });
const rd = r.body.getReader(); const a = await rd.read(); const p = rd.read(); setTimeout(() => ac.abort(), 10);
try { await p } catch (e) { return [new TextDecoder().decode(a.value), e.name] }`, `["first","AbortError"]`},
		{"mid body text", `const ac = new AbortController(); const r = await fetch(BASE + "/stall", { signal: ac.signal });
const p = r.text(); setTimeout(() => ac.abort(new Error("why")), 10); try { await p } catch (e) { return e.message }`, `why`},
		{"after headers before body", `const ac = new AbortController(); const r = await fetch(BASE + "/stall", { signal: ac.signal });
ac.abort(); try { await r.body.getReader().read() } catch (e) { return e.name }`, `AbortError`},
		{"timeout mid body", `const r = await fetch(BASE + "/stall", { signal: AbortSignal.timeout(30) }); const rd = r.body.getReader(); await rd.read();
try { await rd.read() } catch (e) { return e.name }`, `TimeoutError`},
		{"abort after end", `const ac = new AbortController(); const r = await fetch(BASE + "/echo", { signal: ac.signal }); await r.text(); ac.abort(); return "ok"`, `ok`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := fetchHarness(t, srv, webapi.Options{})
			got, err := h.try(c.src)
			if err != nil {
				got = "REJECTED " + err.Error()
			}
			assert.Equal(t, c.want, got)
		})
	}
}

// TestFetchCancelReachesServer checks that reader.cancel() cancels the Go
// request: the server sees its request context end.
func TestFetchCancelReachesServer(t *testing.T) {
	srv := newServer(t)
	h := fetchHarness(t, srv, webapi.Options{})
	h.run(`const r = await fetch(BASE + "/stall"); const rd = r.body.getReader(); await rd.read(); await rd.cancel(); return 1`)
	select {
	case <-srv.canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("the server's request was not canceled")
	}
}

// TestFetchBackpressure checks that a slow reader holds the server back:
// Go reads at most one chunk ahead of what JavaScript took, so the server
// blocks once the socket buffers are full.
func TestFetchBackpressure(t *testing.T) {
	srv := newServer(t)
	h := fetchHarness(t, srv, webapi.Options{})
	require.NoError(t, h.rt.SetGlobal("written", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		return moejs.Number(float64(srv.written.Load())), nil
	})))
	got := h.run(`const r = await fetch(BASE + "/infinite"); const rd = r.body.getReader(); let read = 0;
for (let i = 0; i < 4; i++) read += (await rd.read()).value.length;
await new Promise(r => setTimeout(r, 300)); const a = written();
await new Promise(r => setTimeout(r, 300)); const b = written();
await rd.cancel(); return [read, a, b]`)
	var v [3]float64
	require.NoError(t, json.Unmarshal([]byte(got), &v))
	t.Logf("read %v bytes; server wrote %v then %v", v[0], v[1], v[2])
	assert.Equal(t, v[1], v[2], "the server kept writing while JavaScript did not read")
	assert.Less(t, v[1], float64(64<<20), "the Go side read far ahead")
}

// TestFetchPartlyReadBodyCollected checks that a body whose stream was
// read, then dropped, is closed once it is collected, whether the
// goroutine reading it waits in a read or holds the chunk it read ahead.
func TestFetchPartlyReadBodyCollected(t *testing.T) {
	for _, path := range []string{"/stall", "/infinite"} {
		t.Run(path, func(t *testing.T) {
			srv := newServer(t)
			h := fetchHarness(t, srv, webapi.Options{})
			h.run(`const rd = (await fetch(BASE + "` + path + `")).body.getReader(); await rd.read();
await new Promise(r => setTimeout(r, 50)); return 1`)
			h.run(`let x = 0; for (let i = 0; i < 1000; i++) x += [i].length; return x`)
			deadline := time.Now().Add(10 * time.Second)
			for srv.open.Load() > 0 {
				if time.Now().After(deadline) {
					t.Fatal("the partly read body was not closed after it was collected")
				}
				runtime.GC()
				time.Sleep(50 * time.Millisecond)
			}
		})
	}
}

// readCounter counts the Read calls of a response body as they start.
type readCounter struct {
	io.ReadCloser
	n *atomic.Int64
}

func (r readCounter) Read(p []byte) (int, error) {
	r.n.Add(1)
	return r.ReadCloser.Read(p)
}

type countingTransport struct {
	http.RoundTripper
	reads atomic.Int64
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.RoundTripper.RoundTrip(req)
	if err == nil {
		resp.Body = readCounter{resp.Body, &t.reads}
	}
	return resp, err
}

// TestFetchReadAheadReported checks that the chunk Go reads ahead counts in
// the measurements of the memory limit while JavaScript reaches the
// stream, though only Go holds it, and no longer once the stream is
// canceled. The goroutine's buffer counts from before the read ahead
// starts, which the measurement waits for.
func TestFetchReadAheadReported(t *testing.T) {
	srv := newServer(t)
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	ct := &countingTransport{RoundTripper: tr}
	h := newHarnessRT(t, moejs.NewRuntime(moejs.Options{MemoryLimit: 64 << 20}), webapi.Options{Client: &http.Client{Transport: ct}})
	require.NoError(t, h.rt.SetGlobal("BASE", srv.URL))
	var kept, dropped int64
	require.NoError(t, h.rt.SetGlobal("measure", moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		if moejs.Arg(args, 0).String() == "kept" {
			// Eight reads handed their chunks over; the ninth reads ahead.
			deadline := time.Now().Add(10 * time.Second)
			for ct.reads.Load() < 9 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			kept = h.rt.Realm().MeasureLive(nil)
		} else {
			dropped = h.rt.Realm().MeasureLive(nil)
		}
		return moejs.Undefined(), nil
	})))
	got := h.run(`const rd = (await fetch(BASE + "/infinite")).body.getReader(); let max = 0;
for (let i = 0; i < 8; i++) max = Math.max(max, (await rd.read()).value.buffer.byteLength);
measure("kept"); await rd.cancel(); measure("dropped"); return max`)
	t.Logf("largest chunk %s; %d reads; measured %d with the reader, %d after its cancel", got, ct.reads.Load(), kept, dropped)
	max, err := strconv.Atoi(got)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, kept-dropped, int64(max), "the chunk read ahead")
}

// TestFetchEndedReaderNotReported checks that a stream read to its end
// stops reporting the goroutine's buffer: bodies kept after their end
// count little more than their objects.
func TestFetchEndedReaderNotReported(t *testing.T) {
	srv := newServer(t)
	h := limitHarness(t, srv, 1<<30)
	var m [2]int64
	require.NoError(t, h.rt.SetGlobal("measure", moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		i, _ := strconv.Atoi(moejs.Arg(args, 0).String())
		runtime.GC()
		m[i] = h.rt.Realm().MeasureLive(nil)
		return moejs.Undefined(), nil
	})))
	h.run(`globalThis.kept = []; measure(0);
for (let i = 0; i < 64; i++) { const r = await fetch(BASE + "/chunks?n=1&size=100"); const rd = r.body.getReader(); while (!(await rd.read()).done); kept.push(r, rd) }
measure(1); return 1`)
	t.Logf("64 streamed bodies of 100 bytes, read to their end and kept: measured %d bytes more (%d per body)", m[1]-m[0], (m[1]-m[0])/64)
	assert.Less(t, m[1]-m[0], int64(64*4<<10), "the measurements count the read buffers of ended bodies")
}

// TestFetchClose checks that Close ends fetches in flight and open bodies.
func TestFetchClose(t *testing.T) {
	srv := newServer(t)
	h := fetchHarness(t, srv, webapi.Options{})
	require.NoError(t, h.rt.SetGlobal("closeAPI", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		go h.api.Close()
		return moejs.Undefined(), nil
	})))
	got := h.run(`const r = await fetch(BASE + "/stall"); const rd = r.body.getReader(); await rd.read();
const p = rd.read(); const q = fetch(BASE + "/slow"); closeAPI();
const res = await Promise.allSettled([p, q]);
let after; try { await fetch(BASE + "/echo") } catch (e) { after = e.cause.message }
return [...res.map(x => x.reason.message), after]`)
	assert.Equal(t, `["terminated","fetch failed","webapi: closed"]`, got)
}

// TestIssueSnippets runs the two snippets of the feature request unchanged.
func TestIssueSnippets(t *testing.T) {
	const readReply = `async function readReply(fetch, url, signal, onText) {
  const response = await fetch(url, {
    method: "POST",
    headers: new Headers({ "content-type": "application/json" }),
    body: JSON.stringify({ stream: true }),
    signal,
  });
  if (!response.ok || !response.body) throw new Error("Request failed");
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let finished = false;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) { finished = true; break; }
      onText(decoder.decode(value, { stream: true }));
    }
    onText(decoder.decode());
  } finally {
    try {
      if (!finished) await reader.cancel();
    } finally {
      reader.releaseLock();
    }
  }
}
`
	srv := newServer(t)
	t.Run("stream", func(t *testing.T) {
		h := fetchHarness(t, srv, webapi.Options{})
		got := h.run(readReply + `let text = ""; await readReply(fetch, BASE + "/sse?n=100", undefined, s => text += s);
return [text.split("\n\n").length - 1, text.includes("tök99")]`)
		assert.Equal(t, `[100,true]`, got)
	})
	t.Run("abort", func(t *testing.T) {
		h := fetchHarness(t, srv, webapi.Options{})
		got := h.run(readReply + `const ac = new AbortController(); let text = "";
try { await readReply(fetch, BASE + "/stall", ac.signal, s => { text += s; setTimeout(() => ac.abort(), 5); }) }
catch (e) { return [text, e.name] }`)
		assert.Equal(t, `["first","AbortError"]`, got)
	})
	t.Run("cancel on consumer error", func(t *testing.T) {
		srv := newServer(t)
		h := fetchHarness(t, srv, webapi.Options{})
		got := h.run(readReply + `try { await readReply(fetch, BASE + "/stall", undefined, () => { throw new Error("consumer") }) }
catch (e) { return e.message }`)
		assert.Equal(t, `consumer`, got)
		select {
		case <-srv.canceled:
		case <-time.After(5 * time.Second):
			t.Fatal("reader.cancel() did not cancel the request")
		}
	})
	t.Run("globals", func(t *testing.T) {
		h := newHarness(t, webapi.Options{})
		got := h.run(`return JSON.stringify(Object.fromEntries([
  "fetch", "Headers", "Request", "Response",
  "ReadableStream", "TransformStream",
  "AbortController", "AbortSignal", "URL", "URLSearchParams", "performance"
].map(name => [name, typeof globalThis[name]])));`)
		assert.NotContains(t, got, "undefined")
	})
}

// TestFetchServerSentEvents reads many small events through a reader and
// checks each arrives whole.
func TestFetchServerSentEvents(t *testing.T) {
	srv := newServer(t)
	h := fetchHarness(t, srv, webapi.Options{})
	got := h.run(`const r = await fetch(BASE + "/sse?n=2000"); const rd = r.body.getReader(); const dec = new TextDecoder();
let buf = "", n = 0, sum = 0;
for (;;) { const { done, value } = await rd.read(); if (done) break; buf += dec.decode(value, { stream: true });
  let i; while ((i = buf.indexOf("\n\n")) >= 0) { sum += JSON.parse(buf.slice(6, i)).i; n++; buf = buf.slice(i + 2); } }
return [n, sum]`)
	assert.Equal(t, `[2000,1999000]`, got)
}

// TestFetchUnreadBodyCollected checks the garbage collector's backstop: a
// response whose body nobody reads or cancels is closed once JavaScript no
// longer holds it.
func TestFetchUnreadBodyCollected(t *testing.T) {
	srv := newServer(t)
	h := fetchHarness(t, srv, webapi.Options{})
	h.run(`await fetch(BASE + "/stall"); return 1`)
	// Other code overwrites what the finished one left in the VM's
	// registers.
	h.run(`let x = 0; for (let i = 0; i < 1000; i++) x += [i].length; return x`)
	deadline := time.Now().Add(10 * time.Second)
	for {
		runtime.GC()
		select {
		case <-srv.canceled:
			return
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("the unread body was not closed after it was collected")
		}
	}
}

// TestFetchBodyLockedAfterText checks that a body text() reads is used and
// locked: its stream is disturbed and locked, and cannot be read again.
func TestFetchBodyLockedAfterText(t *testing.T) {
	srv := newServer(t)
	h := fetchHarness(t, srv, webapi.Options{})
	got := h.run(`const r = await fetch(BASE + "/big?n=100");
const t = r.text();
const used1 = r.bodyUsed;
const s = r.body;
const out = [used1, s.locked, r.bodyUsed];
try { s.getReader(); out.push("getReader ok") } catch (e) { out.push(e.name) }
try { r.clone(); out.push("clone ok") } catch (e) { out.push(e.name) }
try { await r.text(); out.push("text ok") } catch (e) { out.push(e.name) }
out.push((await t).length);
return out`)
	assert.Equal(t, `[true,true,true,"TypeError","TypeError","TypeError",100]`, got)
	got = h.run(`const r = new Response("abc"); await r.text();
return [r.bodyUsed, r.body.locked, await r.body.cancel().then(() => "canceled", e => e.name)]`)
	assert.Equal(t, `[true,true,"TypeError"]`, got)
}

// TestFetchBodyReadOnce checks that a body is read by one consumer: the
// stream of a body text() reads cannot be read alongside it.
func TestFetchBodyReadOnce(t *testing.T) {
	srv := newServer(t)
	h := fetchHarness(t, srv, webapi.Options{})
	got := h.run(`const N = 8 << 20;
const r = await fetch(BASE + "/big?n=" + N);
const t = r.text();
let streamed = 0;
try {
  const rd = r.body.getReader();
  for (;;) { const { done, value } = await rd.read(); if (done) break; streamed += value.length }
} catch (e) { streamed = e.name }
let textLen; try { textLen = (await t).length } catch (e) { textLen = e.message }
return [textLen, streamed]`)
	assert.Equal(t, `[8388608,"TypeError"]`, got)
}

// bytesTransport serves a fixed body from a bytes.Reader, which is not
// safe for concurrent Reads, as an in-process transport may.
type bytesTransport struct{ n int }

func (b bytesTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{}, ContentLength: -1, Request: req,
		Body: io.NopCloser(&slowReader{r: bytes.NewReader(bytes.Repeat([]byte("x"), b.n))})}, nil
}

type slowReader struct{ r *bytes.Reader }

func (s *slowReader) Read(p []byte) (int, error) {
	if len(p) > 4096 {
		p = p[:4096]
	}
	return s.r.Read(p)
}

// TestFetchBodyReadOnceCustomTransport is TestFetchBodyReadOnce with a
// body that is not safe for concurrent use, for -race.
func TestFetchBodyReadOnceCustomTransport(t *testing.T) {
	h := newHarness(t, webapi.Options{Client: &http.Client{Transport: bytesTransport{8 << 20}}})
	got := h.run(`const r = await fetch("http://mem/x"); const t = r.text();
let streamed = 0; try { const rd = r.body.getReader();
for (;;) { const { done, value } = await rd.read(); if (done) break; streamed += value.length } } catch (e) { streamed = e.name }
let textLen; try { textLen = (await t).length } catch (e) { textLen = e.message }
return [textLen, streamed]`)
	assert.Equal(t, `[8388608,"TypeError"]`, got)
}

// limitHarness is a harness whose runtime has a memory limit of limit
// bytes and whose fetch reaches srv.
func limitHarness(t *testing.T, srv *server, limit int64) *harness {
	return limitHarnessLoop(t, srv, limit, eventloop.Options{})
}

// limitHarnessLoop is limitHarness with the loop's options lopts.
func limitHarnessLoop(t *testing.T, srv *server, limit int64, lopts eventloop.Options) *harness {
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
	h := newHarnessLoop(t, rt, lopts, webapi.Options{Client: &http.Client{Transport: tr}})
	require.NoError(t, h.rt.SetGlobal("BASE", srv.URL))
	return h
}

// allocated returns the bytes the process allocated while f ran.
func allocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestFetchTextMemoryLimit checks that a body read whole is charged to the
// runtime's memory limit: one larger than the limit is never taken, the
// read rejects with a RangeError, as a builtin that knows its result does
// not fit throws, and Go reads little more of it than the limit.
func TestFetchTextMemoryLimit(t *testing.T) {
	srv := newServer(t)
	for _, c := range []struct{ name, url, init string }{
		{"identity", `"/big?n=" + (128 << 20)`, `{}`},
		{"gzip", `"/gzip-bomb?n=" + (128 << 20)`, `{}`},
		{"gzip-set-by-script", `"/gzip-bomb?n=" + (128 << 20)`, `{ headers: { "accept-encoding": "gzip" } }`},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := limitHarness(t, srv, 16<<20)
			ctrl, err := h.runErr(`try { "x".repeat(128 << 20); return "allocated" } catch (e) { return e.name }`)
			require.NoError(t, err)
			require.Equal(t, "RangeError", ctrl)
			var got string
			n := allocated(func() {
				got, err = h.runErr(`const r = await fetch(BASE + ` + c.url + `, ` + c.init + `);
globalThis.keep = await r.text(); return keep.length`)
			})
			assert.NoError(t, err)
			assert.Equal(t, tooLarge, got)
			assert.Less(t, n, uint64(80<<20), "allocated %d MiB", n>>20)
		})
	}
	h := limitHarness(t, srv, 16<<20)
	assert.Equal(t, "4194304", h.run(`return (await (await fetch(BASE + "/big?n=" + (4 << 20))).text()).length`))
}

// TestFetchStreamBodyMemoryLimit checks a whole read of a stream without
// end, whose chunks the script allocates: its copy, which the read reports
// to the measurements, is never taken past the limit. The chunk and the
// copy so far pass the limit before the read sees the chunk, so the
// script's own allocation may stop the loop first; otherwise the read
// rejects with a RangeError.
func TestFetchStreamBodyMemoryLimit(t *testing.T) {
	srv := newServer(t)
	h := limitHarness(t, srv, 16<<20)
	got, err := h.runErr(`const r = new Response(new ReadableStream({ pull(c) { c.enqueue(new Uint8Array(1 << 20)) } }));
try { await r.arrayBuffer(); return "read" } catch (e) { return e.name + ": " + e.message }`)
	if err != nil {
		assert.ErrorIs(t, err, moejs.ErrMemoryLimit, "result %q", got)
	} else {
		assert.True(t, strings.HasPrefix(got, "RangeError: "), got)
	}
}

// TestFetchTooLarge checks what a whole read of a body larger than the
// limit gets, three times over, announced or not: the RangeError, never
// the loop's stop.
func TestFetchTooLarge(t *testing.T) {
	srv := newServer(t)
	for _, c := range []struct{ name, url string }{
		{"identity 8x", `"/big?n=" + (128 << 20)`},
		{"identity L+1MiB", `"/big?n=" + (17 << 20)`},
		{"identity L+1MiB announced", `"/big?cl=1&n=" + (17 << 20)`},
		{"identity L+1", `"/big?n=" + ((16 << 20) + 1)`},
		{"gzip 8x", `"/gzip-bomb?n=" + (128 << 20)`},
	} {
		for i := 0; i < 3; i++ {
			h := limitHarness(t, srv, 16<<20)
			got, err := h.runErr(`return (await (await fetch(BASE + ` + c.url + `)).text()).length`)
			assert.NoError(t, err, "%s #%d", c.name, i)
			assert.Equal(t, tooLarge, got, "%s #%d", c.name, i)
		}
	}
}

// TestFetchBodyChunksCharged checks that the chunks of a streamed body are
// charged to the memory limit, each in the task that hands it over: a
// stream larger than the limit is read, and counted.
func TestFetchBodyChunksCharged(t *testing.T) {
	srv := newServer(t)
	h := limitHarness(t, srv, 16<<20)
	before := h.rt.Stats().AllocatedBytes
	got := h.run(`const r = await fetch(BASE + "/big?n=" + (64 << 20)); let n = 0;
for await (const c of r.body) n += c.length; return n`)
	assert.Equal(t, strconv.Itoa(64<<20), got)
	assert.GreaterOrEqual(t, h.rt.Stats().AllocatedBytes-before, int64(64<<20))
}

// TestFetchTextPreallocBounded checks that a whole-body read does not
// allocate a large announced Content-Length before the bytes arrive.
func TestFetchTextPreallocBounded(t *testing.T) {
	srv := newServer(t)
	mux := srv.Config.Handler.(*http.ServeMux)
	mux.HandleFunc("/liar", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(60<<20))
		w.Write([]byte("x"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	h := fetchHarness(t, srv, webapi.Options{})
	var m runtime.MemStats
	require.NoError(t, h.rt.SetGlobal("heapMiB", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		runtime.GC()
		runtime.ReadMemStats(&m)
		return moejs.Number(float64(m.HeapAlloc >> 20)), nil
	})))
	got := h.run(`const before = heapMiB(); const ac = new AbortController();
const rs = await Promise.all(Array.from({ length: 10 }, () => fetch(BASE + "/liar", { signal: ac.signal })));
const ps = rs.map(r => r.text().catch(() => {}));
await new Promise(r => setTimeout(r, 200));
const during = heapMiB(); ac.abort(); await Promise.all(ps); return during - before`)
	n, err := strconv.Atoi(got)
	require.NoError(t, err)
	assert.Less(t, n, 40, "10 text() of 1-byte bodies announcing 60 MiB took %d MiB", n)
}

// TestAllowHosts checks the matching of AllowHosts: names, subdomains,
// "*" for every host, and the Host header a script sets.
func TestAllowHosts(t *testing.T) {
	check := func(allow func(*http.Request) error, url, host string) bool {
		req, err := http.NewRequest("GET", url, nil)
		require.NoError(t, err)
		if host != "" {
			req.Host = host
		}
		return allow(req) == nil
	}
	sub := webapi.AllowHosts("API.example.com", "*.example.org")
	assert.True(t, check(sub, "https://api.example.com/x", ""))
	assert.False(t, check(sub, "https://example.com/x", ""))
	assert.True(t, check(sub, "https://a.b.example.org/x", ""))
	assert.False(t, check(sub, "https://example.org/x", ""))
	assert.True(t, check(sub, "https://api.example.com/x", "api.example.com:443"))
	assert.False(t, check(sub, "https://api.example.com/x", "admin.internal"))
	assert.True(t, check(sub, "https://api.example.com/x", "x.example.org"))
	all := webapi.AllowHosts("*")
	for _, u := range []string{"http://127.0.0.1/", "http://169.254.169.254/latest", "https://[::1]:8443/", "http://intranet/"} {
		assert.True(t, check(all, u, "anything"), u)
	}
	assert.False(t, check(webapi.AllowHosts(), "http://127.0.0.1/", ""))
}

// TestAllowHostsHostHeader checks that a Host header a script sets must be
// allowed too: an allowed address does not reach another virtual host.
func TestAllowHostsHostHeader(t *testing.T) {
	srv := newServer(t)
	h := fetchHarness(t, srv, webapi.Options{Allow: webapi.AllowHosts("127.0.0.1")})
	got := h.run(`try { await fetch(BASE + "/echo", { headers: { Host: "admin.internal" } }); return "sent" }
catch (e) { return e.name + ": " + e.cause.message }`)
	assert.Equal(t, "TypeError: webapi: host admin.internal (the Host header) is not allowed", got)
	h = fetchHarness(t, srv, webapi.Options{Allow: webapi.AllowHosts("127.0.0.1", "admin.internal")})
	assert.Equal(t, "admin.internal", h.run(`return (await (await fetch(BASE + "/echo", { headers: { Host: "admin.internal" } })).json()).host`))
}

// TestAllowHostsRedirect checks that Allow approves each redirect.
func TestAllowHostsRedirect(t *testing.T) {
	srv := newServer(t)
	mux := srv.Config.Handler.(*http.ServeMux)
	mux.HandleFunc("/to-localhost", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)+"/echo", http.StatusFound)
	})
	h := fetchHarness(t, srv, webapi.Options{Allow: webapi.AllowHosts("127.0.0.1")})
	got := h.run(`try { await fetch(BASE + "/to-localhost"); return "followed" } catch (e) { return e.cause.message }`)
	assert.Contains(t, got, "not allowed")
}

// TestFetchUnreadBodyCollectedWithSignal is TestFetchUnreadBodyCollected
// for a fetch whose AbortSignal lives on: the signal does not keep the
// response.
func TestFetchUnreadBodyCollectedWithSignal(t *testing.T) {
	srv := newServer(t)
	h := fetchHarness(t, srv, webapi.Options{})
	h.run(`globalThis.ac = new AbortController(); await fetch(BASE + "/stall", { signal: ac.signal }); return 1`)
	h.run(`let x = 0; for (let i = 0; i < 1000; i++) x += [i].length; return x`)
	deadline := time.Now().Add(10 * time.Second)
	for {
		runtime.GC()
		select {
		case <-srv.canceled:
			return
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("the unread body of a fetch with a live signal was not closed after it was collected")
		}
	}
}

// TestFetchDeflate checks a "deflate" body the script asked for itself:
// the zlib format, and raw DEFLATE.
func TestFetchDeflate(t *testing.T) {
	srv := newServer(t)
	h := fetchHarness(t, srv, webapi.Options{})
	got := h.run(`const get = async p => (await fetch(BASE + p, { headers: { "Accept-Encoding": "deflate" } })).text();
return [await get("/zlib"), await get("/deflate")]`)
	assert.Equal(t, `["hello","hello"]`, got)
}

// TestCloseRejectsArrivedResponse checks that Close rejects a fetch whose
// response arrived before it, in a task that had yet to run.
func TestCloseRejectsArrivedResponse(t *testing.T) {
	srv := newServer(t)
	h := fetchHarness(t, srv, webapi.Options{})
	require.NoError(t, h.rt.SetGlobal("blockThenClose", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		time.Sleep(300 * time.Millisecond) // the response arrives and its task is queued
		h.api.Close()
		return moejs.Undefined(), nil
	})))
	got := h.run(`const p = fetch(BASE + "/echo"); blockThenClose();
try { const r = await p; return "fulfilled " + r.status } catch (e) { return e.message + ": " + e.cause.message }`)
	assert.Equal(t, "fetch failed: webapi: closed", got)
}

// TestSetContext checks the context of a pooled runtime's run: canceling
// it fails the run's fetches in flight and closes its bodies, and the
// runtime fetches again with the context of its next run.
func TestSetContext(t *testing.T) {
	srv := newServer(t)
	h := fetchHarness(t, srv, webapi.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	h.api.SetContext(ctx)
	require.Equal(t, "200", h.run(`return (await fetch(BASE + "/echo")).status`))
	time.AfterFunc(100*time.Millisecond, cancel)
	got := h.run(`const r = await fetch(BASE + "/stall"); const rd = r.body.getReader(); await rd.read();
const out = [];
try { await rd.read(); out.push("read") } catch (e) { out.push(e.message + ": " + e.cause.message) }
try { await fetch(BASE + "/echo"); out.push("fetched") } catch (e) { out.push(e.message + ": " + e.cause.message) }
return out`)
	assert.Equal(t, `["terminated: context canceled","fetch failed: context canceled"]`, got)
	select {
	case <-srv.canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("the body of the canceled run is still open")
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	h.api.SetContext(ctx)
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	got = h.run(`try { await fetch(BASE + "/slow"); return "answered" } catch (e) { return e.message + ": " + e.cause.message }`)
	assert.Equal(t, "fetch failed: context canceled", got)
	h.api.SetContext(context.Background())
	assert.Equal(t, "200", h.run(`return (await fetch(BASE + "/echo")).status`))
}

// TestCloseAfterTerminate checks that Close stops a fetch in flight on a
// loop Terminate ended, which does not stop it itself.
func TestCloseAfterTerminate(t *testing.T) {
	srv := newServer(t)
	h := fetchHarness(t, srv, webapi.Options{})
	require.NoError(t, h.rt.SetGlobal("terminate", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		go h.loop.Terminate()
		return moejs.Undefined(), nil
	})))
	_, err := h.runErr(`fetch(BASE + "/slow").catch(() => {}); await new Promise(r => setTimeout(r, 100)); terminate(); await new Promise(() => {})`)
	require.ErrorIs(t, err, eventloop.ErrTerminated)
	h.api.Close()
	deadline := time.Now().Add(5 * time.Second)
	for srv.open.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	assert.Zero(t, srv.open.Load(), "the request outlived Terminate and Close")
}

// pipeTransport serves every request with a body fed by a pipe whose
// writer writes one chunk and then neither writes nor watches the
// request's context, as a host's in-process transport may: only Close
// ends a Read.
type pipeTransport struct{ w *io.PipeWriter }

func (p *pipeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r, w := io.Pipe()
	p.w = w
	go w.Write([]byte("first"))
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: r, ContentLength: -1, Request: req}, nil
}

// TestFetchCancelBlockedRead checks that reader.cancel() and Close close a
// body at once, while a read of it is blocked.
func TestFetchCancelBlockedRead(t *testing.T) {
	for _, c := range []struct{ name, end string }{
		{"cancel", `await rd.cancel(); return "canceled"`},
		{"abort", `ac.abort(); return "aborted"`},
		{"close", `closeAPI(); try { await p } catch (e) { return e.message + ": " + e.cause.message }`},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, webapi.Options{Client: &http.Client{Transport: &pipeTransport{}}})
			require.NoError(t, h.rt.SetGlobal("closeAPI", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
				h.api.Close()
				return moejs.Undefined(), nil
			})))
			errc := make(chan error, 1)
			var got string
			go func() {
				var err error
				got, err = h.runErr(`const ac = new AbortController(); const r = await fetch("http://mem/x", { signal: ac.signal }); const rd = r.body.getReader();
await rd.read(); const p = rd.read(); p.catch(() => {}); await new Promise(r => setTimeout(r, 50));
` + c.end)
				errc <- err
			}()
			select {
			case err := <-errc:
				require.NoError(t, err)
				assert.NotEmpty(t, got)
				if c.name == "close" {
					assert.Equal(t, "terminated: webapi: closed", got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Run still blocked by a read of a closed body")
			}
		})
	}
}

// TestFetchRedirectCredentials checks that a redirect to another origin,
// another scheme on the same host included, drops Authorization and
// Cookie, and keeps the other headers.
func TestFetchRedirectCredentials(t *testing.T) {
	plain := newServer(t)
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/echo-auth", http.StatusFound)
	}))
	t.Cleanup(tls.Close)
	mux := plain.Config.Handler.(*http.ServeMux)
	mux.HandleFunc("/echo-auth", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"auth": r.Header.Get("Authorization"), "key": r.Header.Get("X-Api-Key"), "cookie": r.Header.Get("Cookie"), "proxy": r.Header.Get("Proxy-Authorization")})
	})
	mux.HandleFunc("/to-localhost", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, strings.Replace(plain.URL, "127.0.0.1", "localhost", 1)+"/echo-auth", http.StatusFound)
	})
	mux.HandleFunc("/same", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/echo-auth", http.StatusFound)
	})
	h := newHarness(t, webapi.Options{Client: tls.Client()})
	require.NoError(t, h.rt.SetGlobal("BASE", plain.URL))
	require.NoError(t, h.rt.SetGlobal("TLS", tls.URL))
	got := h.run(`const hd = { Authorization: "Bearer s3cret", "X-Api-Key": "k", Cookie: "a=b", "Proxy-Authorization": "p" };
const get = async u => (await fetch(u, { headers: hd })).json();
return { sameOrigin: await get(BASE + "/same"), otherHost: await get(BASE + "/to-localhost"), httpsToHttp: await get(TLS + "/") }`)
	assert.JSONEq(t, `{"sameOrigin":{"auth":"Bearer s3cret","key":"k","cookie":"a=b","proxy":"p"},
"otherHost":{"auth":"","key":"k","cookie":"","proxy":""},"httpsToHttp":{"auth":"","key":"k","cookie":"","proxy":""}}`, got)
}

// TestFetchMaxInt64Limit checks bodies under a memory limit of
// math.MaxInt64, the way to get the runtime's counters without a limit.
func TestFetchMaxInt64Limit(t *testing.T) {
	srv := newServer(t)
	h := limitHarness(t, srv, math.MaxInt64)
	got := h.run(`const get = async q => (await (await fetch(BASE + "/big?n=" + q)).text()).length;
const r = await fetch(BASE + "/big?n=" + (3 << 20)); let n = 0; for await (const c of r.body) n += c.length;
return [await get(64 << 10), await get((3 << 20) + "&cl=1"), n]`)
	assert.Equal(t, `[65536,3145728,3145728]`, got)
}

// TestFetchHugeContentLength checks a response announcing the largest
// Content-Length there is and ending early: the body is cut, not a Go
// panic.
func TestFetchHugeContentLength(t *testing.T) {
	srv := newServer(t)
	h := fetchHarness(t, srv, webapi.Options{})
	got := h.run(`const r = await fetch(BASE + "/huge-length");
try { await r.text(); return "read" } catch (e) { return e.name + ": " + e.message }`)
	assert.Equal(t, "TypeError: terminated", got)
}

// panicTransport panics in RoundTrip for the path /panic, and serves a
// body whose Read panics for every other one.
type panicTransport struct{}

func (panicTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/panic" {
		panic("round trip")
	}
	return &http.Response{StatusCode: 200, Header: http.Header{}, ContentLength: -1, Request: req, Body: io.NopCloser(panicBody{})}, nil
}

type panicBody struct{}

func (panicBody) Read([]byte) (int, error) { panic("read") }

// TestFetchTransportPanics checks that a panic of a transport or of a
// body's Read, on the goroutine of a request or a read, fails that fetch
// or read rather than the process.
func TestFetchTransportPanics(t *testing.T) {
	h := newHarness(t, webapi.Options{Client: &http.Client{Transport: panicTransport{}}})
	got := h.run(`const out = [];
const err = e => e.name + ": " + e.message + " (" + e.cause.message + ")";
try { await fetch("http://mem/panic"); out.push("fetched") } catch (e) { out.push(err(e)) }
try { await (await fetch("http://mem/a")).text(); out.push("text") } catch (e) { out.push(err(e)) }
try { await (await fetch("http://mem/b")).body.getReader().read(); out.push("read") } catch (e) { out.push(err(e)) }
return out`)
	assert.Equal(t, `["TypeError: fetch failed (moejs: internal error: round trip)","TypeError: terminated (moejs: internal error: read)","TypeError: terminated (moejs: internal error: read)"]`, got)
}

// TestSetContextCancelCause checks a run canceled with a cause while its
// fetch waits for the response: the fetch fails with that cause, and the
// next run fetches.
func TestSetContextCancelCause(t *testing.T) {
	srv := newServer(t)
	h := fetchHarness(t, srv, webapi.Options{})
	ctx, cancel := context.WithCancelCause(context.Background())
	h.api.SetContext(ctx)
	timer := time.AfterFunc(150*time.Millisecond, func() { cancel(errors.New("run over")) })
	defer timer.Stop()
	got := h.run(`try { await fetch(BASE + "/slow"); return "fulfilled" } catch (e) { return e.message + ": " + e.cause.message }`)
	assert.Equal(t, "fetch failed: run over", got)
	h.api.SetContext(context.Background())
	assert.Equal(t, "200", h.run(`return (await fetch(BASE + "/echo")).status`))
}

// TestFetchTextLimitBoundary checks where a whole-body read stops under a
// memory limit L, which bounds what the run keeps and allocates over the
// whole run: at what the run leaves of L as the read starts, within one
// chunk, read by text() or through the body's stream, with and without a
// Content-Length. What the run leaves is between L less what its budget
// counts (Stats) and L less what it retains (MeasureLive), which a
// measurement during the read counts in place of the budget. The body is
// charged once: the largest one read is well past L/2. The boundary is
// found by bisection, each probe on a new runtime.
func TestFetchTextLimitBoundary(t *testing.T) {
	srv := newServer(t)
	const L = 1 << 20
	const chunk = L / 8 // the read window and largest chunk under L
	for _, c := range []struct{ name, read string }{
		{"text", `r.text()`},
		{"stream", `new Response(r.body).text()`},
	} {
		for _, cl := range []string{"", "&cl=1"} {
			name := c.name + cl
			var used, live int64 // as the read started, in the last probe that read the body
			probe := func(n int) bool {
				h := limitHarness(t, srv, L)
				var u, l int64
				require.NoError(t, h.rt.SetGlobal("mark", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
					s := h.rt.Stats()
					u, l = s.LiveBytes+s.RequestAllocatedBytes, h.rt.Realm().MeasureLive(nil)
					return moejs.Undefined(), nil
				})))
				got, err := h.runErr(fmt.Sprintf(`const r = await fetch(BASE + "/big?n=%d%s"); mark();
return (await %s).length`, n, cl, c.read))
				if err != nil {
					require.ErrorIs(t, err, moejs.ErrMemoryLimit, "%s n=%d: %q", name, n, got)
					return false
				}
				if got == tooLarge {
					return false
				}
				require.Equal(t, strconv.Itoa(n), got, name)
				used, live = u, l
				return true
			}
			lo, hi := 0, L+1 // a body of lo bytes is read, one of hi is not
			require.False(t, probe(hi), name)
			for hi-lo > 4<<10 {
				if mid := (lo + hi) / 2; probe(mid) {
					lo = mid
				} else {
					hi = mid
				}
			}
			t.Logf("%s: boundary %d, L-%d; the run used %d, retained %d", name, lo, L-lo, used, live)
			assert.GreaterOrEqual(t, lo, L-int(used)-chunk, "%s: the boundary is below what the run left", name)
			assert.LessOrEqual(t, lo, L-int(live)+chunk, "%s: the boundary is past what the run left", name)
		}
	}
}

// tooLarge is the rejection of a body read whole that is larger than the
// memory limit.
const tooLarge = "REJECTED RangeError: The body is larger than the memory limit"

// TestFetchTextLimitBoundaryPerTask checks what a whole-body read takes
// under a memory limit L with the loop's ResetMemoryPerTask, which gives
// each task a budget of its own: a body of L bytes and no more, read by
// text() or through its stream, with and without a Content-Length; a
// larger one rejects with a RangeError, as a builtin past the limit throws.
func TestFetchTextLimitBoundaryPerTask(t *testing.T) {
	srv := newServer(t)
	const L = 1 << 20
	for _, c := range []struct{ name, read string }{
		{"text", `(await fetch(BASE + "/big?n=%d%s")).text()`},
		{"stream", `new Response((await fetch(BASE + "/big?n=%d%s")).body).text()`},
	} {
		for _, cl := range []string{"", "&cl=1"} {
			for _, n := range []int{L - 64<<10, L - 1, L, L + 1} {
				h := limitHarnessLoop(t, srv, L, eventloop.Options{ResetMemoryPerTask: true})
				got, err := h.runErr(`return (await ` + fmt.Sprintf(c.read, n, cl) + `).length`)
				assert.NoError(t, err, "%s%s n=L%+d", c.name, cl, n-L)
				if n <= L {
					assert.Equal(t, strconv.Itoa(n), got, "%s%s n=L%+d", c.name, cl, n-L)
				} else {
					assert.Equal(t, tooLarge, got, "%s%s n=L%+d", c.name, cl, n-L)
				}
			}
		}
	}
}

// TestFetchStreamTinyLimit checks a body streamed under a memory limit of
// 256 KiB by a script that keeps one chunk at a time: the read buffer
// stays an eighth of the limit, so the body is read to its end.
func TestFetchStreamTinyLimit(t *testing.T) {
	srv := newServer(t)
	h := limitHarness(t, srv, 256<<10)
	got := h.run(`const r = await fetch(BASE + "/big?n=" + (4 << 20)); let n = 0, max = 0;
for await (const c of r.body) { n += c.length; max = Math.max(max, c.buffer.byteLength) }
return [n, max <= (32 << 10)]`)
	assert.Equal(t, `[4194304,true]`, got)
}

// closePanicTransport serves bodies whose Close panics, and counts the
// calls of Close. A request to /block waits for release first.
type closePanicTransport struct {
	closes  atomic.Int64
	release chan struct{}
}

type closePanicBody struct{ t *closePanicTransport }

func (closePanicBody) Read([]byte) (int, error) { return 0, io.EOF }

func (b closePanicBody) Close() error {
	b.t.closes.Add(1)
	panic("close")
}

func (t *closePanicTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/block" {
		<-t.release
	}
	return &http.Response{StatusCode: 200, Header: http.Header{}, ContentLength: -1, Request: req, Body: closePanicBody{t}}, nil
}

// TestBodyClosePanics checks that a response body whose Close panics does
// not take the process down where it is closed off the loop: by the
// watcher of SetContext's context, by the collector's cleanup of a body
// nobody reads, by Close, and by the goroutine of a request whose
// response arrives after Terminate.
func TestBodyClosePanics(t *testing.T) {
	tr := &closePanicTransport{release: make(chan struct{})}
	h := newHarness(t, webapi.Options{Client: &http.Client{Transport: tr}})
	wait := func(n int64, what string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for tr.closes.Load() < n {
			if time.Now().After(deadline) {
				t.Fatalf("%s: the body was not closed", what)
			}
			runtime.GC()
			time.Sleep(10 * time.Millisecond)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.api.SetContext(ctx)
	assert.Equal(t, "200", h.run(`globalThis.r1 = await fetch("http://mem/a"); return r1.status`))
	cancel()
	wait(1, "SetContext")
	h.api.SetContext(context.Background())
	assert.Equal(t, "200", h.run(`return (await fetch("http://mem/b")).status`))
	h.run(`let x = 0; for (let i = 0; i < 1000; i++) x += [i].length; return x`)
	wait(2, "collected")
	assert.Equal(t, "200", h.run(`globalThis.r3 = await fetch("http://mem/c"); return r3.status`))
	h.api.Close()
	wait(3, "Close")

	h = newHarness(t, webapi.Options{Client: &http.Client{Transport: tr}})
	require.NoError(t, h.rt.SetGlobal("terminate", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		go func() {
			h.loop.Terminate()
			close(tr.release)
		}()
		return moejs.Undefined(), nil
	})))
	_, err := h.runErr(`fetch("http://mem/block").catch(() => {}); terminate(); await new Promise(() => {})`)
	require.ErrorIs(t, err, eventloop.ErrTerminated)
	wait(4, "Terminate")
}

// TestFetchRedirectCredentialsChain checks that credentials dropped on a
// redirect to another origin stay dropped when a later redirect comes back
// to the first one, and that a port change is another origin.
func TestFetchRedirectCredentialsChain(t *testing.T) {
	srv := newServer(t)
	other := newServer(t)
	echo := func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"auth": r.Header.Get("Authorization"), "cookie": r.Header.Get("Cookie")})
	}
	mux := srv.Config.Handler.(*http.ServeMux)
	mux.HandleFunc("/creds", echo)
	other.Config.Handler.(*http.ServeMux).HandleFunc("/creds", echo)
	local := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	mux.HandleFunc("/hop1", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, local+"/hop2", http.StatusFound)
	})
	mux.HandleFunc("/hop2", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/creds", http.StatusFound)
	})
	mux.HandleFunc("/port", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/creds", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/same", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/creds", http.StatusMovedPermanently)
	})
	h := fetchHarness(t, srv, webapi.Options{})
	got := h.run(`const hd = { Authorization: "Bearer s", Cookie: "a=b" };
const get = async u => (await fetch(BASE + u, { headers: hd })).json();
return { chain: await get("/hop1"), port: await get("/port"), same: await get("/same") }`)
	assert.JSONEq(t, `{"chain":{"auth":"","cookie":""},"port":{"auth":"","cookie":""},"same":{"auth":"Bearer s","cookie":"a=b"}}`, got)
}

// TestAbortSignalWeakKeepsAborts checks that the weak references of
// AbortSignal.any and of a fetch's abort lose no abort when the collector
// runs while the signals wait: listeners, handlers, a nested any, a fetch
// in flight, a body read and a text().
func TestAbortSignalWeakKeepsAborts(t *testing.T) {
	srv := newServer(t)
	h := fetchHarness(t, srv, webapi.Options{})
	require.NoError(t, h.rt.SetGlobal("gc", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		for range 3 {
			runtime.GC()
		}
		return moejs.Undefined(), nil
	})))
	got := h.run(`globalThis.root = new AbortController(); globalThis.log = [];
(function () {
  const d1 = AbortSignal.any([root.signal]); d1.addEventListener("abort", () => log.push("listener"));
  const d2 = AbortSignal.any([root.signal]); d2.onabort = () => log.push("onabort");
  const d3 = AbortSignal.any([AbortSignal.any([root.signal])]); d3.addEventListener("abort", () => log.push("nested"));
  fetch(BASE + "/slow", { signal: AbortSignal.any([root.signal]) }).catch(e => log.push("fetch " + e.name));
  for (let i = 0; i < 1000; i++) AbortSignal.any([root.signal]);
})();
globalThis.ac2 = new AbortController();
await (async () => {
  const r = await fetch(BASE + "/stall", { signal: AbortSignal.any([ac2.signal]) });
  globalThis.rd = r.body.getReader(); await rd.read();
  const r2 = await fetch(BASE + "/stall", { signal: AbortSignal.any([ac2.signal]) });
  globalThis.tp = r2.text().then(() => "text ok", e => "text " + e.name);
})();
await (async () => { let x = 0; for (let i = 0; i < 2000; i++) x += [i].length; return x })();
await new Promise(r => setTimeout(r, 10));
gc();
root.abort(); const p = rd.read().then(() => "read ok", e => "read " + e.name); ac2.abort();
await new Promise(r => setTimeout(r, 100)); log.push(await p, await tp); return log.sort()`)
	assert.Equal(t, `["fetch AbortError","listener","nested","onabort","read AbortError","text AbortError"]`, got)
}
