package webapi

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"weak"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
)

// fetch and its Go side. A request runs on a goroutine of its own; the
// stream of its response body is read on another, a chunk ahead of what
// the stream took.
// The conn of a request owns its context and body, and is the only state
// the goroutines share with the loop.

var (
	errAborted   = errors.New("webapi: the request was aborted")
	errCanceled  = errors.New("webapi: the response body was canceled")
	errCollected = errors.New("webapi: the response body was garbage-collected unread")
	errRedirect  = errors.New("webapi: redirect mode is \"error\"")
	errTooMany   = errors.New("webapi: too many redirects")
)

// AllowHosts returns an Allow hook that approves the requests to the given
// hosts and rejects every other one. A host is matched case-insensitively
// against the request URL's hostname, and against the Host header's when
// the script set one: both must be allowed. "*.example.com" matches every
// subdomain of example.com, not example.com itself; "*" matches every host.
func AllowHosts(hosts ...string) func(*http.Request) error {
	exact := make(map[string]bool, len(hosts))
	var suffixes []string
	all := false
	for _, h := range hosts {
		h = strings.ToLower(h)
		switch {
		case h == "*":
			all = true
		case strings.HasPrefix(h, "*."):
			suffixes = append(suffixes, h[1:])
		default:
			exact[h] = true
		}
	}
	allowed := func(h string) bool {
		if exact[h] {
			return true
		}
		for _, s := range suffixes {
			if strings.HasSuffix(h, s) {
				return true
			}
		}
		return false
	}
	return func(req *http.Request) error {
		if all {
			return nil
		}
		h := strings.ToLower(req.URL.Hostname())
		if !allowed(h) {
			return errors.New("webapi: host " + h + " is not allowed")
		}
		if req.Host != "" && req.Host != req.URL.Host {
			if h := strings.ToLower(hostname(req.Host)); !allowed(h) {
				return errors.New("webapi: host " + h + " (the Host header) is not allowed")
			}
		}
		return nil
	}
}

// hostname is the host of a Host header without its port.
func hostname(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]")
}

// ErrNoNetwork is the cause of a fetch's TypeError when Options.Client is
// nil.
var ErrNoNetwork = errors.New("webapi: fetch has no network (Options.Client is nil)")

// ErrClosed is the cause of the error of a fetch that Close stopped or that
// started after Close.
var ErrClosed = errors.New("webapi: closed")

// Close stops every fetch in flight, whose promise or body read rejects
// with a TypeError, and closes every response body JavaScript has not read
// to its end, so that no request, goroutine or connection outlives the
// runtime's use. A fetch after Close rejects: Close is for a runtime that
// is dropped; a runtime that goes back to a pool ends its run's requests
// with the context of SetContext instead. Close also ends what the
// installation reports to the memory limit's measurements: the runtime
// drops it at its next measurement. Close may be called from any
// goroutine; the promises settle when the loop next runs. Terminating the
// loop does not stop the requests in flight: call Close as well.
func (a *API) Close() {
	e := a.e
	e.base()
	e.cancel(ErrClosed)
	e.mu.Lock()
	conns := make([]*conn, 0, len(e.conns))
	for c := range e.conns {
		conns = append(conns, c)
	}
	timers := e.timers
	e.timers = nil
	e.closed = true
	reg := e.live.reg
	e.mu.Unlock()
	if reg != nil {
		reg.e.Store(nil)
	}
	for _, c := range conns {
		c.stop(ErrClosed)
	}
	for t := range timers {
		t.Stop()
	}
}

// SetContext makes ctx a parent of the requests of the fetches started
// from now on, with Options.Context, for a host that pools runtimes:
// canceling ctx when a run ends fails the fetches of that run still in
// flight, as a network error does (the error's cause is ctx's), and closes
// their response bodies at once, so that nothing of the run keeps the loop
// or a connection, while the runtime keeps fetching in its next run with
// the context set then. nil leaves Options.Context alone. It may be called
// from any goroutine.
func (a *API) SetContext(ctx context.Context) {
	a.e.mu.Lock()
	a.e.run = ctx
	a.e.mu.Unlock()
}

// netState is the state of fetch in an installation.
type netState struct {
	clientsOnce sync.Once
	clients     [3]*http.Client // by redirect mode

	mu sync.Mutex
	// ctx is the parent of the requests' contexts, made from
	// Options.Context on first use; cancel is Close's.
	ctx    context.Context
	cancel context.CancelCauseFunc
	run    context.Context      // SetContext's, or nil
	conns  map[*conn]struct{}   // requests and bodies not yet closed
	timers map[*time.Timer]bool // the timers of AbortSignal.timeout
	closed bool                 // Close ran

	// canon caches the canonical forms of header names.
	canon map[string]string
}

// base returns the context the requests derive from and the one of
// SetContext, creating the first on first use.
func (e *env) base() (ctx, run context.Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ctx == nil {
		parent := e.opts.Context
		if parent == nil {
			parent = context.Background()
		}
		e.ctx, e.cancel = context.WithCancelCause(parent)
	}
	return e.ctx, e.run
}

