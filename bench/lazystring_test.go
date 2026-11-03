package bench

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Calcium-Ion/moejs"
)

// lazyStringPlugin is a gateway hook over chat messages: forward rebuilds
// the request around the message contents without reading them, inspect
// reads every content's length and searches it.
const lazyStringPlugin = `
export function forward(req) {
  return { model: "upstream-" + req.model, stream: true, messages: req.messages.map(m => ({ role: m.role, content: m.content })) };
}
export function inspect(req) {
  let n = 0;
  for (const m of req.messages) n += m.content.length + (m.content.includes("错误") ? 1 : 0);
  return n;
}
`

// lazyStringText is size bytes, or a little more, of text in the style of
// a chat message: Chinese with punctuation, digits and an emoji, or ASCII.
func lazyStringText(size int, cjk bool) string {
	unit := "The model replied with 42 lines of output. "
	if cjk {
		unit = "模型返回了 42 行中文输出，请检查格式😀。"
	}
	return strings.Repeat(unit, size/len(unit)+1)
}

// BenchmarkLazyString times a request of 8 messages through the hooks of
// lazyStringPlugin, converted from Go maps or parsed from JSON, with the
// result written as JSON (forward) or read as a Go value (inspect).
func BenchmarkLazyString(b *testing.B) {
	mod, err := moejs.Compile("lazy.js", lazyStringPlugin)
	if err != nil {
		b.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		size int
		cjk  bool
	}{
		{"cjk-1k", 1 << 10, true},
		{"cjk-64k", 64 << 10, true},
		{"ascii-1k", 1 << 10, false},
		{"ascii-64k", 64 << 10, false},
	} {
		text := lazyStringText(tc.size, tc.cjk)
		messages := make([]any, 8)
		for i := range messages {
			// Each message has its own copy: FromGo converts one Go string
			// once (hostHeap.str), which would decode a shared text once
			// for all the messages.
			messages[i] = map[string]any{"role": []string{"user", "assistant"}[i%2], "content": strings.Clone(text)}
		}
		req := map[string]any{"model": "gpt", "messages": messages}
		body, err := json.Marshal(req)
		if err != nil {
			b.Fatal(err)
		}
		for _, hook := range []string{"forward", "inspect"} {
			for _, in := range []string{"maps", "json"} {
				b.Run(tc.name+"/"+in+"/"+hook, func(b *testing.B) {
					rt := moejs.NewRuntime(moejs.Options{})
					if err := rt.Load(mod); err != nil {
						b.Fatal(err)
					}
					h, err := mod.Hook(hook)
					if err != nil {
						b.Fatal(err)
					}
					var out []byte
					b.SetBytes(int64(len(body)))
					b.ReportAllocs()
					for b.Loop() {
						var arg moejs.Value
						if in == "maps" {
							arg, err = rt.FromGo(req)
						} else {
							arg, err = rt.ParseJSON(body)
						}
						if err != nil {
							b.Fatal(err)
						}
						res, err := rt.Call(h, arg)
						if err != nil {
							b.Fatal(err)
						}
						if hook == "forward" {
							out, err = rt.AppendJSON(out[:0], res)
						} else {
							_, err = rt.ToGo(res)
						}
						if err != nil {
							b.Fatal(err)
						}
						rt.ReleaseCallData()
					}
				})
			}
		}
	}
}
