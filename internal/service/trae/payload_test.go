package trae

import (
	"encoding/json"
	"testing"
)

func TestPrepareSoloBodyNormalizesOpenAIRequest(t *testing.T) {
	src := []byte(`{
		"model":"ignored",
		"messages":[
			{"role":"user","content":"hello"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}
		],
		"stream":false,
		"stream_options":{"include_usage":true},
		"max_completion_tokens":123,
		"tool_choice":{"type":"function","function":{"name":"lookup"}},
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]
	}`)

	out, err := prepareSoloBody(src, "glm-5.2")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["function"] != soloFunction || got["config_name"] != "glm-5.2" || got["model"] != "glm-5.2" {
		t.Fatalf("unexpected model/function mapping: %#v", got)
	}
	if stream, _ := got["stream"].(bool); !stream {
		t.Fatal("upstream stream must be forced true")
	}
	if _, ok := got["stream_options"]; ok {
		t.Fatal("stream_options must not be sent upstream")
	}
	if got["max_tokens"] != float64(123) {
		t.Fatalf("max_tokens = %#v", got["max_tokens"])
	}
	if got["tool_choice"] != "lookup" {
		t.Fatalf("tool_choice = %#v", got["tool_choice"])
	}

	messages := got["messages"].([]any)
	user := messages[0].(map[string]any)
	content := user["content"].([]any)
	if content[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("content normalization failed: %#v", content)
	}
	assistant := messages[1].(map[string]any)
	calls := assistant["tool_calls"].([]any)
	call := calls[0].(map[string]any)
	if _, ok := call["function"]; ok {
		t.Fatal("assistant function should be renamed to function_call")
	}
	if call["function_call"].(map[string]any)["name"] != "lookup" {
		t.Fatalf("function_call = %#v", call["function_call"])
	}

	tools := got["tools"].([]any)
	params := tools[0].(map[string]any)["function"].(map[string]any)["parameters"]
	if _, ok := params.(string); !ok {
		t.Fatalf("tool parameters should be JSON string, got %T", params)
	}
}

func TestPrepareSoloBodyNoneSuppressesTools(t *testing.T) {
	src := []byte(`{"messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"f","parameters":{}}}],"tool_choice":"none"}`)
	out, err := prepareSoloBody(src, "glm-5.2")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["tools"]; ok {
		t.Fatal("tools should be removed for tool_choice=none")
	}
	if _, ok := got["tool_choice"]; ok {
		t.Fatal("tool_choice should be removed for none")
	}
}
