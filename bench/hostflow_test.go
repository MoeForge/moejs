package bench

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/bench/engines"
)

// The host flow reproduces new-api's plugin host around one hook call, per
// engine, so the native API can be compared with the Sobek-shaped one on
// what the host does, not only on the call:
//
//   - today (sobek): every argument deep-copied like jsonValue, ToValue,
//     call, Export, then by hook kind: Marshal + Unmarshal into the
//     adaptor's struct (convert), the exported map as is (usage facts),
//     Marshal to bytes (render hooks, written to the client) or Marshal +
//     Unmarshal into any (other hooks).
//   - native (moejs): FromGo without copies, Call, then AppendJSON +
//     Unmarshal into the same struct, ToGo (usage facts), AppendJSON (render)
//     or AppendJSON + Unmarshal into any.
//
// in=bytes starts from each argument's JSON bytes, as the host holds stored
// task data and protocol state: Unmarshal + ToValue today, ParseJSON native.

// Mirrors of relay/channel/task/jsplugin's result structs (same json tags).
type flowRequestDescriptor struct {
	ResponseType   string            `json:"responseType"`
	URL            string            `json:"url"`
	Method         string            `json:"method"`
	Headers        map[string]string `json:"headers"`
	Body           any               `json:"body"`
	Credentialless bool              `json:"credentialless"`
	Action         string            `json:"action"`
	Model          string            `json:"model"`
	RewriteModel   string            `json:"rewriteModel"`
	BodyType       string            `json:"bodyType"`
	Parts          []flowRequestPart `json:"parts"`
}

type flowRequestPart struct {
	Name     string `json:"name"`
	Value    any    `json:"value"`
	FileRef  string `json:"fileRef"`
	Filename string `json:"filename"`
}

type flowSubmitResponse struct {
	TaskID    string          `json:"taskId"`
	TaskData  any             `json:"taskData"`
	Immediate *flowTaskResult `json:"immediate"`
	State     any             `json:"state"`
}

type flowTaskResult struct {
	Code             int     `json:"code"`
	TaskID           string  `json:"taskId"`
	Status           string  `json:"status"`
	Progress         string  `json:"progress"`
	Reason           string  `json:"reason"`
	URL              string  `json:"url"`
	RemoteURL        string  `json:"remoteUrl"`
	CompletionTokens float64 `json:"completionTokens"`
	TotalTokens      float64 `json:"totalTokens"`
	State            any     `json:"state"`
}

type flowBatchItem struct {
	TaskID     string `json:"taskId"`
	Action     string `json:"action"`
	Status     string `json:"status"`
	Progress   string `json:"progress"`
	Reason     string `json:"reason"`
	URL        string `json:"url"`
	SubmitTime int64  `json:"submitTime"`
	StartTime  int64  `json:"startTime"`
	FinishTime int64  `json:"finishTime"`
	Data       any    `json:"data"`
	State      any    `json:"state"`
}

// flowKind is what the host does with a hook's result.
type flowKind string

const (
	flowDescriptor flowKind = "descriptor" // build*Request -> requestDescriptor
	flowSubmit     flowKind = "submit"     // parseSubmitResponse -> submitResponse
	flowTask       flowKind = "task"       // parseTaskResult, parseBatchResult -> taskResult, []batch item
	flowUsage      flowKind = "usage"      // extractUsage* -> map[string]any
	flowRender     flowKind = "render"     // render hooks -> JSON bytes
	flowOther      flowKind = "other"      // -> any
)

var flowKinds = []flowKind{flowDescriptor, flowSubmit, flowTask, flowUsage, flowRender, flowOther}

func hostFlowKind(c FixtureCase) flowKind {
	switch {
	case strings.HasPrefix(c.Hook, "build") && strings.HasSuffix(c.Hook, "Request"):
		return flowDescriptor
	case c.Hook == "parseSubmitResponse":
		return flowSubmit
	case c.Hook == "parseTaskResult", c.Hook == "parseBatchResult":
		return flowTask
	case strings.HasPrefix(c.Hook, "extractUsage"):
		return flowUsage
	case strings.Contains(strings.ToLower(c.HookName()), "render"):
		return flowRender
	}
	return flowOther
}