// conn is the Go state of a request: its context and, once the response
// arrived, its body until closed. Its methods may be called from any
// goroutine.
type conn struct {
	e      *env
	base   context.Context // the env's
	ctx    context.Context
	cancel context.CancelCauseFunc

	mu      sync.Mutex
	unwatch func() bool   // stops watching SetContext's context, or nil
	body    io.ReadCloser // nil before the response and after it is closed
	reading bool          // a goroutine reads the body
	done    bool
	why     error // what stopped the request from outside: Close, a context
}

// newConn creates the conn of a request whose context derives from base
// and, when not nil, ends with run.
func (e *env) newConn(base, run context.Context) *conn {
	ctx, cancel := context.WithCancelCause(base)
	c := &conn{e: e, base: base, ctx: ctx, cancel: cancel}
	// The conn is listed before run can stop it: a run canceled already
	// stops it at once, on another goroutine.
	e.mu.Lock()
	if e.conns == nil {
		e.conns = map[*conn]struct{}{}
	}
	e.conns[c] = struct{}{}
	e.mu.Unlock()
	if run != nil {
		unwatch := context.AfterFunc(run, func() { c.stop(context.Cause(run)) })
		c.mu.Lock()
		done := c.done
		if !done {
			c.unwatch = unwatch
		}
		c.mu.Unlock()
		if done {
			unwatch()
		}
	}
	return c
}

// finish ends the request: it cancels the context with cause and closes
// the body at once, also while a read of it runs, which then returns with
// an error.
func (c *conn) finish(cause error) {
	c.cancel(cause)
	c.mu.Lock()
	if c.done {
		c.mu.Unlock()
		return
	}
	c.done = true
	body, unwatch := c.body, c.unwatch
	c.body, c.unwatch = nil, nil
	c.mu.Unlock()
	if unwatch != nil {
		unwatch()
	}
	if body != nil {
		closeBody(body)
	}
	c.e.mu.Lock()
	delete(c.e.conns, c)
	c.e.mu.Unlock()
}

// closeBody closes a response body. finish runs on any goroutine: on the
// watcher of SetContext's context, the collector's cleanup or a request's
// goroutine, where nothing would recover a panic of the body's Close, so
// such a panic is dropped; the body is done with either way.
func closeBody(body io.Closer) {
	defer func() { _ = recover() }()
	body.Close()
}

// stop is finish for Close and the contexts: why is the cause of the
// errors JavaScript gets.
func (c *conn) stop(why error) {
	c.mu.Lock()
	if c.why == nil && !c.done {
		c.why = why
	}
	c.mu.Unlock()
	c.finish(why)
}

// setBody makes body the request's body; false when the request is
// finished already, and body is closed.
func (c *conn) setBody(body io.ReadCloser) bool {
	c.mu.Lock()
	done := c.done
	if !done {
		c.body = body
	}
	c.mu.Unlock()
	if done {
		closeBody(body)
	}
	return !done
}

// startRead marks a read running and returns the body, or nil when the
// request is finished or another read runs: one body is read by one
// goroutine at a time.
func (c *conn) startRead() io.ReadCloser {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done || c.body == nil || c.reading {
		return nil
	}
	c.reading = true
	return c.body
}

// endRead marks the read over.
func (c *conn) endRead() {
	c.mu.Lock()
	c.reading = false
	c.mu.Unlock()
}

// cause is why the request stopped when Close or a context stopped it,
// or err.
func (c *conn) cause(err error) error {
	if errors.Is(err, io.EOF) {
		return err
	}
	c.mu.Lock()
	why := c.why
	c.mu.Unlock()
	if why != nil {
		return why
	}
	if c.base.Err() != nil {
		if cause := context.Cause(c.ctx); cause != nil {
			return cause
		}
	}
	return err
}

// --- errors ---------------------------------------------------------------------

// netError is the TypeError of a network error: message, with a cause
// Error that has the Go error's text and, for the common ones, the
// Node.js code (ECONNREFUSED, ECONNRESET, ETIMEDOUT, ENOTFOUND).
func (e *env) netError(msg string, err error) engine.Value {
	r := e.r
	te := e.newError(engine.KindTypeError, msg)
	cause := e.newError(engine.KindError, err.Error())
	if code := errCode(err); code != "" {
		cause.DefineOwnDataFast(r, e.keys.code, str(code), attrW|attrC)
	}
	te.DefineOwnDataFast(r, e.keys.cause, engine.ObjectValue(cause), attrW|attrC)
	return engine.ObjectValue(te)
}

func errCode(err error) string {
	var dns *net.DNSError
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "ECONNREFUSED"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, io.ErrUnexpectedEOF):
		return "ECONNRESET"
	case errors.As(err, &dns):
		return "ENOTFOUND"
	case errors.Is(err, os.ErrDeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		return "ETIMEDOUT"
	}
	return ""
}

// --- fetch ----------------------------------------------------------------------

// fetchState is a fetch in flight.
type fetchState struct {
	e           *env
	rq          *request
	c           *conn
	p           promise
	aborted     bool
	net         *netBody // the response's body, once it arrived
	reqBody     *streamBody
	removeAbort func()
}

