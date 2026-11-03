package webapi

import (
	"encoding/binary"
	"strconv"
	"strings"
	"unicode/utf8"
	"unsafe"

	"github.com/MoeForge/moejs/engine"
	"github.com/MoeForge/moejs/webapi/internal/whatwgurl"
)

// The Fetch Standard's Body mixin, Request and Response.

// body is the body of a Request or Response. A body given as bytes stays
// bytes, and a fetch response's body stays in Go, until JavaScript asks for
// the stream; text(), json() and the others read them without one.
type body struct {
	src    []byte // the bytes of a body extracted from a string or buffer
	stream *readableStream
	net    *netBody
	ctype  string // the Content-Type of the extraction, "" for none
	null   bool
	hasSrc bool
	used   bool // consumed without a stream
}

func (b *body) isUsed() bool {
	if b.stream != nil {
		return b.stream.disturbed
	}
	return b.used
}

func (b *body) locked() bool { return b.stream != nil && b.stream.locked() }

// readableOf returns the body's stream, creating it from the bytes or the
// network body on first use; nil for a null body. The stream of a body
// that text() or another method consumed, or that a fetch or a new
// Request took, is disturbed and stays locked to the reader that read it.
func (e *env) readableOf(b *body) *readableStream {
	switch {
	case b.null:
		return nil
	case b.stream != nil:
		return b.stream
	case b.used:
		b.stream = e.newReadable(0, nil, nil)
		b.stream.disturbed = true
		_, _ = e.acquireReader(b.stream, false)
	case b.net != nil:
		b.stream = b.net.readable()
	default:
		// The stream takes the bytes over: its source keeps them until it
		// hands their copy over, and reports them meanwhile.
		src := b.src
		b.src = nil
		b.stream = e.newReadable(0, func(c *rsController) error {
			if len(src) > 0 {
				if err := e.r.ChargeMemory(len(src)); err != nil {
					return err
				}
				u, err := e.uint8Array(append([]byte(nil), src...))
				if err != nil {
					return err
				}
				if err := c.enqueue(u); err != nil {
					return err
				}
			}
			return c.close()
		}, nil)
		b.stream.ctrl.bytes = true
		if len(src) > 0 {
			if rep := e.reporter(bytesOf(src)); rep != nil {
				b.stream.held = &[4]engine.Value{engine.ObjectValue(rep)}
			}
		}
	}
	return b.stream
}

// extractBody implements "extract" a body from v.
func (e *env) extractBody(v engine.Value) (body, error) {
	r := e.r
	if v.IsString() {
		s := v.AsString()
		if err := r.ChargeMemory(s.Len()); err != nil {
			return body{}, err
		}
		return body{src: []byte(s.GoString()), hasSrc: true, ctype: "text/plain;charset=UTF-8"}, nil
	}
	if v.IsObject() {
		o := v.AsObject()
		switch x := o.Internal().(type) {
		case *readableStream:
			if x.disturbed || x.locked() {
				return body{}, r.TypeError("The body is a ReadableStream that is locked or disturbed")
			}
			return body{stream: x}, nil
		case *searchParams:
			return body{src: whatwgurl.AppendForm(nil, x.list), hasSrc: true, ctype: "application/x-www-form-urlencoded;charset=UTF-8"}, nil
		}
		switch o.Class() {
		case engine.ClassArrayBuffer, engine.ClassTypedArray, engine.ClassDataView:
			if b, ok := o.BufferData(); ok {
				if err := r.ChargeMemory(len(b)); err != nil {
					return body{}, err
				}
				return body{src: append([]byte{}, b...), hasSrc: true}, nil
			}
		}
		if tag, err := o.GetProp(r, engine.SymbolKey(engine.SymToStringTag)); err != nil {
			return body{}, err
		} else if tag.IsString() {
			switch t := tag.AsString().GoString(); t {
			case "Blob", "File", "FormData":
				return body{}, r.TypeError("A %s body is not supported", t)
			}
		}
	}
	s, err := e.goString(v)
	if err != nil {
		return body{}, err
	}
	if err := r.ChargeMemory(len(s)); err != nil {
		return body{}, err
	}
	return body{src: []byte(s), hasSrc: true, ctype: "text/plain;charset=UTF-8"}, nil
}

