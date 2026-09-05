package trae

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/muskke/trae-api-proxy/internal/config"
	"github.com/muskke/trae-api-proxy/internal/service/websearch"
)

func TestConvertResponsesRequestToChat(t *testing.T) {
	src := []byte(`{
		"model":"gpt-5.4",
		"instructions":"You are a coding agent.",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"inspect repo"}]}],
		"tools":[{"type":"function","name":"shell_command","description":"Run a command","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}],
		"tool_choice":"auto",
		"parallel_tool_calls":false,
		"stream":true,
		"store":false,
		"reasoning":{"effort":"high","summary":"auto"}
	}`)

	ctx, err := ConvertResponsesRequest(src)
	if err != nil {
		t.Fatal(err)
	}
	if ctx.RequestedModel != "gpt-5.4" || !ctx.Stream || ctx.ParallelToolCalls {
		t.Fatalf("unexpected context: %#v", ctx)
	}
	if ctx.ReasoningEffort != "high" || ctx.ReasoningSummary != "auto" {
		t.Fatalf("reasoning = %q/%q", ctx.ReasoningEffort, ctx.ReasoningSummary)
	}

	var chat map[string]any
	if err := json.Unmarshal(ctx.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	messages := chat["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %#v", messages)
	}
	if messages[0].(map[string]any)["role"] != "system" || messages[1].(map[string]any)["content"] != "inspect repo" {
		t.Fatalf("messages = %#v", messages)
	}
	tools := chat["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "shell_command" {
		t.Fatalf("function tool = %#v", fn)
	}
}