// fetch implements fetch(input, init).
func (e *env) fetch(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
	input, init := engine.Arg(args, 0), engine.Arg(args, 1)
	var rq *request
	if x, ok := input.IsObject(), false; x && init.IsUndefined() {
		if rq, ok = input.AsObject().Internal().(*request); ok {
			if rq.body.isUsed() || rq.body.locked() {
				return e.rejected(e.typeError("Failed to execute 'fetch': Cannot construct a Request with a Request object that has already been used.")), nil
			}
			c := *rq
			if rq.ownSignal != nil {
				c.signal = rq.ownSignal
			}
			if !rq.body.null && rq.body.stream == nil {
				// The fetch takes the bytes over.
				rq.body.used, rq.body.src = true, nil
			}
			rq = &c
		}
	}
	if rq == nil {
		var err error
		if rq, err = e.newRequest(input, init); err != nil {
			return e.rejectedErr(err)
		}
	}
	if s := rq.signal; s != nil && s.aborted {
		if rq.body.stream != nil && !rq.body.stream.locked() {
			if _, err := e.cancelStream(rq.body.stream, s.reason); err != nil {
				return undefined, err
			}
		}
		return e.rejected(s.reason), nil
	}
	p := e.newPromise()
	if e.opts.Client == nil {
		return p.value(), e.rejectP(p, e.netError("fetch failed", ErrNoNetwork))
	}
	base, run := e.base()
	if err := context.Cause(base); err != nil {
		return p.value(), e.rejectP(p, e.netError("fetch failed", err))
	}
	if run != nil {
		if err := context.Cause(run); err != nil {
			return p.value(), e.rejectP(p, e.netError("fetch failed", err))
		}
	}
	f := &fetchState{e: e, rq: rq, p: p, c: e.newConn(base, run)}
	e.live.add(e, f)
	req, err := e.goRequest(f)
	if err != nil {
		f.c.finish(err)
		e.live.remove(f)
		reason, ok := r.ThrownValue(err)
		if !ok {
			return undefined, err
		}
		return p.value(), e.rejectP(p, reason)
	}
	if rq.signal != nil {
		// The signal may outlive the fetch by far: its algorithm must not
		// keep the response, whose body the collector closes when
		// JavaScript drops it unread.
		wf, c := weak.Make(f), f.c
		f.removeAbort = rq.signal.addAlgorithm(func(reason engine.Value) error {
			if f := wf.Value(); f != nil {
				return f.abort(reason)
			}
			c.finish(errAborted)
			return nil
		}, undefined)
	}
	client := e.client(rq.redirect)
	allow := e.opts.Allow
	done := e.loop.Hold()
	go func() {
		resp, err := send(allow, client, req)
		if !e.post(func() error { return f.responded(resp, err) }) {
			if resp != nil {
				closeBody(resp.Body)
			}
			f.c.finish(ErrClosed)
		}
		done(nil)
	}()
	return p.value(), nil
}

// send approves req with allow, when not nil, and sends it with client,
// on the request's goroutine; a panic of either is the error.
func send(allow func(*http.Request) error, client *http.Client, req *http.Request) (resp *http.Response, err error) {
	defer recoverRead(&err)
	if allow != nil {
		if err = allow(req); err != nil {
			return nil, err
		}
	}
	return client.Do(req)
}

// client returns the client of the redirect mode.
func (e *env) client(mode int) *http.Client {
	e.clientsOnce.Do(func() {
		base := e.opts.Client
		allow := e.opts.Allow
		for m := range e.clients {
			c := *base
			user := base.CheckRedirect
			c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
				switch m {
				case redirectError:
					return errRedirect
				case redirectManual:
					return http.ErrUseLastResponse
				}
				if len(via) >= 20 {
					return errTooMany
				}
				if crossOrigin(req, via) {
					// The Fetch Standard drops Authorization on a redirect
					// to another origin, as undici does Cookie and
					// Proxy-Authorization too; Go compares the hosts only.
					delete(req.Header, "Authorization")
					delete(req.Header, "Cookie")
					delete(req.Header, "Proxy-Authorization")
				}
				if allow != nil {
					if err := allow(req); err != nil {
						return err
					}
				}
				if user != nil {
					return user(req, via)
				}
				return nil
			}
			e.clients[m] = &c
		}
	})
	return e.clients[mode]
}

// crossOrigin reports whether a redirect of the chain via to req changed
// the origin: a header dropped once stays dropped.
func crossOrigin(req *http.Request, via []*http.Request) bool {
	prev := req.URL
	for i := len(via) - 1; i >= 0; i-- {
		if !sameOrigin(via[i].URL, prev) {
			return true
		}
		prev = via[i].URL
	}
	return false
}

// sameOrigin reports whether a and b have the same scheme, host and port.
func sameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}