// The kinds of body consumption.
const (
	consumeText = iota
	consumeJSON
	consumeArrayBuffer
	consumeBytes
)

// consume implements the body methods text(), json(), arrayBuffer() and
// bytes(): a promise of the whole body. Every byte it hands to JavaScript
// is charged to the runtime's memory limit first, as it arrives: the
// chunks of a stream as the stream hands them over, and their copy as it
// grows; a body larger than the limit can never be taken (readAll).
func (e *env) consume(b *body, kind int) (engine.Value, error) {
	if b.isUsed() || b.locked() {
		return e.rejected(e.typeError("Body is unusable: Body has already been read")), nil
	}
	switch {
	case b.null:
		return e.settleConsumed(kind, nil, true)
	case b.stream == nil && b.net != nil:
		b.used = true
		p := e.newPromise()
		b.net.readAll(p, func(data []byte) error { return e.settle(p, kind, data) })
		return p.value(), nil
	case b.stream == nil:
		// The bytes are copied, and no longer needed.
		src := b.src
		b.used, b.src = true, nil
		if err := e.r.ChargeMemory(len(src)); err != nil {
			return undefined, err
		}
		return e.settleConsumed(kind, src, false)
	}
	// Read the stream's chunks.
	rd, err := e.acquireReader(b.stream, false)
	if err != nil {
		return e.rejectedErr(err)
	}
	p := e.newPromise()
	var data []byte
	limit := e.r.MemoryLimit()
	var cs *consumer
	if limit > 0 {
		cs = &consumer{p: p}
		rd.keep = e.reporter(cs)
	}
	var step func() error
	fail := func(reason engine.Value) error { return e.rejectP(p, reason) }
	step = func() error {
		for {
			again := false
			sync := true
			err := e.read(rd, readRequest{
				onChunk: func(v engine.Value) error {
					bs, ok := viewBytes(v)
					if !ok || v.AsObject().Class() != engine.ClassTypedArray {
						reason := e.typeError("The body stream's chunk is not a Uint8Array")
						if _, err := e.cancelStream(rd.s, reason); err != nil {
							return err
						}
						return fail(reason)
					}
					if limit > 0 && int64(len(data)+len(bs)) > limit {
						// As readAll: a body larger than the limit can
						// never be handed over.
						reason := e.tooLarge()
						if _, err := e.cancelStream(rd.s, reason); err != nil {
							return err
						}
						return fail(reason)
					}
					// The copy is charged as it grows, each byte once.
					if cerr := e.r.ChargeMemory(len(bs)); cerr != nil {
						reason := e.limitError(cerr)
						if _, err := e.cancelStream(rd.s, reason); err != nil {
							return err
						}
						if err := fail(reason); err != nil {
							return err
						}
						return cerr
					}
					data = append(data, bs...)
					if cs != nil {
						cs.n = len(data)
					}
					if sync {
						again = true
						return nil
					}
					return step()
				},
				onClose: func() error { return e.settle(p, kind, data) },
				onError: fail,
			})
			sync = false
			if err != nil || !again {
				return err
			}
		}
	}
	if err := step(); err != nil {
		return undefined, err
	}
	return p.value(), nil
}

// consumer is a whole read of a body's stream (consume) under a memory
// limit, which its internal reader reports (rsReader.keep): while it runs,
// the promise only the read's closures keep and the n bytes copied so far,
// which Go holds.
type consumer struct {
	p promise
	n int
}

func (cs *consumer) ReportLive(add func(engine.Value), addBytes func(int)) {
	if !cs.p.settled() {
		cs.p.liveValues(add)
		addBytes(cs.n)
	}
}

// settle settles p with what kind makes of data, which is p's to give
// away and charged already.
func (e *env) settle(p promise, kind int, data []byte) error {
	v, err := e.consumed(kind, data, true)
	if err != nil {
		reason, ok := e.r.ThrownValue(err)
		if !ok {
			return err
		}
		return e.rejectP(p, reason)
	}
	return e.fulfill(p, v)
}