func TestConvertResponsesFunctionCallLoop(t *testing.T) {
	src := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"list files"}]},
			{"type":"function_call","call_id":"call_1","name":"shell_command","arguments":"{\"command\":\"ls\"}"},
			{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_text","text":"a.go\nb.go"}]}
		],
		"tools":[{"type":"function","name":"shell_command","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}]
	}`)
	ctx, err := ConvertResponsesRequest(src)
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	if err := json.Unmarshal(ctx.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	messages := chat["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %#v", messages)
	}
	assistant := messages[1].(map[string]any)
	calls := assistant["tool_calls"].([]any)
	call := calls[0].(map[string]any)
	if call["id"] != "call_1" || call["function"].(map[string]any)["name"] != "shell_command" {
		t.Fatalf("call = %#v", call)
	}
	tool := messages[2].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" || tool["content"] != "a.go\nb.go" {
		t.Fatalf("tool output = %#v", tool)
	}
}

func TestConvertResponsesCustomAndNamespaceTools(t *testing.T) {
	src := []byte(`{
		"model":"gpt-5.4",
		"input":"edit the file",
		"tools":[
			{"type":"custom","name":"apply_patch","description":"Apply a patch"},
			{"type":"namespace","name":"mcp","tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}]}
		]
	}`)
	ctx, err := ConvertResponsesRequest(src)
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	if err := json.Unmarshal(ctx.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	tools := chat["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %#v", tools)
	}
	first := tools[0].(map[string]any)["function"].(map[string]any)
	if first["name"] != "apply_patch" {
		t.Fatalf("custom bridge name = %#v", first["name"])
	}
	params := first["parameters"].(map[string]any)
	if _, ok := params["properties"].(map[string]any)["input"]; !ok {
		t.Fatalf("custom bridge parameters = %#v", params)
	}
	second := tools[1].(map[string]any)["function"].(map[string]any)
	if second["name"] != "mcp__read_file" {
		t.Fatalf("namespace bridge name = %#v", second["name"])
	}
}

func TestConvertResponsesRejectsHostedMCPTool(t *testing.T) {
	_, err := ConvertResponsesRequest([]byte(`{"model":"m","input":"x","tools":[{"type":"mcp","server_url":"https://example.com/mcp"}]}`))
	if err == nil || !strings.Contains(err.Error(), "hosted tool runtime") {
		t.Fatalf("expected hosted-tool error, got %v", err)
	}
}

func TestAggregateToResponsesTextReasoningAndFunction(t *testing.T) {
	request, err := ConvertResponsesRequest([]byte(`{
		"model":"gpt-5.4","input":"weather?",
		"tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	input := `event: metadata
+data: {"prompt_completion_id":"abc"}
+
+event: output
+data: {"reasoning_content":"think ","response":"checking ","tool_calls":[{"index":0,"id":"call_1","function_call":{"name":"weather","arguments":"{\"city\":\"SG\"}"}}]}
+
+event: token_usage
+data: {"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}
+
+event: done
+data: {"finish_reason":"tool_calls"}
+
+`
	input = strings.ReplaceAll(input, "\n+", "\n")
	result, err := AggregateToResponses(strings.NewReader(input), "gpt-5.4", request, TransformOptions{ReasoningMode: "preserve"})
	if err != nil {
		t.Fatal(err)
	}
	if result["id"] != "resp_abc" || result["object"] != "response" || result["status"] != "completed" {
		t.Fatalf("response = %#v", result)
	}
	output := result["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("output = %#v", output)
	}
	if output[0].(map[string]any)["type"] != "reasoning" || output[1].(map[string]any)["type"] != "message" {
		t.Fatalf("output = %#v", output)
	}
	call := output[2].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_1" || call["name"] != "weather" {
		t.Fatalf("call = %#v", call)
	}
	usage := result["usage"].(map[string]any)
	if usage["input_tokens"] != float64(3) || usage["output_tokens"] != float64(4) || usage["total_tokens"] != float64(7) {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestAggregateToResponsesRestoresCustomToolCall(t *testing.T) {
	request, err := ConvertResponsesRequest([]byte(`{"model":"m","input":"edit","tools":[{"type":"custom","name":"apply_patch"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	input := `event: output
+data: {"tool_calls":[{"index":0,"id":"call_patch","function_call":{"name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\\n*** End Patch\"}"}}]}
+
+event: done
+data: {"finish_reason":"tool_calls"}
+
+`
	input = strings.ReplaceAll(input, "\n+", "\n")
	result, err := AggregateToResponses(strings.NewReader(input), "m", request, TransformOptions{ReasoningMode: "drop"})
	if err != nil {
		t.Fatal(err)
	}
	output := result["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output = %#v", output)
	}
	call := output[0].(map[string]any)
	if call["type"] != "custom_tool_call" || call["name"] != "apply_patch" || !strings.Contains(call["input"].(string), "Begin Patch") {
		t.Fatalf("custom call = %#v", call)
	}
}

func TestStreamToResponsesEmitsCodexCriticalEvents(t *testing.T) {
	request, err := ConvertResponsesRequest([]byte(`{"model":"gpt-5.4","input":"hello","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	input := `event: output
+data: {"response":"Hel","reasoning_content":"think"}
+
+event: output
+data: {"response":"lo"}
+
+event: token_usage
+data: {"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}
+
+event: done
+data: {}
+
+`
	input = strings.ReplaceAll(input, "\n+", "\n")
	recorder := httptest.NewRecorder()
	if err := StreamToResponses(recorder, strings.NewReader(input), "gpt-5.4", request, TransformOptions{ReasoningMode: "preserve"}); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	for _, event := range []string{
		"event: response.created",
		"event: response.reasoning_text.delta",
		"event: response.output_item.added",
		"event: response.output_text.delta",
		"event: response.output_item.done",
		"event: response.completed",
	} {
		if !strings.Contains(body, event) {
			t.Fatalf("missing %q in stream:\n%s", event, body)
		}
	}
	if strings.Contains(body, "data: [DONE]") {
		t.Fatalf("Responses streams must end with response.completed, not [DONE]: %s", body)
	}
	if !strings.Contains(body, `"input_tokens":1`) || !strings.Contains(body, `"output_tokens":2`) {
		t.Fatalf("usage missing from response.completed: %s", body)
	}
}

func TestStreamToResponsesFunctionCallEvents(t *testing.T) {
	request, err := ConvertResponsesRequest([]byte(`{"model":"m","input":"x","stream":true,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	input := `event: output
+data: {"tool_calls":[{"index":0,"id":"call_1","function_call":{"name":"lookup","arguments":"{\"q\":"}}]}
+
+event: output
+data: {"tool_calls":[{"index":0,"function_call":{"arguments":"\"x\"}"}}]}
+
+event: done
+data: {"finish_reason":"tool_calls"}
+
+`
	input = strings.ReplaceAll(input, "\n+", "\n")
	recorder := httptest.NewRecorder()
	if err := StreamToResponses(recorder, strings.NewReader(input), "m", request, TransformOptions{ReasoningMode: "drop"}); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "event: response.function_call_arguments.delta") ||
		!strings.Contains(body, "event: response.function_call_arguments.done") ||
		!strings.Contains(body, `"type":"function_call"`) ||
		!strings.Contains(body, `"arguments":"{\"q\":\"x\"}"`) {
		t.Fatalf("unexpected function-call stream:\n%s", body)
	}
}

func TestConvertResponsesToolSearchAndLoadedNamespace(t *testing.T) {
	src := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"find a calendar tool"}]},
			{"type":"tool_search_call","call_id":"search_1","execution":"client","arguments":{"query":"calendar","limit":4}},
			{"type":"tool_search_output","call_id":"search_1","status":"completed","execution":"client","tools":[
				{"type":"namespace","name":"calendar","tools":[{"type":"function","name":"create_event","parameters":{"type":"object","properties":{"title":{"type":"string"}}}}]}
			]}
		],
		"tools":[{"type":"tool_search","execution":"client","description":"Search deferred tools","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}]
	}`)

	ctx, err := ConvertResponsesRequest(src)
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	if err := json.Unmarshal(ctx.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	tools := chat["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("translated tools = %#v", tools)
	}
	searchName := tools[0].(map[string]any)["function"].(map[string]any)["name"]
	loadedName := tools[1].(map[string]any)["function"].(map[string]any)["name"]
	if searchName != "tool_search" || loadedName != "calendar__create_event" {
		t.Fatalf("tool names = %q / %q", searchName, loadedName)
	}
	messages := chat["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %#v", messages)
	}
	call := messages[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if call["function"].(map[string]any)["name"] != "tool_search" {
		t.Fatalf("tool search call = %#v", call)
	}
	output := messages[2].(map[string]any)
	if output["tool_call_id"] != "search_1" || !strings.Contains(output["content"].(string), "create_event") {
		t.Fatalf("tool search output = %#v", output)
	}
}

func TestAggregateToResponsesRestoresToolSearchCall(t *testing.T) {
	request, err := ConvertResponsesRequest([]byte(`{"model":"m","input":"find tools","tools":[{"type":"tool_search","execution":"client","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	input := `event: output
+data: {"tool_calls":[{"index":0,"id":"search_1","function_call":{"name":"tool_search","arguments":"{\"query\":\"calendar\"}"}}]}
+
+event: done
+data: {"finish_reason":"tool_calls"}
+
+`
	input = strings.ReplaceAll(input, "\n+", "\n")
	result, err := AggregateToResponses(strings.NewReader(input), "m", request, TransformOptions{ReasoningMode: "drop"})
	if err != nil {
		t.Fatal(err)
	}
	output := result["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output = %#v", output)
	}
	call := output[0].(map[string]any)
	if call["type"] != "tool_search_call" || call["call_id"] != "search_1" || call["execution"] != "client" {
		t.Fatalf("tool search call = %#v", call)
	}
	args, ok := call["arguments"].(map[string]any)
	if !ok || args["query"] != "calendar" {
		t.Fatalf("tool search arguments = %#v", call["arguments"])
	}
}

func TestStreamToResponsesIncludesInProgressAndToolSearchDone(t *testing.T) {
	request, err := ConvertResponsesRequest([]byte(`{"model":"m","input":"find tools","stream":true,"tools":[{"type":"tool_search","execution":"client","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	input := `event: output
+data: {"tool_calls":[{"index":0,"id":"search_1","function_call":{"name":"tool_search","arguments":"{\"query\":\"calendar\"}"}}]}
+
+event: done
+data: {"finish_reason":"tool_calls"}
+
+`
	input = strings.ReplaceAll(input, "\n+", "\n")
	recorder := httptest.NewRecorder()
	if err := StreamToResponses(recorder, strings.NewReader(input), "m", request, TransformOptions{ReasoningMode: "drop"}); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	for _, needle := range []string{
		"event: response.in_progress",
		`"type":"tool_search_call"`,
		`"execution":"client"`,
		`"query":"calendar"`,
		"event: response.completed",
	} {
		if !strings.Contains(body, needle) {
			t.Fatalf("missing %q in stream:\n%s", needle, body)
		}
	}
}

func TestConvertResponsesAcceptsCurrentCodexEnvelopeFields(t *testing.T) {
	src := []byte(`{
		"model":"gpt-5.4",
		"instructions":"You are a coding agent.",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"reply ok"}]}],
		"tools":[{"type":"custom","name":"apply_patch"}],
		"tool_choice":"auto",
		"parallel_tool_calls":true,
		"reasoning":{"effort":"high","summary":"auto"},
		"store":false,
		"stream":true,
		"include":["reasoning.encrypted_content"],
		"service_tier":"auto",
		"prompt_cache_key":"codex-test",
		"client_metadata":{"client_name":"codex"},
		"text":{"format":{"type":"text"},"verbosity":"medium"}
	}`)
	ctx, err := ConvertResponsesRequest(src)
	if err != nil {
		t.Fatal(err)
	}
	if !ctx.Stream || ctx.RequestedModel != "gpt-5.4" || ctx.ReasoningEffort != "high" {
		t.Fatalf("unexpected context: %#v", ctx)
	}
}

func TestResponsesObjectUsesEmptyToolArray(t *testing.T) {
	request, err := ConvertResponsesRequest([]byte(`{"model":"m","input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	input := "event: output\ndata: {\"response\":\"ok\"}\n\nevent: done\ndata: {}\n\n"
	result, err := AggregateToResponses(strings.NewReader(input), "m", request, TransformOptions{ReasoningMode: "drop"})
	if err != nil {
		t.Fatal(err)
	}
	tools, ok := result["tools"].([]any)
	if !ok || tools == nil || len(tools) != 0 {
		t.Fatalf("tools must be a non-nil empty array, got %#v", result["tools"])
	}
}

func TestConvertResponsesWebSearchTool(t *testing.T) {
	ctx, err := ConvertResponsesRequest([]byte(`{
		"model":"gpt-5.4",
		"input":"what happened today?",
		"tools":[{
			"type":"web_search",
			"search_context_size":"high",
			"filters":{"allowed_domains":["example.com"]},
			"user_location":{"type":"approximate","country":"SG"}
		}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if ctx.HostedWebSearch == nil {
		t.Fatal("expected hosted web search options")
	}
	if ctx.HostedWebSearch.SearchContextSize != "high" || len(ctx.HostedWebSearch.AllowedDomains) != 1 || ctx.HostedWebSearch.AllowedDomains[0] != "example.com" {
		t.Fatalf("web search options = %#v", ctx.HostedWebSearch)
	}
	var chat map[string]any
	if err := json.Unmarshal(ctx.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	tools := chat["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", tools)
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "web_search" {
		t.Fatalf("web search bridge name = %#v", fn["name"])
	}
}

func TestConvertResponsesAcceptsWebSearchCallHistory(t *testing.T) {
	ctx, err := ConvertResponsesRequest([]byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"latest news"}]},
			{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"latest news"}},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Here is the result."}]}
		],
		"tools":[{"type":"web_search"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	if err := json.Unmarshal(ctx.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	messages := chat["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("web_search_call history should be ignored as provider artifact; messages = %#v", messages)
	}
}

type fakeHostedWebSearch struct {
	calls []websearch.Action
}

func (f *fakeHostedWebSearch) Backend() string { return "fake" }

func (f *fakeHostedWebSearch) Execute(_ context.Context, action websearch.Action, _ websearch.ToolOptions) (websearch.Result, error) {
	f.calls = append(f.calls, action)
	return websearch.Result{
		Action:  action,
		Sources: []websearch.Source{{Title: "Example", URL: "https://example.com/latest", Snippet: "Current result"}},
		Content: "Current result from the web",
	}, nil
}

func TestExecuteResponsesWithHostedWebSearch(t *testing.T) {
	var upstreamBodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBodies = append(upstreamBodies, append([]byte(nil), body...))
		w.Header().Set("Content-Type", "text/event-stream")
		if len(upstreamBodies) == 1 {
			_, _ = io.WriteString(w, "event: output\ndata: {\"tool_calls\":[{\"index\":0,\"id\":\"call_web_1\",\"function_call\":{\"name\":\"web_search\",\"arguments\":\"{\\\"action\\\":\\\"search\\\",\\\"query\\\":\\\"OpenAI latest\\\"}\"}}]}\n\nevent: done\ndata: {\"finish_reason\":\"tool_calls\"}\n\n")
			return
		}
		_, _ = io.WriteString(w, "event: output\ndata: {\"response\":\"The web result says hello.\"}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
	}))
	defer server.Close()

	cfg := &config.Config{APIBaseURL: server.URL, UpstreamMode: "solo"}
	client := NewClient(cfg)
	request, err := ConvertResponsesRequest([]byte(`{"model":"gpt-5.4","input":"what is new?","tools":[{"type":"web_search"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	runtime := &fakeHostedWebSearch{}
	execution, err := client.ExecuteResponsesWithHostedTools(context.Background(), Session{APIBaseURL: server.URL, Platform: "global"}, "gpt-5.4", request, runtime, TransformOptions{ReasoningMode: "preserve"}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(runtime.calls) != 1 || runtime.calls[0].Type != "search" || runtime.calls[0].Query != "OpenAI latest" {
		t.Fatalf("runtime calls = %#v", runtime.calls)
	}
	if len(upstreamBodies) != 2 || !strings.Contains(string(upstreamBodies[1]), `"role":"tool"`) || !strings.Contains(string(upstreamBodies[1]), "Current result from the web") {
		t.Fatalf("second upstream body = %s", upstreamBodies[len(upstreamBodies)-1])
	}
	output := execution.Response["output"].([]any)
	if len(output) < 2 {
		t.Fatalf("response output = %#v", output)
	}
	searchItem := output[0].(map[string]any)
	if searchItem["type"] != "web_search_call" || searchItem["status"] != "completed" {
		t.Fatalf("web search output item = %#v", searchItem)
	}
	message := output[len(output)-1].(map[string]any)
	if message["type"] != "message" {
		t.Fatalf("final item = %#v", message)
	}

	recorder := httptest.NewRecorder()
	if err := StreamHostedResponses(recorder, execution, request); err != nil {
		t.Fatal(err)
	}
	stream := recorder.Body.String()
	for _, needle := range []string{
		"event: response.web_search_call.in_progress",
		"event: response.web_search_call.searching",
		"event: response.web_search_call.completed",
		`"type":"web_search_call"`,
		"event: response.output_text.delta",
		"event: response.completed",
	} {
		if !strings.Contains(stream, needle) {
			t.Fatalf("missing %q in hosted Responses stream:\n%s", needle, stream)
		}
	}
}