// port is the port of u, the scheme's default when absent.
func port(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch u.Scheme {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}

// goRequest makes the http.Request of f's request.
func (e *env) goRequest(f *fetchState) (*http.Request, error) {
	rq := f.rq
	u := rq.url
	switch u.Scheme() {
	case "http", "https":
	default:
		return nil, &engine.Exception{Value: e.netError("fetch failed", errors.New("webapi: the URL scheme "+u.Scheme()+" is not supported"))}
	}
	var bodyReader io.Reader
	switch b := &rq.body; {
	case b.null:
	case b.stream != nil || b.net != nil:
		s := e.readableOf(b)
		rd, err := e.acquireReader(s, false)
		if err != nil {
			return nil, err
		}
		f.reqBody = &streamBody{e: e, rd: rd, ctx: f.c.ctx, ch: make(chan streamResult, 1)}
		bodyReader = f.reqBody
	default:
		if b.used {
			return nil, e.r.TypeError("Failed to execute 'fetch': the request body has already been read")
		}
		b.used = true
		if len(b.src) > 0 {
			bodyReader = bytes.NewReader(b.src)
		} else {
			bodyReader = http.NoBody
		}
	}
	req, err := http.NewRequestWithContext(f.c.ctx, rq.method, "", bodyReader)
	if err != nil {
		return nil, e.r.TypeError("Failed to execute 'fetch': %s", err.Error())
	}
	href := u.Href()
	if i := strings.IndexByte(href, '#'); i >= 0 {
		href = href[:i]
	}
	if req.URL, err = url.Parse(href); err != nil {
		// What Go's parser rejects (an invalid percent sequence the URL
		// Standard keeps) goes as the opaque request target.
		path := u.Pathname() + u.Search()
		if strings.HasPrefix(path, "//") {
			path = "//" + u.Host() + path
		}
		req.URL = &url.URL{Scheme: u.Scheme(), Host: u.Host(), Opaque: path}
	}
	req.Host = req.URL.Host
	if f.reqBody != nil {
		req.ContentLength = -1
	}
	// The values are slices of one array, each of a capacity of one: a
	// second value of a name appends to a copy.
	list := rq.headers.sortedListRaw()
	vals := make([]string, 0, len(list)+1)
	add := func(k, v string) {
		if old, ok := req.Header[k]; ok {
			req.Header[k] = append(old, v)
			return
		}
		vals = append(vals, v)
		n := len(vals)
		req.Header[k] = vals[n-1 : n : n]
	}
	accept := false
	for _, h := range list {
		switch h.name {
		case "host":
			req.Host = h.value
			continue
		case "content-length":
			continue
		case "accept":
			accept = true
		}
		add(e.canonical(h.name), h.value)
	}
	if !accept {
		add("Accept", "*/*")
	}
	return req, nil
}

// abort is the abort algorithm of a fetch's signal.
func (f *fetchState) abort(reason engine.Value) error {
	e := f.e
	f.aborted = true
	f.c.finish(errAborted)
	f.settle()
	if !f.p.settled() {
		if err := e.rejectP(f.p, reason); err != nil {
			return err
		}
	}
	if rb := f.reqBody; rb != nil && rb.rd.s != nil {
		if _, err := e.cancelStream(rb.rd.s, reason); err != nil {
			return err
		}
	}
	if nb := f.net; nb != nil {
		return nb.abort(reason)
	}
	return nil
}

// settle ends what the fetch keeps until it settles: its listing as work
// in flight (fetchState.ReportLive) and the bytes of its request's body,
// which only the transport may still read.
func (f *fetchState) settle() {
	f.e.live.remove(f)
	f.rq.body.src = nil
}

func (f *fetchState) dropAbort() {
	if f.removeAbort != nil {
		f.removeAbort()
		f.removeAbort = nil
	}
}

// responded settles the fetch with the response or the error of client.Do,
// on the loop.
func (f *fetchState) responded(resp *http.Response, err error) error {
	e := f.e
	f.settle()
	if f.aborted {
		if resp != nil {
			closeBody(resp.Body)
		}
		return nil
	}
	if err == nil && f.c.ctx.Err() != nil {
		// Close or a context stopped the request after its response
		// arrived, before this task ran.
		closeBody(resp.Body)
		err = context.Cause(f.c.ctx)
	}
	if err != nil {
		f.c.finish(err)
		f.dropAbort()
		return e.rejectP(f.p, e.netError("fetch failed", f.c.cause(err)))
	}
	res := &response{typ: "basic", status: resp.StatusCode, statusText: statusText(resp.Status, resp.StatusCode),
		headers: &headers{guard: guardImmutable, src: resp.Header}}
	// Go's client gives a request that a redirect made the response that
	// caused it.
	if rr := resp.Request; rr != nil && rr.Response != nil {
		res.redirected = true
		res.url = rr.URL.String()
	} else {
		res.url = f.rq.url.Href()
		if i := strings.IndexByte(res.url, '#'); i >= 0 {
			res.url = res.url[:i]
		}
	}
	if e.r.MemoryLimit() > 0 {
		// The response keeps its headers, status text and URL as Go
		// strings.
		if cerr := e.r.ChargeMemory(mapBytes(resp.Header) + len(res.statusText) + len(res.url)); cerr != nil {
			closeBody(resp.Body)
			f.c.finish(cerr)
			f.dropAbort()
			if rerr := e.rejectP(f.p, e.limitError(cerr)); rerr != nil {
				return rerr
			}
			return cerr
		}
	}
	if f.rq.method == "HEAD" || nullBodyStatus(resp.StatusCode) {
		closeBody(resp.Body)
		f.c.finish(nil)
		f.dropAbort()
		res.body = body{null: true}
	} else {
		var rc io.ReadCloser = resp.Body
		if !resp.Uncompressed {
			if enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))); enc == "gzip" || enc == "x-gzip" || enc == "deflate" {
				rc = &decoder{enc: enc, src: resp.Body}
			}
		}
		f.c.setBody(rc)
		f.net = &netBody{e: e, f: f, length: resp.ContentLength}
		runtime.AddCleanup(f.net, func(c *conn) { c.finish(errCollected) }, f.c)
		res.body = body{net: f.net}
	}
	o, _ := e.newObject(clsResponse, nil, res)
	return e.fulfill(f.p, engine.ObjectValue(o))
}