// settleConsumed returns a promise of what kind makes of data.
func (e *env) settleConsumed(kind int, data []byte, owned bool) (engine.Value, error) {
	v, err := e.consumed(kind, data, owned)
	if err != nil {
		return e.rejectedErr(err)
	}
	return e.resolved(v)
}

// consumed makes the value of kind of data; owned data is the caller's to
// give away (JavaScript may keep and change it). The caller charged the
// bytes; consumed charges what it allocates besides.
func (e *env) consumed(kind int, data []byte, owned bool) (engine.Value, error) {
	switch kind {
	case consumeText:
		s := utf8Text(data, owned)
		if !isASCII(s) {
			// The runtime's string of non-ASCII text is UTF-16.
			if err := e.r.ChargeMemory(2 * len(s)); err != nil {
				return undefined, err
			}
		}
		return str(s), nil
	case consumeJSON:
		return e.r.JSONParseGoString(utf8Text(data, owned))
	}
	if !owned {
		data = append([]byte{}, data...)
	}
	buf, err := e.r.NewArrayBuffer(data)
	if err != nil {
		return undefined, err
	}
	if kind == consumeArrayBuffer {
		return engine.ObjectValue(buf), nil
	}
	return e.uint8ArrayOver(buf)
}

// utf8Text implements "UTF-8 decode" of data to a Go string: the BOM is
// skipped, and each maximal subpart of an invalid sequence is U+FFFD, as
// the runtime's TextDecoder makes them. Owned data is used, not copied.
func utf8Text(data []byte, owned bool) string {
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		data = data[3:]
	}
	if !utf8.Valid(data) {
		data, owned = replaceInvalidUTF8(data), true
	}
	if len(data) == 0 {
		return ""
	}
	if owned {
		// The bytes are not changed again: the string can use them.
		return unsafe.String(&data[0], len(data))
	}
	return string(data)
}

// replaceInvalidUTF8 returns data with each maximal subpart of an invalid
// sequence replaced by U+FFFD.
func replaceInvalidUTF8(data []byte) []byte {
	out := make([]byte, 0, len(data)+len(data)/2)
	for i := 0; i < len(data); {
		if c := data[i]; c < utf8.RuneSelf {
			out = append(out, c)
			i++
			continue
		}
		if r, n := utf8.DecodeRune(data[i:]); r != utf8.RuneError || n > 1 {
			out = append(out, data[i:i+n]...)
			i += n
			continue
		}
		i += maximalSubpart(data[i:])
		out = append(out, "\uFFFD"...)
	}
	return out
}

// maximalSubpart returns the length of the maximal subpart of the invalid
// sequence b starts with: its lead byte and the continuation bytes that
// still fit it, at least one byte (the WHATWG UTF-8 decoder).
func maximalSubpart(b []byte) int {
	c := b[0]
	lo, hi := byte(0x80), byte(0xBF)
	need := 0
	switch {
	case c >= 0xC2 && c <= 0xDF:
		need = 1
	case c >= 0xE0 && c <= 0xEF:
		need = 2
		if c == 0xE0 {
			lo = 0xA0
		} else if c == 0xED {
			hi = 0x9F
		}
	case c >= 0xF0 && c <= 0xF4:
		need = 3
		if c == 0xF0 {
			lo = 0x90
		} else if c == 0xF4 {
			hi = 0x8F
		}
	}
	n := 1
	for n <= need && n < len(b) && b[n] >= lo && b[n] <= hi {
		lo, hi = 0x80, 0xBF
		n++
	}
	return n
}

