// Package whatwgurl implements the URL Standard (https://url.spec.whatwg.org/):
// the basic URL parser with the state overrides of the URL API's setters,
// the host parser with domain to ASCII (UTS #46, Unicode 17.0.0), the URL
// serializer, origins, and application/x-www-form-urlencoded.
//
// Inputs are strings of scalar values in valid UTF-8; a caller holding
// UTF-16 replaces lone surrogates with U+FFFD before it calls in.
//
// The parser works on the bytes of the UTF-8 input. That is equivalent to the
// standard's code points: no code point above U+007F is a delimiter, and
// every percent-encode set holds them all, so UTF-8 percent-encoding a code
// point percent-encodes its bytes.
package whatwgurl

import (
	"errors"
	"math"
	"strings"
)

// ErrInvalid is the error of a parse that fails.
var ErrInvalid = errors.New("whatwgurl: invalid URL")

// URL is a URL record. Its fields are kept as the serialization and the
// offsets of the components in it, so the getters slice and do not allocate.
type URL struct {
	href string
	// href[:schemeEnd] is the scheme. With a host, href[schemeEnd+3:userEnd]
	// is the username, href[userEnd+1:passEnd] the password (passEnd ==
	// userEnd when it is empty), href[hostStart:hostEnd] the host and
	// href[hostEnd+1:pathStart] the port, when pathStart > hostEnd.
	schemeEnd, userEnd, passEnd, hostStart, hostEnd uint32
	// href[pathStart:] holds the path, then the query after the '?' at
	// queryStart and the fragment after the '#' at fragStart.
	pathStart, queryStart, fragStart uint32
	flags                            uint8
	opts                             *Options
}

const (
	flagHost uint8 = 1 << iota
	flagOpaque
	flagQuery
	flagFragment
)

// record is a URL record with its components as strings, which the parser
// and the setters work on.
type record struct {
	scheme, username, password string
	host                       string // serialized
	hasHost                    bool
	port                       string // serialized; "" when null
	// path is the URL path serialized: "/" before every segment. Segments
	// hold no '/', so this keeps the list. With opaque, it is the opaque path.
	path     string
	opaque   bool
	query    string
	hasQuery bool
	fragment string
	hasFrag  bool
}

// Parse runs the basic URL parser on input against base (nil: no base) with
// the zero Options.
func Parse(input string, base *URL) (*URL, error) {
	return (*Options)(nil).Parse(input, base)
}

// Parse runs the basic URL parser on input against base (nil: no base) and
// returns the URL record, or ErrInvalid for failure. The URL keeps o for its
// setters.
func (o *Options) Parse(input string, base *URL) (*URL, error) {
	in := trimControlOrSpace(input)
	in = removeTabOrNewline(in)
	var r record
	var br *record
	if base != nil {
		b := base.record()
		br = &b
	}
	if !o.basicParse(in, br, &r, stateNone) {
		return nil, ErrInvalid
	}
	u := &URL{opts: o}
	if !u.set(&r, in) {
		return nil, ErrInvalid
	}
	return u, nil
}

// trimControlOrSpace removes leading and trailing C0 controls and spaces.
func trimControlOrSpace(s string) string {
	i, j := 0, len(s)
	for i < j && s[i] <= ' ' {
		i++
	}
	for j > i && s[j-1] <= ' ' {
		j--
	}
	return s[i:j]
}

// removeTabOrNewline removes every ASCII tab or newline.
func removeTabOrNewline(s string) string {
	if strings.IndexAny(s, "\t\n\r") < 0 {
		return s
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if c := s[i]; c != '\t' && c != '\n' && c != '\r' {
			b = append(b, c)
		}
	}
	return string(b)
}

// hrefWriter writes a serialization, or compares it with cmp when sb is nil.
// The length n is int64 so that the components of a record, each a string,
// cannot sum past it on 32-bit systems.
type hrefWriter struct {
	sb  *strings.Builder
	cmp string
	n   int64
	eq  bool
}

// maxHref bounds a serialization: its offsets are uint32, and on 32-bit
// systems a string holds at most math.MaxInt bytes.
const maxHref = min(1<<32-1, math.MaxInt)

func (w *hrefWriter) put(s string) {
	if w.sb != nil {
		w.sb.WriteString(s)
	} else if w.eq {
		end := w.n + int64(len(s))
		w.eq = end <= int64(len(w.cmp)) && w.cmp[w.n:end] == s
	}
	w.n += int64(len(s))
}

// serialize runs the URL serializer on r into w and records the offsets in
// u.
func (u *URL) serialize(r *record, w *hrefWriter) {
	at := func() uint32 { return uint32(w.n) }
	w.put(r.scheme)
	u.schemeEnd = at()
	w.put(":")
	u.flags = 0
	if r.hasHost {
		u.flags |= flagHost
		w.put("//")
		w.put(r.username)
		u.userEnd = at()
		if r.password != "" {
			w.put(":")
			w.put(r.password)
		}
		u.passEnd = at()
		if r.username != "" || r.password != "" {
			w.put("@")
		}
		u.hostStart = at()
		w.put(r.host)
		u.hostEnd = at()
		if r.port != "" {
			w.put(":")
			w.put(r.port)
		}
	} else {
		u.userEnd, u.passEnd, u.hostStart, u.hostEnd = at(), at(), at(), at()
		if !r.opaque && strings.HasPrefix(r.path, "//") {
			w.put("/.")
		}
	}
	if r.opaque {
		u.flags |= flagOpaque
	}
	u.pathStart = at()
	w.put(r.path)
	u.queryStart = at()
	if r.hasQuery {
		u.flags |= flagQuery
		w.put("?")
		w.put(r.query)
	}
	u.fragStart = at()
	if r.hasFrag {
		u.flags |= flagFragment
		w.put("#")
		w.put(r.fragment)
	}
}