// decoder decodes a gzip or deflate body that Go's transport did not: one
// whose request set Accept-Encoding itself. It reads the header on its
// first Read, on the reading goroutine.
type decoder struct {
	enc string
	src io.ReadCloser
	r   io.Reader
}

func (d *decoder) Read(p []byte) (int, error) {
	if d.r == nil {
		if d.enc == "deflate" {
			// "deflate" is the zlib format (RFC 9110); some servers send
			// raw DEFLATE, as browsers accept too.
			var h [2]byte
			n, err := io.ReadFull(d.src, h[:])
			if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
				return 0, err
			}
			src := io.MultiReader(bytes.NewReader(h[:n]), d.src)
			if n == 2 && h[0]&0x0F == 8 && h[0]>>4 <= 7 && (uint16(h[0])<<8|uint16(h[1]))%31 == 0 {
				z, err := zlib.NewReader(src)
				if err != nil {
					return 0, err
				}
				d.r = z
			} else {
				d.r = flate.NewReader(src)
			}
		} else {
			z, err := gzip.NewReader(d.src)
			if err != nil {
				return 0, err
			}
			d.r = z
		}
	}
	return d.r.Read(p)
}

func (d *decoder) Close() error { return d.src.Close() }

// --- response bodies ------------------------------------------------------------

const (
	minChunk = 16 << 10
	maxChunk = 256 << 10
	// minRead is the smallest read buffer under a memory limit (chunkMax).
	minRead = 4 << 10
	// maxPrealloc is the most a whole-body read allocates for a body's
	// announced Content-Length before its bytes arrive.
	maxPrealloc = 1 << 20
)

// chunkMax is the largest buffer a response body is read into, and the
// window of a whole-body read under a memory limit (readAll): maxChunk,
// or an eighth of the runtime's memory limit when that is less, and no
// less than minRead, so that a chunk JavaScript keeps whole is a small
// part of what the limit allows.
func (e *env) chunkMax() int {
	if limit := e.r.MemoryLimit(); limit > 0 && limit/8 < maxChunk {
		return max(int(limit/8), minRead)
	}
	return maxChunk
}

// netBody is the body of a fetch response, read from Go.
type netBody struct {
	e      *env
	f      *fetchState
	stream *readableStream
	// rd reads the body for its stream, from the stream's first pull on.
	rd     *bodyReader
	length int64 // Content-Length, -1 when unknown
	// consume is the promise of a text(), json() or other read of the
	// whole body, while it runs.
	consume  *promise
	reason   engine.Value
	aborted  bool
	finished bool
}

// readable creates the body's stream, a stream of bytes: its source reads
// the body on a goroutine one chunk ahead of what the stream took
// (bodyReader).
func (nb *netBody) readable() *readableStream {
	e := nb.e
	nb.stream = e.newReadable(0, nb.pull, func(engine.Value) (engine.Value, error) {
		nb.end(errCanceled)
		return undefined, nil
	})
	nb.stream.ctrl.bytes = true
	if nb.aborted {
		_ = nb.stream.ctrl.errorWith(nb.reason)
	}
	return nb.stream
}

// end finishes the request and drops the abort algorithm. The stream no
// longer reports the reader: the goroutine drops what it holds.
func (nb *netBody) end(cause error) {
	if nb.stream != nil {
		nb.stream.src = nil
	}
	if !nb.finished {
		nb.finished = true
		nb.f.c.finish(cause)
		nb.f.dropAbort()
	}
}

// abort errors the body with reason.
func (nb *netBody) abort(reason engine.Value) error {
	nb.aborted, nb.reason = true, reason
	nb.end(errAborted)
	if p := nb.consume; p != nil && !p.settled() {
		return nb.e.rejectP(*p, reason)
	}
	if s := nb.stream; s != nil && s.state == rsReadable {
		return s.ctrl.errorWith(reason)
	}
	return nil
}

// terminated is the TypeError of a body that failed after its response.
func (nb *netBody) terminated(err error) engine.Value {
	return nb.e.netError("terminated", nb.f.c.cause(err))
}