// isASCII reports whether s is ASCII.
func isASCII(s string) bool {
	for len(s) >= 8 {
		if binary.LittleEndian.Uint64([]byte(s[:8]))&0x8080808080808080 != 0 {
			return false
		}
		s = s[8:]
	}
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// bodyMembers are the members of the Body mixin for a class whose state
// has a body.
func (e *env) bodyMembers(class string, bodyOf func(*engine.Realm, engine.Value, string) (*body, error)) []member {
	consume := func(name string, kind int) member {
		return member{name: name, fn: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
			b, err := bodyOf(r, this, name)
			if err != nil {
				return e.rejectedErr(err)
			}
			return e.consume(b, kind)
		}}
	}
	unsupported := func(name string) member {
		return member{name: name, fn: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
			if _, err := bodyOf(r, this, name); err != nil {
				return e.rejectedErr(err)
			}
			return e.rejected(e.typeError(class + ".prototype." + name + " is not supported")), nil
		}}
	}
	return []member{
		{name: "body", get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
			b, err := bodyOf(r, this, "body")
			if err != nil {
				return undefined, err
			}
			s := e.readableOf(b)
			if s == nil {
				return engine.Null(), nil
			}
			return engine.ObjectValue(s.obj), nil
		}},
		{name: "bodyUsed", get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
			b, err := bodyOf(r, this, "bodyUsed")
			if err != nil {
				return undefined, err
			}
			return engine.Bool(b.isUsed()), nil
		}},
		consume("arrayBuffer", consumeArrayBuffer),
		unsupported("blob"),
		consume("bytes", consumeBytes),
		unsupported("formData"),
		consume("json", consumeJSON),
		consume("text", consumeText),
	}
}

// cloneBody implements "clone" a body: a stream is teed.
func (e *env) cloneBody(b *body) (body, error) {
	c := *b
	if b.null || (b.stream == nil && b.net == nil) {
		return c, nil
	}
	s := e.readableOf(b)
	b1, b2, err := e.tee(s)
	if err != nil {
		return body{}, err
	}
	b.stream, c.stream = b1, b2
	b.net, c.net = nil, nil
	return c, nil
}

// headersObject returns the Headers object of h, creating it on first use.
func (e *env) headersObject(h *headers) engine.Value {
	if h.obj == nil {
		h.obj, _ = e.newObject(clsHeaders, nil, h)
	}
	return engine.ObjectValue(h.obj)
}

// --- Request --------------------------------------------------------------------

const (
	redirectFollow = iota
	redirectError
	redirectManual
)

var redirectModes = []string{"follow", "error", "manual"}

// request is the state of a Request, and what fetch sends.
type request struct {
	method  string
	url     *whatwgurl.URL
	headers *headers
	body    body
	// signal is the signal the request follows; ownSignal, the Request's
	// signal attribute, is created on first read.
	signal    *abortSignal
	ownSignal *abortSignal

	redirect                                 int
	mode, credentials, cache, referrer       string
	referrerPolicy, integrity, duplex, prior string
	keepalive                                bool
}

