package engines_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/grafana/sobek"
	"github.com/grafana/sobek/parser"
	"github.com/stretchr/testify/assert"
)

// tlaDriver evaluates a module on one engine's module API. Its log gets what
// a promiseDriver's does, and "M <state> <result>" for the evaluation when it
// returns: the promise's on Sobek, Load's outcome natively.
// A result that is an exception, as a rejection's is, shows as "exc" and the
// thrown value.
type tlaDriver interface {
	evaluate(name, src string)
	settle(h int, reject bool, v any)
	final() []string
	// module is "M <state> <result>" of the evaluation now, "" when the API
	// does not show it (native Load reports the outcome once).
	module() string
}

type sobekTLADriver struct {
	*sobekPromiseDriver
	p *sobek.Promise
}

func (d *sobekTLADriver) evaluate(name, src string) {
	noImports := func(any, string) (sobek.ModuleRecord, error) { return nil, errors.New("no imports") }
	rec, err := sobek.ParseModule(name, src, noImports, parser.WithDisableSourceMaps)
	if err == nil {
		err = rec.Link()
	}
	if err != nil {
		d.lines = append(d.lines, "error")
		return
	}
	d.p = d.rt.CyclicModuleRecordEvaluate(rec, noImports)
	d.lines = append(d.lines, d.module())
}

func (d *sobekTLADriver) module() string {
	var result any
	if d.p.Result() != nil {
		result = d.p.Result().Export()
		if ex, ok := result.(*sobek.Exception); ok {
			result = "exc " + ex.Value().String()
		}
	}
	return tlaLine(int(d.p.State()), result)
}

type moejsTLADriver struct {
	*moejsPromiseDriver
}

func (d *moejsTLADriver) evaluate(name, src string) {
	mod, err := moejs.Compile(name, src)
	if err != nil {
		d.lines = append(d.lines, "error")
		return
	}
	var exc *moejs.Exception
	switch err := d.rt.Load(mod); {
	case err == nil:
		d.lines = append(d.lines, tlaLine(int(moejs.PromiseFulfilled), nil))
	case errors.Is(err, moejs.ErrModulePending):
		d.lines = append(d.lines, tlaLine(int(moejs.PromisePending), nil))
	case errors.As(err, &exc):
		thrown, _, _ := strings.Cut(exc.Error(), "\n") // no stack
		d.lines = append(d.lines, tlaLine(int(moejs.PromiseRejected), "exc "+thrown))
	default:
		d.lines = append(d.lines, "error")
	}
}

func (d *moejsTLADriver) module() string { return "" }

func tlaLine(state int, result any) string {
	var l promiseLog
	l.promise(nil, state, result)
	return "M" + l.lines[0][2:]
}

// TestTopLevelAwaitMatchesSobek evaluates the same modules with top-level
// await on Sobek (ParseModule, Link, CyclicModuleRecordEvaluate) and native
// Load, then settles host promises, and compares the logs, what the
// rejection tracker saw and the evaluation's outcome.
func TestTopLevelAwaitMatchesSobek(t *testing.T) {
	type settle struct {
		h      int
		reject bool
		v      any
	}
	cases := []struct {
		name    string
		src     string
		settles []settle
		want    []string // Sobek's log
		// native is the log of native Load where it deviates from Sobek's.
		native []string
		module string // Sobek's evaluation at the end
	}{
		{name: "fulfilled", src: `log("a"); await null; log("b");`, want: []string{"a", "b", "M 1 <nil>"}},
		{name: "no await", src: `log("a");`, want: []string{"a", "M 1 <nil>"}},
		{name: "rejected after await", src: `await 0; throw 5;`, want: []string{"R0", "M 2 exc 5"}},
		// Sobek runs the body of an async module before it reacts to the
		// body's promise, so a throw before the first await rejects that
		// promise unhandled, then handles it (R0 H0), and the evaluation's
		// promise one job later (R1). Native Load has no promise but the
		// body's: the tracker sees its rejection only.
		{name: "throws before await", src: `throw 6; await 0;`,
			want: []string{"R0", "H0", "R1", "M 2 exc 6"}, native: []string{"R0", "M 2 exc 6"}},
		{name: "error before await", src: `export const a = 1; throw new Error("early boom"); await null;`,
			want:   []string{"R0", "H0", "R1", "M 2 exc Error: early boom"},
			native: []string{"R0", "M 2 exc Error: early boom"}},
		{name: "error after await", src: `export const a = 1; await null; throw new Error("late boom");`,
			want: []string{"R0", "M 2 exc Error: late boom"}},
		{name: "awaits a rejected error", src: `await Promise.reject(new TypeError("awaited boom"));`,
			want: []string{"R0", "H0", "R1", "M 2 exc TypeError: awaited boom"}},
		{name: "pending", src: `log("a"); await new Promise(() => {}); log("b");`, want: []string{"a", "M 0 <nil>"}},
		{name: "handled after await", src: `const p = Promise.reject(1); await null; p.catch(() => log("c"));`},
		{name: "awaits a rejection", src: `await Promise.reject(2);`},
		{name: "catches a rejection", src: `try { await Promise.reject(3) } catch (e) { log("c" + e) } log("end");`},
		{name: "async function", src: `async function f() { await null; log("f"); return 4 } log("v" + await f());`},
		{name: "microtask order", src: `Promise.resolve().then(() => log("job")); log("sync"); await null; log("after");`},
		{name: "host fulfills",
			src:     `globalThis.p = hostPromise(); log("v " + await p);`,
			settles: []settle{{0, false, "s"}},
			want:    []string{"M 0 <nil>", "v s", "P0 1 s"}, module: "M 1 <nil>"},
		{name: "host rejects",
			src:     `globalThis.p = hostPromise(); try { await p } catch (e) { log("c " + e) } throw "t";`,
			settles: []settle{{0, true, "u"}}},
		{name: "host rejects unhandled",
			src:     `globalThis.p = hostPromise(); await p;`,
			settles: []settle{{0, true, "w"}}},
	}
	drivers := []struct {
		name string
		new  func() tlaDriver
	}{
		{"sobek", func() tlaDriver {
			return &sobekTLADriver{sobekPromiseDriver: newSobekPromiseDriver(nil).(*sobekPromiseDriver)}
		}},
		{"moejs", func() tlaDriver {
			return &moejsTLADriver{moejsPromiseDriver: newMoejsPromiseDriver(nil).(*moejsPromiseDriver)}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var reference []string
			var module string
			for _, dr := range drivers {
				d := dr.new()
				d.evaluate("tla.js", c.src)
				for _, s := range c.settles {
					d.settle(s.h, s.reject, s.v)
				}
				got, m := d.final(), d.module()
				if reference == nil {
					reference, module = got, m
					assert.NotContains(t, got, "error", "sobek")
					if c.want != nil {
						assert.Equal(t, c.want, got, "sobek")
					}
					if c.module != "" {
						assert.Equal(t, c.module, m, "sobek")
					}
					continue
				}
				if want := reference; dr.name != "moejs" || c.native == nil {
					assert.Equal(t, want, got, dr.name)
				} else {
					assert.Equal(t, c.native, got, dr.name)
				}
				if m != "" {
					assert.Equal(t, module, m, dr.name)
				}
			}
		})
	}
}