// hostFlow runs one case through one engine's host path.
type hostFlow struct {
	h      *HookCase
	kind   flowKind
	rt     engines.Phased
	native *moejs.Runtime // nil: today's path
	bytes  bool
	buf    []byte
}

func newHostFlow(h *HookCase, rt engines.Runtime, bytes bool) *hostFlow {
	f := &hostFlow{h: h, kind: hostFlowKind(h.Case), rt: rt.(engines.Phased), bytes: bytes}
	if m, ok := rt.(*engines.MoejsRuntime); ok {
		f.native = m.Moejs()
	}
	return f
}

func (f *hostFlow) args() ([]any, error) {
	if f.native != nil {
		if !f.bytes {
			return f.h.Args, nil
		}
		out := make([]any, len(f.h.Case.Args))
		for i, raw := range f.h.Case.Args {
			v, err := f.native.ParseJSON(raw)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	}
	out := make([]any, len(f.h.Args))
	for i := range out {
		if f.bytes {
			if err := json.Unmarshal(f.h.Case.Args[i], &out[i]); err != nil {
				return nil, err
			}
			continue
		}
		v, ok := cloneJSON(f.h.Args[i], 0)
		if !ok {
			return nil, fmt.Errorf("argument %d is not JSON-shaped", i)
		}
		out[i] = v
	}
	return out, nil
}

// run returns the host-side result: a struct pointer, a map, JSON bytes or
// a decoded any, by kind.
func (f *hostFlow) run() (any, error) {
	args, err := f.args()
	if err != nil {
		return nil, err
	}
	prepared, err := f.rt.Prepare(args...)
	if err != nil {
		return nil, err
	}
	raw, err := f.rt.CallPrepared(f.h.Case.Hook, f.h.Case.HookPath(), prepared)
	if err != nil {
		return nil, err
	}
	if f.kind == flowUsage {
		if f.native != nil {
			return f.native.ToGo(raw.(moejs.Value))
		}
		return f.rt.Export(raw), nil
	}
	var data []byte
	if f.native != nil {
		if f.buf, err = f.native.AppendJSON(f.buf[:0], raw.(moejs.Value)); err != nil {
			return nil, err
		}
		data = f.buf
	} else if data, err = json.Marshal(f.rt.Export(raw)); err != nil {
		return nil, err
	}
	var target any
	switch f.kind {
	case flowRender:
		return data, nil
	case flowDescriptor:
		target = new(flowRequestDescriptor)
	case flowSubmit:
		target = new(flowSubmitResponse)
	case flowTask:
		if f.h.Case.Hook == "parseBatchResult" {
			target = new([]flowBatchItem)
		} else {
			target = new(flowTaskResult)
		}
	default:
		target = new(any)
	}
	return target, json.Unmarshal(data, target)
}

// cloneJSON mirrors new-api's cloneJSONValue: containers copied, integers
// widened to float64, strings validated.
func cloneJSON(v any, depth int) (any, bool) {
	if depth > 64 {
		return nil, false
	}
	switch t := v.(type) {
	case nil, bool, float64:
		return t, true
	case string:
		return t, utf8.ValidString(t)
	case int64:
		return float64(t), true
	case int:
		return float64(t), true
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			c, ok := cloneJSON(item, depth+1)
			if !ok {
				return nil, false
			}
			out[k] = c
		}
		return out, true
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			c, ok := cloneJSON(item, depth+1)
			if !ok {
				return nil, false
			}
			out[i] = c
		}
		return out, true
	}
	return nil, false
}

var hostFlowEngines = []func() engines.Engine{
	func() engines.Engine { return engines.NewMoejsEngine() },
	func() engines.Engine { return engines.NewSobekEngine() },
}

// hostFlowCases returns the cases the flow covers: every case that does not
// expect a throw.
func hostFlowCases(tb testing.TB) []*HookCase {
	cases := allCases(tb)
	var out []*HookCase
	for i := range cases {
		if cases[i].Case.ExpectedError == "" {
			out = append(out, &cases[i])
		}
	}
	return out
}

