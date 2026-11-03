// Command webapibench measures fetch of package webapi against Node.js:
//
//	webapibench -serve 127.0.0.1:8090                # the HTTP server
//	webapibench -js fetch-bench.js -base URL         # the script in moejs
//	node fetch-bench.js  (BASE=URL)                  # the script in Node.js
//	webapibench -go -base URL                        # the same requests from Go
//
// The server answers POST /small with a chat-completion-sized JSON body,
// /stream?mb=N with N MiB written 64 KiB at a time, and /sse?n=N with N
// server-sent events, each written and flushed on its own. The Go mode is
// the floor: what net/http alone costs for the same work.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/eventloop"
	"github.com/MoeForge/moejs/webapi"
)

var smallReply = []byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1760000000,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"Hello! How can I help you today?"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":9,"total_tokens":19}}`)

func serve(addr string) {
	block := bytes.Repeat([]byte("0123456789abcdef"), 4096) // 64 KiB
	mux := http.NewServeMux()
	mux.HandleFunc("/small", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req_123")
		w.Write(smallReply)
	})
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		mb, _ := strconv.Atoi(r.URL.Query().Get("mb"))
		w.Header().Set("Content-Length", strconv.Itoa(mb<<20))
		for range mb * 16 {
			if _, err := w.Write(block); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		buf := make([]byte, 0, 256)
		for i := range n {
			buf = append(buf[:0], `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"tok`...)
			buf = strconv.AppendInt(buf, int64(i), 10)
			buf = append(buf, "\"}}]}\n\n"...)
			if _, err := w.Write(buf); err != nil {
				return
			}
			f.Flush()
		}
	})
	log.Fatal(http.ListenAndServe(addr, mux))
}

func runJS(file, base string, rounds int, only string) {
	src, err := os.ReadFile(file)
	check(err)
	rt := moejs.NewRuntime(moejs.Options{})
	loop, err := eventloop.New(rt, eventloop.Options{})
	check(err)
	api, err := webapi.Install(loop, webapi.Options{Client: &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}}})
	check(err)
	defer api.Close()
	check(rt.SetGlobal("BASE", base))
	check(rt.SetGlobal("ROUNDS", rounds))
	check(rt.SetGlobal("ONLY", only))
	check(rt.SetGlobal("print", moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		fmt.Println(moejs.Arg(args, 0).String())
		return moejs.Undefined(), nil
	})))
	check(rt.SetGlobal("cpuusage", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		return moejs.Number(cpuMicros()), nil
	})))
	var ms runtime.MemStats
	check(rt.SetGlobal("memstats", moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, _ []moejs.Value) (moejs.Value, error) {
		runtime.ReadMemStats(&ms)
		return rt.FromGo([]any{float64(ms.Mallocs), float64(ms.TotalAlloc)})
	})))
	s, err := moejs.CompileScript(file, string(src))
	check(err)
	check(loop.Run(func(rt *moejs.Runtime) error {
		_, err := rt.RunScript(s)
		return err
	}))
}

// runGo does the script's work with net/http alone.
func runGo(base string, rounds int, only string) {
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}}
	req := []byte(`{"model":"m","stream":false,"messages":[{"role":"user","content":"Say hello."}]}`)
	small := func(n, conc int) map[string]any {
		var wg sync.WaitGroup
		var mu sync.Mutex
		i := 0
		for range conc {
			wg.Go(func() {
				for {
					mu.Lock()
					i++
					stop := i > n
					mu.Unlock()
					if stop {
						return
					}
					hr, _ := http.NewRequest("POST", base+"/small", bytes.NewReader(req))
					hr.Header.Set("Content-Type", "application/json")
					hr.Header.Set("Authorization", "Bearer k")
					resp, err := client.Do(hr)
					check(err)
					var v map[string]any
					check(json.NewDecoder(resp.Body).Decode(&v))
					resp.Body.Close()
				}
			})
		}
		wg.Wait()
		return nil
	}
	stream := func(path string) map[string]any {
		resp, err := client.Get(base + path)
		check(err)
		buf := make([]byte, 64<<10)
		bytes, chunks := 0, 0
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				bytes += n
				chunks++
			}
			if err == io.EOF {
				break
			}
			check(err)
		}
		resp.Body.Close()
		return map[string]any{"bytes": bytes, "chunks": chunks}
	}
	measure := func(name string, units int, f func() map[string]any) {
		f()
		var m0, m1 runtime.MemStats
		for round := range rounds {
			runtime.ReadMemStats(&m0)
			c0 := cpuMicros()
			t0 := time.Now()
			extra := f()
			ms := float64(time.Since(t0)) / 1e6
			c1 := cpuMicros()
			runtime.ReadMemStats(&m1)
			row := map[string]any{"name": name, "round": round, "ms": ms, "units": units, "usPerUnit": ms * 1000 / float64(units),
				"cpuUsPerUnit":  (c1 - c0) / float64(units),
				"allocsPerUnit": float64(m1.Mallocs-m0.Mallocs) / float64(units), "bytesPerUnit": (m1.TotalAlloc - m0.TotalAlloc) / uint64(units)}
			for k, v := range extra {
				row[k] = v
			}
			if b, ok := extra["bytes"].(int); ok {
				row["MBps"] = float64(b) / ms / 1000
				row["usPerChunk"] = ms * 1000 / float64(extra["chunks"].(int))
				row["cpuUsPerChunk"] = (c1 - c0) / float64(extra["chunks"].(int))
			}
			out, _ := json.Marshal(row)
			fmt.Println(string(out))
		}
	}
	want := func(n string) bool { return only == "" || strings.Contains(","+only+",", ","+n+",") }
	if want("small1") {
		measure("small1", 2000, func() map[string]any { return small(2000, 1) })
	}
	if want("small8") {
		measure("small8", 4000, func() map[string]any { return small(4000, 8) })
	}
	if want("stream") {
		measure("stream", 1, func() map[string]any { return stream("/stream?mb=256") })
	}
	if want("sse") {
		measure("sse", 20000, func() map[string]any { return stream("/sse?n=20000") })
	}
}

// cpuMicros is the process's CPU time, user and system, in microseconds.
func cpuMicros() float64 {
	var ru syscall.Rusage
	check(syscall.Getrusage(syscall.RUSAGE_SELF, &ru))
	return float64(ru.Utime.Nano()+ru.Stime.Nano()) / 1e3
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func main() {
	addr := flag.String("serve", "", "serve on this address")
	js := flag.String("js", "", "run this script in moejs")
	goMode := flag.Bool("go", false, "do the work with net/http alone")
	base := flag.String("base", "http://127.0.0.1:8090", "the server's URL")
	rounds := flag.Int("rounds", 5, "measured rounds of each case")
	only := flag.String("only", "", "comma-separated cases: small1,small8,stream,sse")
	prof := flag.String("cpuprofile", "", "write a CPU profile")
	flag.Parse()
	if *prof != "" {
		f, err := os.Create(*prof)
		check(err)
		check(pprof.StartCPUProfile(f))
		defer pprof.StopCPUProfile()
	}
	switch {
	case *addr != "":
		serve(*addr)
	case *js != "":
		runJS(*js, *base, *rounds, *only)
	case *goMode:
		runGo(*base, *rounds, *only)
	default:
		flag.Usage()
	}
}