// newRequest implements the steps of the Request constructor that make
// the request of input and init; fetch calls it with no Request object.
func (e *env) newRequest(input, init engine.Value) (*request, error) {
	r := e.r
	var rq *request
	var inputReq *request
	if input.IsObject() {
		inputReq, _ = input.AsObject().Internal().(*request)
	}
	if inputReq == nil {
		s, err := e.goString(input)
		if err != nil {
			return nil, err
		}
		u, err := e.urlOptions().Parse(s, nil)
		if err != nil {
			return nil, r.TypeError("Failed to parse URL from %s", s)
		}
		if u.Username() != "" || u.Password() != "" {
			return nil, r.TypeError("Request cannot be constructed from a URL that includes credentials: %s", s)
		}
		// The request keeps the URL, a Go string; so with its
		// integrity, referrer and headers, below.
		if err := r.ChargeMemory(len(u.Href())); err != nil {
			return nil, err
		}
		rq = &request{method: "GET", url: u, headers: &headers{guard: guardRequest}, body: body{null: true},
			mode: "cors", credentials: "same-origin", cache: "default", referrer: "about:client"}
	} else {
		c := *inputReq
		rq = &c
		rq.headers = inputReq.headers.clone()
		rq.ownSignal = nil
		if inputReq.ownSignal != nil {
			rq.signal = inputReq.ownSignal
		}
	}
	if init.IsNullish() {
		if inputReq != nil && !inputReq.body.null {
			if inputReq.body.isUsed() || inputReq.body.locked() {
				return nil, r.TypeError("Cannot construct a Request with a Request object that has already been used.")
			}
			// The new request takes the body over.
			inputReq.body = body{used: true, null: false, hasSrc: true}
		}
		return rq, nil
	}
	if !init.IsObject() {
		return nil, r.TypeError("Failed to construct 'Request': The provided value is not of type 'RequestInit'.")
	}
	strMember := func(name string) (string, bool, error) {
		v, err := e.getName(init, name)
		if err != nil || v.IsUndefined() {
			return "", false, err
		}
		s, err := e.goString(v)
		return s, true, err
	}
	enum := func(name string, dst *string, allowed ...string) error {
		s, ok, err := strMember(name)
		if err != nil || !ok {
			return err
		}
		for _, a := range allowed {
			if s == a {
				*dst = s
				return nil
			}
		}
		return r.TypeError("Failed to construct 'Request': The provided value '%s' is not a valid enum value of type Request%s.", s, strings.ToUpper(name[:1])+name[1:])
	}
	if err := enum("cache", &rq.cache, "default", "no-store", "reload", "no-cache", "force-cache", "only-if-cached"); err != nil {
		return nil, err
	}
	if err := enum("credentials", &rq.credentials, "omit", "same-origin", "include"); err != nil {
		return nil, err
	}
	if err := enum("duplex", &rq.duplex, "half"); err != nil {
		return nil, err
	}
	if s, ok, err := strMember("integrity"); err != nil {
		return nil, err
	} else if ok {
		if err := r.ChargeMemory(len(s)); err != nil {
			return nil, err
		}
		rq.integrity = s
	}
	if v, err := e.getName(init, "keepalive"); err != nil {
		return nil, err
	} else if !v.IsUndefined() {
		rq.keepalive = engine.ToBoolean(v)
	}
	if s, ok, err := e.byteStringMember(init, "method"); err != nil {
		return nil, err
	} else if ok {
		if len(s) > maxMethod {
			return nil, r.TypeError("Failed to construct 'Request': the HTTP method is longer than %d bytes.", maxMethod)
		}
		if !isToken(s) {
			return nil, r.TypeError("'%s' is not a valid HTTP method.", s)
		}
		switch up := strings.ToUpper(s); up {
		case "CONNECT", "TRACE", "TRACK":
			return nil, r.TypeError("'%s' HTTP method is unsupported.", s)
		case "DELETE", "GET", "HEAD", "OPTIONS", "POST", "PUT", "PATCH":
			if up != "PATCH" {
				s = up
			}
		}
		rq.method = s
	}
	if err := enum("mode", &rq.mode, "same-origin", "no-cors", "cors"); err != nil {
		return nil, err
	}
	var mode string
	if err := enum("redirect", &mode, redirectModes...); err != nil {
		return nil, err
	}
	for i, m := range redirectModes {
		if m == mode {
			rq.redirect = i
		}
	}
	if s, ok, err := strMember("referrer"); err != nil {
		return nil, err
	} else if ok {
		if err := r.ChargeMemory(len(s)); err != nil {
			return nil, err
		}
		rq.referrer = s
	}
	if err := enum("referrerPolicy", &rq.referrerPolicy, "", "no-referrer", "no-referrer-when-downgrade", "same-origin", "origin", "strict-origin", "origin-when-cross-origin", "strict-origin-when-cross-origin", "unsafe-url"); err != nil {
		return nil, err
	}
	if v, err := e.getName(init, "signal"); err != nil {
		return nil, err
	} else if !v.IsUndefined() {
		if v.IsNull() {
			rq.signal = nil
		} else if s, err := thisAs[*abortSignal](r, v, "Request"); err == nil {
			rq.signal = s
		} else {
			return nil, r.TypeError("Failed to construct 'Request': member signal is not of type AbortSignal.")
		}
	}
	if v, err := e.getName(init, "window"); err != nil {
		return nil, err
	} else if !v.IsNullish() {
		return nil, r.TypeError("Failed to construct 'Request': 'window' can only be null.")
	}
	if v, err := e.getName(init, "headers"); err != nil {
		return nil, err
	} else if !v.IsUndefined() {
		h := &headers{guard: guardRequest}
		if err := e.fillHeaders(h, v); err != nil {
			return nil, err
		}
		rq.headers = h
	}
	bv, err := e.getName(init, "body")
	if err != nil {
		return nil, err
	}
	if !bv.IsNullish() {
		if rq.method == "GET" || rq.method == "HEAD" {
			return nil, r.TypeError("Request with GET/HEAD method cannot have body.")
		}
		b, err := e.extractBody(bv)
		if err != nil {
			return nil, err
		}
		if b.ctype != "" && !rq.headers.has("content-type") {
			rq.headers.append("content-type", b.ctype)
		}
		rq.body = b
	} else if inputReq != nil && !inputReq.body.null {
		if inputReq.body.isUsed() || inputReq.body.locked() {
			return nil, r.TypeError("Cannot construct a Request with a Request object that has already been used.")
		}
		inputReq.body = body{used: true, hasSrc: true}
	} else if bv.IsNull() {
		rq.body = body{null: true}
	}
	return rq, nil
}