// set makes r u's record, reusing in for the serialization when it is the
// same. It reports false, leaving u as it was, when the serialization is
// longer than maxHref.
func (u *URL) set(r *record, in string) bool {
	nu := URL{opts: u.opts}
	w := hrefWriter{cmp: in, eq: true}
	nu.serialize(r, &w)
	if w.n > maxHref {
		return false
	}
	if w.eq && w.n == int64(len(in)) {
		nu.href = in
	} else {
		var sb strings.Builder
		sb.Grow(int(w.n))
		nu.serialize(r, &hrefWriter{sb: &sb})
		nu.href = sb.String()
	}
	*u = nu
	return true
}

// record returns u's record, sliced from the serialization.
func (u *URL) record() record {
	h := u.href
	r := record{scheme: h[:u.schemeEnd]}
	if u.flags&flagHost != 0 {
		r.hasHost = true
		r.username = h[u.schemeEnd+3 : u.userEnd]
		if u.passEnd > u.userEnd {
			r.password = h[u.userEnd+1 : u.passEnd]
		}
		r.host = h[u.hostStart:u.hostEnd]
		if u.pathStart > u.hostEnd {
			r.port = h[u.hostEnd+1 : u.pathStart]
		}
	}
	r.path = h[u.pathStart:u.queryStart]
	r.opaque = u.flags&flagOpaque != 0
	if u.flags&flagQuery != 0 {
		r.hasQuery = true
		r.query = h[u.queryStart+1 : u.fragStart]
	}
	if u.flags&flagFragment != 0 {
		r.hasFrag = true
		r.fragment = h[u.fragStart+1:]
	}
	return r
}

// Clone returns a copy of u.
func (u *URL) Clone() *URL {
	c := *u
	return &c
}

// Href returns the serialization of u.
func (u *URL) Href() string { return u.href }

// String returns the serialization of u.
func (u *URL) String() string { return u.href }

// Scheme returns u's scheme.
func (u *URL) Scheme() string { return u.href[:u.schemeEnd] }

// Protocol returns u's scheme followed by ':'.
func (u *URL) Protocol() string { return u.href[:u.schemeEnd+1] }

// Username returns u's username.
func (u *URL) Username() string {
	if u.flags&flagHost == 0 {
		return ""
	}
	return u.href[u.schemeEnd+3 : u.userEnd]
}

// Password returns u's password.
func (u *URL) Password() string {
	if u.passEnd == u.userEnd {
		return ""
	}
	return u.href[u.userEnd+1 : u.passEnd]
}

// Host returns u's host, serialized, and ':' and the port when it has one;
// "" when the host is null.
func (u *URL) Host() string {
	if u.flags&flagHost == 0 {
		return ""
	}
	return u.href[u.hostStart:u.pathStart]
}

// Hostname returns u's host, serialized; "" when it is null.
func (u *URL) Hostname() string { return u.href[u.hostStart:u.hostEnd] }

// Port returns u's port, serialized; "" when it is null.
func (u *URL) Port() string {
	if u.flags&flagHost == 0 || u.pathStart == u.hostEnd {
		return ""
	}
	return u.href[u.hostEnd+1 : u.pathStart]
}

// Pathname returns u's path, serialized.
func (u *URL) Pathname() string { return u.href[u.pathStart:u.queryStart] }

// Search returns '?' and u's query, or "" when it is null or empty.
func (u *URL) Search() string {
	if u.fragStart-u.queryStart <= 1 {
		return ""
	}
	return u.href[u.queryStart:u.fragStart]
}

// Query returns u's query; ok is false when it is null.
func (u *URL) Query() (q string, ok bool) {
	if u.flags&flagQuery == 0 {
		return "", false
	}
	return u.href[u.queryStart+1 : u.fragStart], true
}

// Hash returns '#' and u's fragment, or "" when it is null or empty.
func (u *URL) Hash() string {
	if len(u.href)-int(u.fragStart) <= 1 {
		return ""
	}
	return u.href[u.fragStart:]
}

// Fragment returns u's fragment; ok is false when it is null.
func (u *URL) Fragment() (f string, ok bool) {
	if u.flags&flagFragment == 0 {
		return "", false
	}
	return u.href[u.fragStart+1:], true
}

// HasOpaquePath reports whether u's path is opaque.
func (u *URL) HasOpaquePath() bool { return u.flags&flagOpaque != 0 }

// Origin returns the ASCII serialization of u's origin (HTML §7.1.1), "null"
// for an opaque origin.
func (u *URL) Origin() string {
	switch u.Scheme() {
	case "ftp", "http", "https", "ws", "wss":
		if u.hostStart == u.schemeEnd+3 {
			return u.href[:u.pathStart]
		}
		return u.href[:u.schemeEnd+3] + u.href[u.hostStart:u.pathStart]
	case "blob":
		p, err := u.opts.Parse(u.Pathname(), nil)
		if err == nil && (p.Scheme() == "http" || p.Scheme() == "https") {
			return p.Origin()
		}
	}
	return "null"
}