// bodyReader reads a body for its stream on a goroutine of its own, one
// chunk ahead of what the stream took: it reads a chunk, which waits until
// the stream takes it, and reads the next while JavaScript works on that
// one, so that the network's wait overlaps JavaScript's work. A chunk is
// what one read returned, everything the connection had, up to the
// buffer's size. The goroutine keeps no reference to the netBody, so that
// a body nobody reads is still collected, which closes the body and stops
// the goroutine.
type bodyReader struct {
	next chan int // the chunk was taken; the largest buffer to read into next

	mu sync.Mutex
	// chunk is the chunk read ahead, what charging it costs, and the
	// read's error; ready while they wait to be taken.
	chunk []byte
	cost  int
	err   error
	ready bool
	// wake, while the stream waits for the chunk, posts its delivery.
	wake func()
	// bufLen is the length of the goroutine's buffer, which it holds
	// while it reads.
	bufLen int
}

// pull takes the chunk read ahead, or waits for the read that runs. The
// first pull starts the goroutine.
func (nb *netBody) pull(rc *rsController) error {
	e := nb.e
	br := nb.rd
	if br == nil {
		body := nb.f.c.startRead()
		if body == nil {
			nb.end(nil)
			return rc.errorWith(nb.terminated(context.Cause(nb.f.c.ctx)))
		}
		br = &bodyReader{next: make(chan int, 1)}
		nb.rd = br
		rc.s.src = br
		go br.run(nb.f.c, body, min(minChunk, e.chunkMax()))
	}
	br.mu.Lock()
	if !br.ready {
		hold := e.loop.Hold()
		br.wake = func() {
			if !e.post(func() error { return nb.take(rc) }) {
				nb.f.c.finish(ErrClosed)
			}
			hold(nil)
		}
		br.mu.Unlock()
		return nil
	}
	br.mu.Unlock()
	return nb.take(rc)
}

// take hands the chunk read ahead to the stream, on the loop, and lets the
// goroutine read the next one meanwhile.
func (nb *netBody) take(rc *rsController) error {
	e := nb.e
	if rc.s.state != rsReadable || rc.pull == nil {
		nb.end(errCanceled)
		return nil
	}
	br := nb.rd
	br.mu.Lock()
	chunk, cost, err := br.chunk, br.cost, br.err
	br.chunk, br.ready = nil, false
	br.mu.Unlock()
	if err == nil {
		br.next <- e.chunkMax()
	}
	if len(chunk) > 0 {
		if cerr := e.r.ChargeMemory(cost); cerr != nil {
			nb.end(cerr)
			_ = rc.errorWith(e.limitError(cerr))
			return cerr
		}
		u, uerr := e.uint8Array(chunk)
		if uerr != nil {
			return uerr
		}
		if eerr := rc.enqueue(u); eerr != nil {
			return eerr
		}
	}
	switch {
	case err == io.EOF:
		nb.end(nil)
		if cerr := rc.close(); cerr != nil {
			return cerr
		}
	case err != nil:
		nb.end(err)
		return rc.errorWith(nb.terminated(err))
	}
	return rc.pulled()
}

// run reads the body until its end, an error, or the end of the request,
// and then marks the read over. A chunk that fills most of the buffer is
// handed over with it, and the next read gets a new buffer, twice as large
// when the chunk filled it, up to the stream's largest (chunkMax); a small
// one is copied, so that the buffer serves the next read.
func (br *bodyReader) run(c *conn, body io.Reader, size int) {
	defer c.endRead()
	var buf []byte
	for {
		if buf == nil {
			buf = make([]byte, size)
			br.mu.Lock()
			br.bufLen = size
			br.mu.Unlock()
		}
		n, err := readSome(body, buf)
		var chunk []byte
		cost := n
		switch {
		case n == 0:
		case 2*n < len(buf):
			chunk = append([]byte(nil), buf[:n]...)
		default:
			chunk, cost = buf[:n:n], len(buf) // JavaScript keeps the whole buffer
			if n == len(buf) {
				size = 2 * len(buf)
			}
			buf = nil
		}
		if err != nil {
			br.put(chunk, cost, err, 0) // the goroutine ends, and drops buf
			return
		}
		br.put(chunk, cost, nil, len(buf))
		select {
		case max := <-br.next:
			size = min(size, max)
		case <-c.ctx.Done():
			// Close, a context or the collector ended the request: the
			// chunk not taken is dropped for the request's error.
			br.put(nil, 0, context.Cause(c.ctx), 0)
			return
		}
	}
}

// put makes chunk, its cost and err the chunk read ahead, and posts its
// delivery when the stream waits for it.
func (br *bodyReader) put(chunk []byte, cost int, err error, bufLen int) {
	br.mu.Lock()
	br.chunk, br.cost, br.err, br.ready, br.bufLen = chunk, cost, err, true, bufLen
	wake := br.wake
	br.wake = nil
	br.mu.Unlock()
	if wake != nil {
		wake()
	}
}