// maxMethod is the longest HTTP method a Request takes. The Fetch Standard
// sets no limit, but no real method comes near it, and servers refuse
// request lines much shorter than a long one.
const maxMethod = 4 << 10

// byteStringMember reads the member name of the dictionary d as a
// ByteString.
func (e *env) byteStringMember(d engine.Value, name string) (string, bool, error) {
	v, err := e.getName(d, name)
	if err != nil || v.IsUndefined() {
		return "", false, err
	}
	s, err := e.byteString(v)
	return s, true, err
}

func (e *env) requestSpec() classSpec {
	req := func(r *engine.Realm, this engine.Value, name string) (*request, error) {
		return thisAs[*request](r, this, "Request.prototype."+name)
	}
	get := func(name string, f func(*request) engine.Value) member {
		return member{name: name, get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
			rq, err := req(r, this, name)
			if err != nil {
				return undefined, err
			}
			return f(rq), nil
		}}
	}
	members := []member{
		get("method", func(rq *request) engine.Value { return str(rq.method) }),
		get("url", func(rq *request) engine.Value { return str(rq.url.Href()) }),
		get("headers", func(rq *request) engine.Value { return e.headersObject(rq.headers) }),
		get("destination", func(*request) engine.Value { return str("") }),
		get("referrer", func(rq *request) engine.Value {
			if rq.referrer == "" || rq.referrer == "no-referrer" {
				return str("")
			}
			return str(rq.referrer)
		}),
		get("referrerPolicy", func(rq *request) engine.Value { return str(rq.referrerPolicy) }),
		get("mode", func(rq *request) engine.Value { return str(rq.mode) }),
		get("credentials", func(rq *request) engine.Value { return str(rq.credentials) }),
		get("cache", func(rq *request) engine.Value { return str(rq.cache) }),
		get("redirect", func(rq *request) engine.Value { return str(redirectModes[rq.redirect]) }),
		get("integrity", func(rq *request) engine.Value { return str(rq.integrity) }),
		get("keepalive", func(rq *request) engine.Value { return engine.Bool(rq.keepalive) }),
		get("isReloadNavigation", func(*request) engine.Value { return engine.Bool(false) }),
		get("isHistoryNavigation", func(*request) engine.Value { return engine.Bool(false) }),
		get("duplex", func(*request) engine.Value { return str("half") }),
		get("signal", func(rq *request) engine.Value {
			if rq.ownSignal == nil {
				if rq.signal != nil {
					rq.ownSignal = e.anySignal([]*abortSignal{rq.signal})
				} else {
					rq.ownSignal = e.newSignal()
				}
			}
			return engine.ObjectValue(rq.ownSignal.obj)
		}),
		{name: "clone", fn: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
			rq, err := req(r, this, "clone")
			if err != nil {
				return undefined, err
			}
			if rq.body.isUsed() || rq.body.locked() {
				return undefined, r.TypeError("Request.clone: Body has already been consumed.")
			}
			c := *rq
			c.headers = rq.headers.clone()
			if c.body, err = e.cloneBody(&rq.body); err != nil {
				return undefined, err
			}
			if rq.ownSignal != nil {
				c.signal = rq.ownSignal
			}
			c.ownSignal = nil
			o, _ := e.newObject(clsRequest, nil, &c)
			return engine.ObjectValue(o), nil
		}},
	}
	members = append(members, e.bodyMembers("Request", func(r *engine.Realm, this engine.Value, name string) (*body, error) {
		rq, err := req(r, this, name)
		if err != nil {
			return nil, err
		}
		return &rq.body, nil
	})...)
	return classSpec{
		name: "Request", length: 1, members: members,
		ctor: func(r *engine.Realm, args []engine.Value, newTarget *engine.Object) (engine.Value, error) {
			if len(args) == 0 {
				return undefined, r.TypeError("Failed to construct 'Request': 1 argument required, but only 0 present.")
			}
			rq, err := e.newRequest(args[0], engine.Arg(args, 1))
			if err != nil {
				return undefined, err
			}
			o, err := e.newObject(clsRequest, newTarget, rq)
			if err != nil {
				return undefined, err
			}
			return engine.ObjectValue(o), nil
		},
	}
}

