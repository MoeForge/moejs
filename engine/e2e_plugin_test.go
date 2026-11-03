package engine_test

import (
	"sync"
	"testing"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/engine"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A plugin-shaped module that crosses every package boundary at once:
// syntax -> compiler -> interpreter -> builtins, with host values in and out.
// It avoids String/RegExp/JSON, which get their own coverage.
const e2ePluginSource = `
const MODELS = { a: { max: 4, sizes: ["1024", "1536"] }, b: { max: 1, sizes: ["512"] } };
function collect(input) {
  const out = [];
  if (!Array.isArray(input)) return out;
  for (const item of input) {
    if (!item || typeof item !== "object" || Array.isArray(item)) continue;
    const content = item.content === undefined ? [item] : Array.isArray(item.content) ? item.content : [item.content];
    for (const part of content) {
      if (typeof part === "string") { out.push(part); continue; }
      if (!part || typeof part !== "object") continue;
      const { type, text } = part;
      if (["input_text", "text"].includes(type) && typeof text === "string") out.push(text);
    }
  }
  return out;
}
export function decode(ctx) {
  const req = ctx.body && ctx.body.value;
  if (!req || typeof req !== "object" || Array.isArray(req)) throw new Error("request body must be a JSON object");
  const spec = MODELS[req.model];
  if (!spec) throw new Error("unsupported model: " + req.model);
  const texts = collect(req.input);
  const n = Number.isInteger(req.n) && req.n > 0 ? req.n : 1;
  const body = Object.assign({}, { model: req.model, n: n, count: texts.length, max: Math.max(n, spec.max) });
  const headers = {};
  for (const k of Object.keys(ctx.headers || {})) headers[k] = ctx.headers[k];
  let total = 0;
  texts.forEach(function (t) { total += t.length; });
  return {
    kind: "submit", model: ctx.model, requestBody: body,
    texts: texts.map(function (t, i) { return i + ":" + t; }).filter(function (t) { return t.length > 2; }),
    hasAuth: Object.prototype.hasOwnProperty.call(headers, "Authorization"),
    upstream: ctx.upstream?.kind ?? "vendor",
    sizes: [...spec.sizes, "x"].join(","),
    total: total, tpl: ` + "`" + `${req.model}/${n}` + "`" + `,
  };
}
export function fail() {
  try { throw new TypeError("boom"); } catch (e) { return e instanceof TypeError && e.message === "boom" && typeof e.stack === "string" ? "caught" : "wrong"; }
}
export const meta = { apiVersion: 1, key: "smoke", models: Object.keys(MODELS) };
export function hot(raw) {
  const body = JSON.parse(raw);
  const prompt = String(body.prompt || "").replace(/\s+/g, " ").trim();
  const parts = prompt.split(" ").filter(function (p) { return p.length > 0; });
  const size = /^(\d+)\s*[xX×*]\s*(\d+)$/.exec(String(body.size || ""));
  return JSON.stringify({
    prompt: prompt, words: parts.length, upper: parts[0].toUpperCase(), tail: prompt.slice(-4),
    size: size ? [Number(size[1]), Number(size[2])] : null,
    ok: /^https?:\/\//i.test(String(body.url)), enc: encodeURIComponent("a b/é"),
    when: new Date(0).toISOString(), has: "日本語テキスト".includes("テキ"),
  });
}
`

func e2eHookArg(model string) map[string]any {
	return map[string]any{
		"model": model, "upstream": map[string]any{"kind": "vendor"},
		"headers": map[string]string{"Authorization": "Bearer sk-test", "Content-Type": "application/json"},
		"body": map[string]any{"kind": "json", "value": map[string]any{
			"model": model, "n": float64(2),
			"input": []any{
				map[string]any{"role": "system", "content": "You are helpful. 请根据描述生成图片。"},
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "input_text", "text": "A lighthouse at dusk"},
					map[string]any{"type": "input_image", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
					map[string]any{"type": "input_text", "text": "soft palette"},
				}},
			},
		}},
	}
}