// ReportLive reports the bytes the goroutine holds: its buffer and the
// chunk not yet taken.
func (br *bodyReader) ReportLive(_ func(engine.Value), addBytes func(int)) {
	br.mu.Lock()
	n := br.bufLen
	if br.ready {
		n += br.cost
	}
	br.mu.Unlock()
	addBytes(n)
}

// readSome reads at least one byte or an error. A panic of r's Read is an
// error.
func readSome(r io.Reader, buf []byte) (n int, err error) {
	defer recoverRead(&err)
	for {
		n, err := r.Read(buf)
		if n > 0 || err != nil {
			return n, err
		}
	}
}

// readAll reads the whole body and calls done with it on the loop; a
// failure rejects p. Without a memory limit one goroutine reads it to its
// end and hands it over in one task. Under a limit the bytes are charged
// as they arrive (bodyRead): what Go holds of the body never runs more
// than a window of chunkMax bytes ahead of the charges, and a body
// smaller than a window still costs one task. Whatever the budget, a body
// larger than the limit can never be taken: one whose Content-Length is
// larger rejects at once, and the read of another stops once it passes the
// limit, before it charges the last window, and rejects with a RangeError
// (tooLarge). That is the bound with the loop's
// Options.ResetMemoryPerTask, whose budgets do not add up across tasks.
func (nb *netBody) readAll(p promise, done func([]byte) error) {
	e := nb.e
	nb.consume = &p
	if nb.aborted {
		_ = e.rejectP(p, nb.reason)
		return
	}
	if limit := e.r.MemoryLimit(); limit > 0 && nb.length > limit {
		// The body announces it does not fit.
		nb.end(errTooLarge)
		_ = e.rejectP(p, e.tooLarge())
		return
	}
	body := nb.f.c.startRead()
	if body == nil {
		nb.end(nil)
		_ = e.rejectP(p, nb.terminated(context.Cause(nb.f.c.ctx)))
		return
	}
	size := minChunk
	if nb.length > 0 {
		size = int(min(nb.length, maxPrealloc-1)) + 1
	}
	if limit := e.r.MemoryLimit(); limit > 0 {
		if int64(size) > limit {
			size = int(limit) + 1
		}
		br := &bodyRead{nb: nb, p: p, done: done, limit: limit, window: e.chunkMax()}
		br.buf = make([]byte, 0, size)
		e.live.add(e, br)
		br.read(body)
		return
	}
	hold := e.loop.Hold()
	go func() {
		data, err := readBody(body, make([]byte, 0, size), math.MaxInt, 0)
		nb.f.c.endRead()
		if !e.post(func() error {
			if p.settled() {
				return nil
			}
			if err != io.EOF {
				nb.end(err)
				return e.rejectP(p, nb.terminated(err))
			}
			nb.end(nil)
			// A limit set meanwhile counts the body.
			if cerr := e.r.ChargeMemory(len(data)); cerr != nil {
				if rerr := e.rejectP(p, e.limitError(cerr)); rerr != nil {
					return rerr
				}
				return cerr
			}
			return done(data)
		}) {
			nb.f.c.finish(ErrClosed)
		}
		hold(nil)
	}()
}

// bodyRead is a whole-body read under a memory limit (readAll). Each
// window is read on a goroutine of its own, up to window bytes past what
// was charged or to the body's end, and its bytes are charged in the task
// it then posts, before the next window is read. The read is listed as
// work in flight while it runs: the measurements of the limit see the
// bytes charged, which only Go holds, and the read's promise through it
// (bodyRead.ReportLive), so that a measurement between two windows does
// not drop them from the budget.
type bodyRead struct {
	nb     *netBody
	p      promise
	done   func([]byte) error
	limit  int64
	window int
	// buf holds the bytes read, the first charged of them charged. The
	// goroutine of a window appends to it; the loop uses it only between
	// windows.
	buf     []byte
	charged int
}

// read reads the next window of body on a goroutine, which posts step.
// The window that reaches the limit reads one byte past it instead, so
// that a body larger than the limit is known too large before the window
// is charged.
func (br *bodyRead) read(body io.Reader) {
	nb := br.nb
	e := nb.e
	stop := br.charged + br.window
	if int64(stop) >= br.limit {
		stop = int(br.limit) + 1
	}
	hold := e.loop.Hold()
	go func() {
		var err error
		br.buf, err = readBody(body, br.buf, stop, br.limit)
		nb.f.c.endRead()
		if !e.post(func() error { return br.step(err) }) {
			nb.f.c.finish(ErrClosed)
		}
		hold(nil)
	}()
}

