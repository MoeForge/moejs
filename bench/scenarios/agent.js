// agent forwards an OpenAI Chat Completions agent request to an upstream that
// serves the model under its own name, wants the gateway's preamble at the top
// of the system prompt, and rejects some parameters and JSON Schema keywords.
// It follows the shape of a task plugin's buildSubmitRequest: the host passes
// the parsed client request in ctx.requestBody and sends whatever comes back.

const DROP_PARAMS = ["store", "service_tier", "prompt_cache_retention", "web_search_options", "safety_identifier"];
const DROP_SCHEMA_KEYS = ["$schema", "$id", "examples"];

// text flattens a message's content to one string: an array of text parts
// becomes the parts joined by newlines.
function text(content) {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  const parts = [];
  for (const p of content) {
    if (p && p.type === "text" && typeof p.text === "string") parts.push(p.text);
  }
  return parts.join("\n");
}

// cleanSchema copies a tool's JSON Schema without the keywords the upstream
// rejects.
function cleanSchema(s) {
  if (Array.isArray(s)) return s.map(cleanSchema);
  if (s === null || typeof s !== "object") return s;
  const out = {};
  for (const k of Object.keys(s)) {
    if (DROP_SCHEMA_KEYS.indexOf(k) >= 0) continue;
    if (k === "additionalProperties" && s[k] === false) continue;
    out[k] = cleanSchema(s[k]);
  }
  return out;
}

function message(m, preamble) {
  const role = m.role === "developer" ? "system" : m.role;
  let content = text(m.content);
  if (preamble) content = preamble + "\n\n" + content;
  if (role === "assistant" && Array.isArray(m.tool_calls) && m.tool_calls.length > 0) {
    return {
      role: role,
      content: content.length > 0 ? content : null,
      tool_calls: m.tool_calls.map(function (c) {
        const args = c.function.arguments;
        return {
          id: c.id,
          type: "function",
          function: { name: c.function.name, arguments: typeof args === "string" ? args : JSON.stringify(args) },
        };
      }),
    };
  }
  if (role === "tool") return { role: role, tool_call_id: m.tool_call_id, content: content };
  return { role: role, content: content };
}

export function buildUpstreamRequest(ctx) {
  const req = ctx.requestBody || {};
  const channel = ctx.channel || {};
  const msgs = Array.isArray(req.messages) ? req.messages : [];

  let chars = 0;
  for (const m of msgs) chars += text(m.content).length;
  if (channel.maxPromptChars && chars > channel.maxPromptChars) {
    throw new Error("prompt has " + chars + " characters, the channel allows " + channel.maxPromptChars);
  }

  const body = {};
  for (const k of Object.keys(req)) {
    if (k === "messages" || k === "tools" || DROP_PARAMS.indexOf(k) >= 0) continue;
    body[k] = req[k];
  }
  body.model = ctx.upstreamModel || req.model;
  if (body.max_tokens !== undefined) {
    body.max_completion_tokens = body.max_tokens;
    delete body.max_tokens;
  }
  if (body.stream === true) body.stream_options = { include_usage: true };
  if (ctx.userId) body.user = "u" + ctx.userId;

  const preamble = channel.systemPreamble || "";
  let injected = preamble === "";
  body.messages = msgs.map(function (m) {
    if (!injected && (m.role === "system" || m.role === "developer")) {
      injected = true;
      return message(m, preamble);
    }
    return message(m, "");
  });
  if (!injected) body.messages.unshift({ role: "system", content: preamble });

  if (Array.isArray(req.tools)) {
    body.tools = req.tools.map(function (t) {
      const f = t.function || {};
      return {
        type: "function",
        function: {
          name: f.name,
          description: f.description || "",
          parameters: cleanSchema(f.parameters || { type: "object", properties: {} }),
        },
      };
    });
  }

  const headers = {
    "Content-Type": "application/json",
    Authorization: "Bearer " + ctx.apiKey,
    "X-Prompt-Chars": String(chars),
  };
  if (ctx.requestId) headers["X-Request-Id"] = ctx.requestId;
  return { url: ctx.baseUrl + "/v1/chat/completions", method: "POST", headers: headers, body: body };
}