// flowComparable normalizes a flow result for comparison across engines: JSON
// bytes are decoded (map key order differs between Marshal and stringify).
func flowComparable(v any) any {
	if b, ok := v.([]byte); ok {
		var out any
		if err := json.Unmarshal(b, &out); err != nil {
			return err.Error()
		}
		return out
	}
	return v
}

// TestHostFlowEquivalence checks that the native host path decodes every
// case to the same Go result as today's Sobek path, for both input forms.
func TestHostFlowEquivalence(t *testing.T) {
	skipWithoutPlugins(t)
	cases := hostFlowCases(t)
	type column struct {
		name    string
		results []any
	}
	var cols []column
	for _, mk := range hostFlowEngines {
		e := mk()
		rts := pluginRuntimes(t, mustRunner(t, e))
		for _, bytes := range []bool{false, true} {
			col := column{name: e.Name() + "/in=" + flowInput(bytes)}
			for _, h := range cases {
				got, err := newHostFlow(h, rts[h.Plugin], bytes).run()
				if err != nil {
					t.Errorf("%s %s/%s: %v", col.name, h.Plugin, h.Case.Name, err)
					got = err.Error()
				}
				col.results = append(col.results, flowComparable(got))
			}
			cols = append(cols, col)
		}
		closeAll(rts)
	}
	ref := cols[len(cols)-2] // sobek/in=maps: today's host
	for _, col := range cols {
		for ci, h := range cases {
			if _, ok := AcceptedDifferences["sobek/"+h.Plugin+"/"+h.Case.Name]; ok {
				continue // the reference itself is not deterministic
			}
			if want, got := ref.results[ci], col.results[ci]; !reflect.DeepEqual(want, got) {
				t.Errorf("%s %s/%s (%s) differs from %s:\n  want %s\n  got  %s", col.name, h.Plugin, h.Case.Name, hostFlowKind(h.Case), ref.name, flowJSON(want), flowJSON(got))
			}
		}
	}
}

func flowInput(bytes bool) string {
	if bytes {
		return "bytes"
	}
	return "maps"
}

func flowJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%#v", v)
	}
	return string(b)
}

// BenchmarkHostFlow measures the host flow per engine, input form and
// result kind (all: every case once per op).
func BenchmarkHostFlow(b *testing.B) {
	cases := hostFlowCases(b)
	for _, mk := range hostFlowEngines {
		e := mk()
		rts := pluginRuntimes(b, mustRunner(b, e))
		var released [][]*hostFlow
		for _, bytes := range []bool{false, true} {
			in := flowInput(bytes)
			var all []*hostFlow
			byKind := map[flowKind][]*hostFlow{}
			for _, h := range cases {
				f := newHostFlow(h, rts[h.Plugin], bytes)
				all = append(all, f)
				byKind[f.kind] = append(byKind[f.kind], f)
			}
			run := func(name string, fs []*hostFlow) {
				b.Run(e.Name()+"/in="+in+"/"+name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						for _, f := range fs {
							if _, err := f.run(); err != nil {
								b.Fatalf("%s/%s: %v", f.h.Plugin, f.h.Case.Name, err)
							}
						}
					}
					b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(fs)), "ns/call")
				})
			}
			run("all", all)
			for _, k := range flowKinds {
				run(string(k), byKind[k])
			}
			if all[0].native != nil {
				released = append(released, all)
			}
		}
		// A host that pools runtimes releases each after its request. These
		// rows run last, so the others find the runtimes as they were
		// without them.
		for _, all := range released {
			b.Run(e.Name()+"/in="+flowInput(all[0].bytes)+"/all+release", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					for _, f := range all {
						if _, err := f.run(); err != nil {
							b.Fatalf("%s/%s: %v", f.h.Plugin, f.h.Case.Name, err)
						}
						f.native.ReleaseCallData()
					}
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(all)), "ns/call")
			})
		}
		closeAll(rts)
	}
}