// --- Response -------------------------------------------------------------------

// response is the state of a Response.
type response struct {
	typ        string
	url        string
	redirected bool
	status     int
	statusText string
	headers    *headers
	body       body
}

// nullBodyStatus reports whether a response of status has no body.
func nullBodyStatus(status int) bool {
	switch status {
	case 101, 103, 204, 205, 304:
		return true
	}
	return false
}

func isRedirectStatus(status int) bool {
	switch status {
	case 301, 302, 303, 307, 308:
		return true
	}
	return false
}

// initResponse implements "initialize a response" from init and body.
func (e *env) initResponse(res *response, init engine.Value, b *body) error {
	r := e.r
	res.status, res.typ, res.headers = 200, "default", &headers{guard: guardResponse}
	if !init.IsNullish() {
		if !init.IsObject() {
			return r.TypeError("Failed to construct 'Response': The provided value is not of type 'ResponseInit'.")
		}
		hv, err := e.getName(init, "headers")
		if err != nil {
			return err
		}
		sv, err := e.getName(init, "status")
		if err != nil {
			return err
		}
		if !sv.IsUndefined() {
			n, err := r.ToNumber(sv)
			if err != nil {
				return err
			}
			if n != n || n < 0 || n > 65535 {
				n = 0
			}
			res.status = int(n)
		}
		tv, err := e.getName(init, "statusText")
		if err != nil {
			return err
		}
		if !tv.IsUndefined() {
			if res.statusText, err = e.byteString(tv); err != nil {
				return err
			}
			for i := 0; i < len(res.statusText); i++ {
				if c := res.statusText[i]; c != '\t' && (c < 0x20 || c == 0x7F) {
					return r.TypeError("Failed to construct 'Response': Invalid statusText")
				}
			}
			if err := r.ChargeMemory(len(res.statusText)); err != nil {
				return err
			}
		}
		if !hv.IsUndefined() {
			if err := e.fillHeaders(res.headers, hv); err != nil {
				return err
			}
		}
	}
	if res.status < 200 || res.status > 599 {
		return r.RangeError("Failed to construct 'Response': The status provided (%d) is outside the range [200, 599].", res.status)
	}
	if b == nil {
		res.body = body{null: true}
		return nil
	}
	if nullBodyStatus(res.status) {
		return r.TypeError("Response with null body status cannot have body")
	}
	res.body = *b
	if b.ctype != "" && !res.headers.has("content-type") {
		res.headers.append("content-type", b.ctype)
	}
	return nil
}

