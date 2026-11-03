package main

import (
	"github.com/MoeForge/moejs/bench/engines"
)

// scenario is one hook call to record; Args are JSON-shaped Go values.
type scenario struct {
	Name   string
	Hook   string
	Member string
	Path   []string
	Args   []any
}

type obj = map[string]any
type arr = []any

// scenarios holds every plugin's recorded calls, keyed by plugin key.
var scenarios = map[string][]scenario{
	"alibaba":   alibabaScenarios(),
	"doubao":    doubaoScenarios(),
	"google":    googleScenarios(),
	"hailuo":    hailuoScenarios(),
	"jimeng":    jimengScenarios(),
	"kling":     klingScenarios(),
	"sora":      soraScenarios(),
	"sunoapi":   sunoapiScenarios(),
	"vertex-ai": vertexScenarios(),
	"vidu":      viduScenarios(),
}

const publicTaskID = "task_pub_0001"

// protocolPath maps a protocol name to the host route it is served on.
var protocolPath = map[string]string{
	"openai_responses": "/v1/responses",
	"openai_video":     "/v1/videos",
	"openai_image":     "/v1/images/generations",
}

// jsonBody is the RouteRequestContext body for a JSON request.
func jsonBody(value any) obj { return obj{"kind": "json", "value": value} }

// multipartBody is the RouteRequestContext body for a multipart request.
func multipartBody(fields obj, files arr) obj {
	return obj{"kind": "multipart", "fields": fields, "files": files}
}

// protocolCtx mirrors ProtocolRequestContext.JSValue().
func protocolCtx(protocol, operation, model, upstreamModel string, body obj) obj {
	ctx := obj{
		"path":      protocolPath[protocol],
		"method":    "POST",
		"params":    obj{},
		"query":     obj{},
		"body":      body,
		"protocol":  protocol,
		"operation": operation,
		"model":     model,
		"stream":    false,
	}
	if upstreamModel != "" {
		ctx["upstreamModel"] = upstreamModel
	}
	return ctx
}

// routeCtx mirrors RouteRequestContext.JSValue() for a native route.
func routeCtx(method, path string, params, query obj, body obj) obj {
	if params == nil {
		params = obj{}
	}
	if query == nil {
		query = obj{}
	}
	return obj{"path": path, "method": method, "params": params, "query": query, "body": body}
}

type creds struct {
	baseURL string
	apiKey  string
	// oauth is set for auth.type oauth2_jwt plugins: authHeader and projectId
	// replace apiKey in the context (see applyUpstreamCredentials).
	oauth     obj
	viaNewAPI bool
}

func (c creds) apply(ctx obj) obj {
	switch {
	case c.viaNewAPI:
		ctx["upstream"] = obj{"kind": "new_api"}
		ctx["auth"] = obj{"authHeader": "Bearer " + c.apiKey}
		ctx["authHeader"] = "Bearer " + c.apiKey
		ctx["apiKey"] = c.apiKey
	case c.oauth != nil:
		ctx["upstream"] = obj{"kind": "vendor"}
		ctx["auth"] = c.oauth
		ctx["authHeader"] = c.oauth["authHeader"]
	default:
		ctx["upstream"] = obj{"kind": "vendor"}
		ctx["auth"] = obj{"authHeader": c.apiKey}
		ctx["authHeader"] = c.apiKey
		ctx["apiKey"] = c.apiKey
	}
	return ctx
}

type submitOpts struct {
	creds         creds
	protocol      string // route the client used; defaults to openai_responses
	clientBody    any    // the client JSON body (ctx.body.value)
	requestBody   any    // the decoded requestBody (task_request)
	action        string
	model         string
	upstreamModel string
	userSetting   any
	usagePurpose  string
	originTaskID  string
	originTasks   arr
	files         arr
	authError     string
}

// submitCtx mirrors TaskAdaptor.submitContext.
func submitCtx(o submitOpts) obj {
	protocol := o.protocol
	if protocol == "" {
		protocol = "openai_responses"
	}
	upstream := o.upstreamModel
	if upstream == "" {
		upstream = o.model
	}
	files := o.files
	if files == nil {
		files = arr{}
	}
	ctx := obj{
		"path":           protocolPath[protocol],
		"method":         "POST",
		"params":         obj{},
		"query":          obj{},
		"body":           jsonBody(o.clientBody),
		"requestBody":    o.requestBody,
		"requestHeaders": obj{"Content-Type": "application/json", "Accept": "application/json"},
		"files":          files,
		"action":         o.action,
		"originTaskId":   o.originTaskID,
		"publicTaskId":   publicTaskID,
		"model":          o.model,
		"upstreamModel":  upstream,
		"baseUrl":        o.creds.baseURL,
		"userSetting":    o.userSetting,
	}
	if o.originTasks != nil {
		ctx["originTasks"] = o.originTasks
	}
	if o.usagePurpose != "" {
		ctx["usagePurpose"] = o.usagePurpose
	}
	o.creds.apply(ctx)
	if o.authError != "" {
		ctx["authError"] = o.authError
	}
	return ctx
}