var e2eExpectedDecode = map[string]any{
	"kind": "submit", "model": "a",
	"requestBody": map[string]any{"count": int64(3), "max": int64(4), "model": "a", "n": int64(2)},
	"texts":       []any{"0:You are helpful. 请根据描述生成图片。", "1:A lighthouse at dusk", "2:soft palette"},
	"hasAuth":     true, "upstream": "vendor", "sizes": "1024,1536,x", "total": int64(59), "tpl": "a/2",
}

func e2eCallDecode(t *testing.T, r *engine.Realm, env *engine.ModuleEnv, model string) (any, error) {
	decode, ok := env.GetBindingValue("decode")
	require.True(t, ok)
	arg, err := r.FromGo(e2eHookArg(model))
	require.NoError(t, err)
	out, err := r.Call(decode, engine.Undefined(), []engine.Value{arg})
	if err != nil {
		return nil, err
	}
	return r.ToGo(out), nil
}

func TestE2EPluginModuleAcrossSharedRealms(t *testing.T) {
	mod, err := syntax.ParseModule("smoke.js", e2ePluginSource, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(mod)
	require.NoError(t, err)

	r := engine.NewRealmWith(engine.RealmOptions{SharedIntrinsics: true})
	env, err := r.EvaluateModule(code)
	require.NoError(t, err)

	meta, ok := env.GetBindingValue("meta")
	require.True(t, ok)
	assert.Equal(t, map[string]any{"apiVersion": int64(1), "key": "smoke", "models": []any{"a", "b"}}, r.ToGo(meta))

	fail, ok := env.GetBindingValue("fail")
	require.True(t, ok)
	caught, err := r.Call(fail, engine.Undefined(), nil)
	require.NoError(t, err)
	assert.Equal(t, "caught", r.ToGo(caught))

	out, err := e2eCallDecode(t, r, env, "a")
	require.NoError(t, err)
	assert.Equal(t, e2eExpectedDecode, out)

	_, err = e2eCallDecode(t, r, env, "zzz")
	var exc *engine.Exception
	require.ErrorAs(t, err, &exc)
	assert.Contains(t, err.Error(), "unsupported model: zzz")
	assert.True(t, exc.Value.IsObject())
	msg, err := exc.Value.AsObject().GetProp(r, engine.StringKey(engine.AtomMessage))
	require.NoError(t, err)
	assert.Equal(t, "unsupported model: zzz", r.ToGo(msg))

	hot, ok := env.GetBindingValue("hot")
	require.True(t, ok)
	raw, err := r.FromGo(`{"prompt":"  A   watercolor\n lighthouse  ","size":"1536 x 1024","url":"HTTPS://example.com/x"}`)
	require.NoError(t, err)
	hotOut, err := r.Call(hot, engine.Undefined(), []engine.Value{raw})
	require.NoError(t, err)
	assert.JSONEq(t, `{"prompt":"A watercolor lighthouse","words":3,"upper":"A","tail":"ouse","size":[1536,1024],"ok":true,"enc":"a%20b%2F%C3%A9","when":"1970-01-01T00:00:00.000Z","has":true}`, r.ToGo(hotOut).(string))

	// The compiled module is immutable: many realms evaluate it concurrently.
	var wg sync.WaitGroup
	results := make([]any, 8)
	errs := make([]error, 8)
	for i := range results {
		wg.Go(func() {
			realm := engine.NewRealmWith(engine.RealmOptions{SharedIntrinsics: true})
			env, err := realm.EvaluateModule(code)
			if err != nil {
				errs[i] = err
				return
			}
			results[i], errs[i] = e2eCallDecode(t, realm, env, "a")
		})
	}
	wg.Wait()
	for i := range results {
		require.NoError(t, errs[i])
		assert.Equal(t, e2eExpectedDecode, results[i])
	}
}