func (e *env) responseSpec() classSpec {
	resp := func(r *engine.Realm, this engine.Value, name string) (*response, error) {
		return thisAs[*response](r, this, "Response.prototype."+name)
	}
	get := func(name string, f func(*response) engine.Value) member {
		return member{name: name, get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
			res, err := resp(r, this, name)
			if err != nil {
				return undefined, err
			}
			return f(res), nil
		}}
	}
	members := []member{
		get("type", func(res *response) engine.Value { return str(res.typ) }),
		get("url", func(res *response) engine.Value { return str(res.url) }),
		get("redirected", func(res *response) engine.Value { return engine.Bool(res.redirected) }),
		get("status", func(res *response) engine.Value { return engine.Int64Value(int64(res.status)) }),
		get("ok", func(res *response) engine.Value { return engine.Bool(res.status >= 200 && res.status <= 299) }),
		get("statusText", func(res *response) engine.Value { return byteValue(res.statusText) }),
		get("headers", func(res *response) engine.Value { return e.headersObject(res.headers) }),
		{name: "clone", fn: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
			res, err := resp(r, this, "clone")
			if err != nil {
				return undefined, err
			}
			if res.body.isUsed() || res.body.locked() {
				return undefined, r.TypeError("Response.clone: Body has already been consumed.")
			}
			c := *res
			c.headers = res.headers.clone()
			if c.body, err = e.cloneBody(&res.body); err != nil {
				return undefined, err
			}
			o, _ := e.newObject(clsResponse, nil, &c)
			return engine.ObjectValue(o), nil
		}},
	}
	members = append(members, e.bodyMembers("Response", func(r *engine.Realm, this engine.Value, name string) (*body, error) {
		res, err := resp(r, this, name)
		if err != nil {
			return nil, err
		}
		return &res.body, nil
	})...)
	return classSpec{
		name: "Response", members: members,
		ctor: func(r *engine.Realm, args []engine.Value, newTarget *engine.Object) (engine.Value, error) {
			res := &response{}
			var b *body
			if v := engine.Arg(args, 0); !v.IsNullish() {
				x, err := e.extractBody(v)
				if err != nil {
					return undefined, err
				}
				b = &x
			}
			if err := e.initResponse(res, engine.Arg(args, 1), b); err != nil {
				return undefined, err
			}
			o, err := e.newObject(clsResponse, newTarget, res)
			if err != nil {
				return undefined, err
			}
			return engine.ObjectValue(o), nil
		},
		statics: []member{
			{name: "error", fn: func(r *engine.Realm, _ engine.Value, _ []engine.Value) (engine.Value, error) {
				res := &response{typ: "error", headers: &headers{guard: guardImmutable}, body: body{null: true}}
				o, _ := e.newObject(clsResponse, nil, res)
				return engine.ObjectValue(o), nil
			}},
			{name: "json", length: 1, fn: func(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
				s, err := r.JSONStringify(engine.Arg(args, 0))
				if err != nil {
					return undefined, err
				}
				if s == nil {
					return undefined, r.TypeError("Response.json: the data is not JSON serializable")
				}
				if err := r.ChargeMemory(s.Len()); err != nil {
					return undefined, err
				}
				b := body{src: []byte(s.GoString()), hasSrc: true, ctype: "application/json"}
				res := &response{}
				if err := e.initResponse(res, engine.Arg(args, 1), &b); err != nil {
					return undefined, err
				}
				o, _ := e.newObject(clsResponse, nil, res)
				return engine.ObjectValue(o), nil
			}},
			{name: "redirect", length: 1, fn: func(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
				s, err := e.goString(engine.Arg(args, 0))
				if err != nil {
					return undefined, err
				}
				u, err := e.urlOptions().Parse(s, nil)
				if err != nil {
					return undefined, r.TypeError("Failed to execute 'redirect' on 'Response': Invalid URL")
				}
				status := 302
				if v := engine.Arg(args, 1); !v.IsUndefined() {
					n, err := r.ToNumber(v)
					if err != nil {
						return undefined, err
					}
					status = int(n)
				}
				if !isRedirectStatus(status) {
					return undefined, r.RangeError("Failed to execute 'redirect' on 'Response': Invalid status code")
				}
				h := &headers{guard: guardImmutable}
				h.list = append(h.list, hdr{"location", u.Href()})
				res := &response{typ: "default", status: status, headers: h, body: body{null: true}}
				o, _ := e.newObject(clsResponse, nil, res)
				return engine.ObjectValue(o), nil
			}},
		},
	}
}

// statusText returns the reason phrase of a Go status line.
func statusText(status string, code int) string {
	return strings.TrimPrefix(status, strconv.Itoa(code)+" ")
}