type queryOpts struct {
	creds         creds
	taskID        string
	action        string
	model         string
	upstreamModel string
	data          any
	state         any
}

// queryCtx mirrors TaskAdaptor.queryContext.
func queryCtx(o queryOpts) obj {
	upstream := o.upstreamModel
	if upstream == "" {
		upstream = o.model
	}
	ctx := obj{
		"taskId":        o.taskID,
		"publicTaskId":  publicTaskID,
		"action":        o.action,
		"model":         o.model,
		"upstreamModel": upstream,
		"baseUrl":       o.creds.baseURL,
		"data":          o.data,
		"state":         o.state,
	}
	return o.creds.apply(ctx)
}

// batchCtx mirrors TaskAdaptor.batchQueryContext.
func batchCtx(c creds, tasks arr) obj {
	return c.apply(obj{"baseUrl": c.baseURL, "tasks": tasks})
}

// artifactCtx mirrors taskArtifactContext (+ the buildContentRequest extras).
func artifactCtx(c creds, taskID, upstreamTaskID, status, action string, data, state any, artifactKey string) obj {
	ctx := obj{
		"taskId":          taskID,
		"status":          status,
		"action":          action,
		"data":            data,
		"state":           state,
		"producerVersion": "1.0.0",
	}
	if artifactKey != "" {
		ctx["upstreamTaskId"] = upstreamTaskID
		ctx["artifactKey"] = artifactKey
		ctx["baseUrl"] = c.baseURL
		ctx["clientRequest"] = obj{"method": "GET", "headers": obj{}, "rangeHeader": ""}
		c.apply(ctx)
	}
	return ctx
}

// submitResp is the second parseSubmitResponse argument.
func submitResp(status int, body any) obj {
	return obj{
		"statusCode": status,
		"headers":    obj{"Content-Type": arr{"application/json"}, "X-Request-Id": arr{"req_0123456789abcdef"}},
		"body":       body,
	}
}

// pollResp is the third parseTaskResult argument (hookHTTPResponse).
func pollResp(status int) obj {
	return obj{"status": status, "headers": obj{"Content-Type": "application/json"}}
}

// taskInfo is jsonValue(*relaycommon.TaskInfo) handed to extractUsageOnComplete.
func taskInfo(taskID, status, progress, url string) obj {
	info := obj{"code": 0, "task_id": taskID, "status": status}
	if progress != "" {
		info["progress"] = progress
	}
	if url != "" {
		info["url"] = url
	}
	return info
}

// taskView mirrors dto.TaskView (service.BuildTaskPluginView).
func taskView(platform, status, progress, failReason string, data any) obj {
	view := obj{
		"task_id":     publicTaskID,
		"platform":    platform,
		"status":      status,
		"progress":    progress,
		"fail_reason": failReason,
		"created_at":  1700000000,
	}
	if status == "SUCCESS" || status == "FAILURE" {
		view["updated_at"] = 1700000123
		view["finished_at"] = 1700000123
	} else {
		view["updated_at"] = 1700000050
	}
	if data != nil {
		view["data"] = data
	}
	return view
}

// renderCtx mirrors taskPluginProtocolRendererContext: the protocol request
// plus artifact content URLs when the task succeeded.
func renderCtx(model, upstreamModel string, clientBody obj, artifacts obj) obj {
	ctx := protocolCtx("openai_responses", "create", model, upstreamModel, jsonBody(clientBody))
	if artifacts != nil {
		ctx["artifacts"] = artifacts
	}
	return ctx
}

func artifactURL(key string) string {
	return "https://gateway.example.com/v1/tasks/" + publicTaskID + "/artifacts/" + key + "?sig=YWJj&exp=1700003600"
}

func videoArtifacts() obj {
	return obj{"video": obj{"key": "video", "type": "video", "url": artifactURL("video")}}
}

func imageArtifacts(n int) obj {
	out := obj{}
	for i := 1; i <= n; i++ {
		key := "image-" + string(rune('0'+i))
		out[key] = obj{"key": key, "type": "image", "url": artifactURL(key)}
	}
	return out
}

// responsesInput builds an OpenAI Responses `input` array with text parts
// and optional image parts, the shape the Responses protocol decoders walk.
func responsesInput(texts []string, images ...string) arr {
	content := arr{}
	for _, t := range texts {
		content = append(content, obj{"type": "input_text", "text": t})
	}
	for _, img := range images {
		content = append(content, obj{"type": "input_image", "image_url": obj{"url": img}})
	}
	return arr{
		obj{"role": "system", "content": "You are a helpful video generation assistant. 请根据用户描述生成内容。"},
		obj{"role": "user", "content": content},
	}
}

var pngDataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

var host = engines.DefaultHost