// step charges the window just read, on the loop, and reads the next one
// or hands the body over at its end. A charge that fails aborts the read:
// the body is closed and p rejects with the limit's error.
func (br *bodyRead) step(err error) error {
	nb, p := br.nb, br.p
	e := nb.e
	if p.settled() { // aborted
		e.live.remove(br)
		return nil
	}
	switch {
	case err == errTooLarge:
		// No budget can take the body: a RangeError, as a builtin that
		// knows its size before it allocates throws.
		nb.end(err)
		e.live.remove(br)
		return e.rejectP(p, e.tooLarge())
	case err != nil && err != io.EOF:
		nb.end(err)
		e.live.remove(br)
		return e.rejectP(p, nb.terminated(err))
	}
	if cerr := e.r.ChargeMemory(len(br.buf) - br.charged); cerr != nil {
		nb.end(cerr)
		e.live.remove(br)
		if rerr := e.rejectP(p, e.limitError(cerr)); rerr != nil {
			return rerr
		}
		return cerr
	}
	br.charged = len(br.buf)
	if err == io.EOF {
		nb.end(nil)
		e.live.remove(br)
		return br.done(br.buf)
	}
	body := nb.f.c.startRead()
	if body == nil {
		nb.end(nil)
		e.live.remove(br)
		return e.rejectP(p, nb.terminated(context.Cause(nb.f.c.ctx)))
	}
	br.read(body)
	return nil
}

// errTooLarge stops a read past the memory limit.
var errTooLarge = errors.New("webapi: the body is larger than the memory limit")

// tooLarge is what JavaScript gets for a body read whole that is larger
// than the memory limit.
func (e *env) tooLarge() engine.Value {
	return e.rangeError("The body is larger than the memory limit")
}

// readBody appends what r reads to buf, which it grows, until r's end
// (io.EOF), an error, more than limit bytes when limit is above zero
// (errTooLarge), or stop bytes in buf (nil). A panic of r's Read is an
// error.
func readBody(r io.Reader, buf []byte, stop int, limit int64) (_ []byte, err error) {
	defer recoverRead(&err)
	for len(buf) < stop {
		if len(buf) == cap(buf) {
			n := max(2*cap(buf), minRead)
			if limit > 0 && int64(n) > limit {
				n = int(limit) + 1 // no overflow: limit < n
			}
			buf = slices.Grow(buf, n-len(buf))
		}
		end := min(cap(buf), stop)
		if limit > 0 && int64(end) > limit {
			end = int(limit) + 1 // past it the read stops
		}
		n, err := r.Read(buf[len(buf):end])
		buf = buf[:len(buf)+n]
		switch {
		case limit > 0 && int64(len(buf)) > limit:
			return buf, errTooLarge
		case err != nil:
			return buf, err
		}
	}
	return buf, nil
}

// recoverRead makes a panic on the goroutine of a request or a read, where
// nothing else would recover it, the error of that request or read: an
// *moejs.InternalError, which JavaScript gets as a network error's cause.
func recoverRead(err *error) {
	if x := recover(); x != nil {
		*err = &moejs.InternalError{Value: x, Stack: debug.Stack()}
	}
}

// limitError is what JavaScript gets for a read that a memory limit
// stopped: a RangeError with the limit's message.
func (e *env) limitError(err error) engine.Value {
	var ml *engine.MemoryLimitError
	if errors.As(err, &ml) {
		return e.rangeError(ml.Error())
	}
	return e.rangeError(err.Error())
}

// --- request bodies -------------------------------------------------------------

// streamBody is the body of a request whose body is a ReadableStream: the
// transport's Reads take its chunks from the loop, one at a time.
type streamBody struct {
	e    *env
	rd   *rsReader
	ctx  context.Context
	ch   chan streamResult
	left []byte
	err  error
}

type streamResult struct {
	b   []byte
	err error
}

func (b *streamBody) Read(p []byte) (int, error) {
	for len(b.left) == 0 {
		if b.err != nil {
			return 0, b.err
		}
		if !b.e.post(b.next) {
			b.err = ErrClosed
			continue
		}
		select {
		case res := <-b.ch:
			b.left, b.err = res.b, res.err
		case <-b.ctx.Done():
			b.err = context.Cause(b.ctx)
		}
	}
	n := copy(p, b.left)
	b.left = b.left[n:]
	return n, nil
}

// next reads a chunk on the loop and sends it to Read.
func (b *streamBody) next() error {
	e := b.e
	if b.rd.s == nil {
		b.ch <- streamResult{err: io.EOF}
		return nil
	}
	return e.read(b.rd, readRequest{
		onChunk: func(v engine.Value) error {
			bs, ok := viewBytes(v)
			if !ok || v.AsObject().Class() != engine.ClassTypedArray {
				reason := e.typeError("The request body stream's chunk is not a Uint8Array")
				b.ch <- streamResult{err: errors.New("webapi: a request body chunk is not a Uint8Array")}
				_, err := e.cancelStream(b.rd.s, reason)
				return err
			}
			b.ch <- streamResult{b: append([]byte(nil), bs...)}
			return nil
		},
		onClose: func() error {
			b.ch <- streamResult{err: io.EOF}
			return nil
		},
		onError: func(engine.Value) error {
			b.ch <- streamResult{err: errors.New("webapi: the request body stream errored")}
			return nil
		},
	})
}

// Close cancels the stream when the transport stops reading early.
func (b *streamBody) Close() error {
	b.e.post(func() error {
		if s := b.rd.s; s != nil && s.state == rsReadable {
			_, err := b.e.cancelStream(s, undefined)
			return err
		}
		return nil
	})
	return nil
}
