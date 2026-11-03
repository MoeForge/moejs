package webapi_test

import (
	"bytes"
	"flag"
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/eventloop"
	"github.com/MoeForge/moejs/webapi"
)

// memTransport answers every request from memory: body is the response
// body, given chunk bytes per Read.
type memTransport struct {
	body  []byte
	chunk int
}

type chunkReader struct {
	b     []byte
	chunk int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.b) == 0 {
		return 0, io.EOF
	}
	n := min(len(p), len(c.b), c.chunk)
	copy(p, c.b[:n])
	c.b = c.b[n:]
	return n, nil
}

func (t *memTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		io.Copy(io.Discard, req.Body)
		req.Body.Close()
	}
	return &http.Response{
		StatusCode: 200, Status: "200 OK", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header:        http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {"req_1"}},
		Body:          io.NopCloser(&chunkReader{b: t.body, chunk: t.chunk}),
		ContentLength: int64(len(t.body)), Request: req,
	}, nil
}

// benchLimit is the memory limit of the runtimes of benchJS, 0 for none:
// go test -bench . -args -memlimit 67108864.
var benchLimit = flag.Int64("memlimit", 0, "the memory limit of the benchmarks' runtimes")

// benchJS runs the function bench(n) of src b.N times through one loop
// run, and reports the allocations per call.
func benchJS(b *testing.B, tr http.RoundTripper, src string) {
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: *benchLimit})
	loop, err := eventloop.New(rt, eventloop.Options{})
	if err != nil {
		b.Fatal(err)
	}
	api, err := webapi.Install(loop, webapi.Options{Client: &http.Client{Transport: tr}})
	if err != nil {
		b.Fatal(err)
	}
	defer api.Close()
	s, err := moejs.CompileScript("bench.js", src+`
globalThis.result = undefined;
bench(N).then(() => { result = "ok" }, e => { result = String(e) });`)
	if err != nil {
		b.Fatal(err)
	}
	run := func(n int) {
		if err := rt.SetGlobal("N", n); err != nil {
			b.Fatal(err)
		}
		if err := loop.Run(func(rt *moejs.Runtime) error { _, err := rt.RunScript(s); return err }); err != nil {
			b.Fatal(err)
		}
		v, _ := rt.Realm().Global.GetProp(rt.Realm(), rt.Realm().KeyFromGoString("result"))
		if v.String() != "ok" {
			b.Fatal(v.String())
		}
	}
	run(10)
	b.ReportAllocs()
	b.ResetTimer()
	run(b.N)
}

var smallReply = []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Hello!"},"finish_reason":"stop"}]}`)

// BenchmarkFetchJSON is a small JSON request and response through an
// in-memory transport: what webapi and the engine cost per request.
func BenchmarkFetchJSON(b *testing.B) {
	benchJS(b, &memTransport{body: smallReply, chunk: 1 << 20}, `
var body = JSON.stringify({ model: "m", messages: [{ role: "user", content: "hi" }] });
async function bench(n) {
  for (let i = 0; i < n; i++) {
    const r = await fetch("http://api.example/v1/chat", { method: "POST", headers: { "content-type": "application/json", authorization: "Bearer k" }, body });
    if (r.headers.get("x-request-id") !== "req_1") throw new Error("header");
    await r.json();
  }
}`)
}

// BenchmarkBodyChunk reads a body of 4 KiB chunks with getReader: the cost
// of one chunk.
func BenchmarkBodyChunk(b *testing.B) {
	const chunk = 4 << 10
	benchJS(b, &memTransport{body: bytes.Repeat([]byte("x"), 1<<20), chunk: chunk}, `
async function bench(n) {
  let left = n;
  while (left > 0) {
    const r = await fetch("http://api.example/stream");
    const rd = r.body.getReader();
    for (;;) {
      const { done, value } = await rd.read();
      if (done) break;
      if (--left === 0) { await rd.cancel(); break; }
    }
  }
}`)
}

// BenchmarkReadQueued reads chunks a JavaScript source enqueued: the
// stream's own cost per read.
func BenchmarkReadQueued(b *testing.B) {
	benchJS(b, nil, `
async function bench(n) {
  const chunk = new Uint8Array(16);
  const s = new ReadableStream({ pull(c) { c.enqueue(chunk); } }, { highWaterMark: 4 });
  const rd = s.getReader();
  for (let i = 0; i < n; i++) await rd.read();
}`)
}

// BenchmarkSSEEvent parses server-sent events from a body read in 1 KiB
// chunks, as fetch-bench.js does: the cost of one event, JavaScript's
// parsing included, without a network.
func BenchmarkSSEEvent(b *testing.B) {
	var body []byte
	for i := range 1000 {
		body = append(body, `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"tok`...)
		body = strconv.AppendInt(body, int64(i), 10)
		body = append(body, "\"}}]}\n\n"...)
	}
	benchJS(b, &memTransport{body: body, chunk: 1 << 10}, `
async function bench(n) {
  let events = 0;
  while (events < n) {
    const r = await fetch("http://api.example/sse");
    const rd = r.body.getReader();
    const dec = new TextDecoder();
    let buf = "";
    for (;;) {
      const { done, value } = await rd.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let i;
      while ((i = buf.indexOf("\n\n")) >= 0) {
        JSON.parse(buf.slice(6, i));
        buf = buf.slice(i + 2);
        if (++events === n) { await rd.cancel(); return; }
      }
    }
  }
}`)
}

// BenchmarkInstall is a runtime and its loop with the Web APIs installed
// and not used; BenchmarkNewRuntimeAndLoop is the same without Install.
func BenchmarkInstall(b *testing.B) {
	for _, mutable := range []bool{false, true} {
		name := "shared"
		if mutable {
			name = "mutable"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				rt := moejs.NewRuntime(moejs.Options{MutableIntrinsics: mutable})
				loop, _ := eventloop.New(rt, eventloop.Options{})
				_, _ = webapi.Install(loop, webapi.Options{})
			}
		})
	}
}

func BenchmarkNewRuntimeAndLoop(b *testing.B) {
	for _, mutable := range []bool{false, true} {
		name := "shared"
		if mutable {
			name = "mutable"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				rt := moejs.NewRuntime(moejs.Options{MutableIntrinsics: mutable})
				_, _ = eventloop.New(rt, eventloop.Options{})
			}
		})
	}
}
