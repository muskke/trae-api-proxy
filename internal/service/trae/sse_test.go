package trae

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

const sampleSSE = `event: metadata
data: {"prompt_completion_id":"abc123"}

event: output
data: {"response":"Hello ","reasoning_content":"think ","tool_calls":[{"index":0,"id":"call_1","type":"function","function_call":{"name":"lookup","arguments":"{\"q\":"}}]}

event: output
data: {"response":"world","reasoning_content":"done","tool_calls":[{"index":0,"function_call":{"arguments":"\"x\"}"}}]}

event: token_usage
data: {"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}

event: done
data: {"finish_reason":"tool_calls"}

`

func TestAggregateToOpenAI(t *testing.T) {
	result, err := AggregateToOpenAI(strings.NewReader(sampleSSE), "glm-5.2")
	if err != nil {
		t.Fatal(err)
	}
	if result["id"] != "chatcmpl-abc123" {
		t.Fatalf("id = %#v", result["id"])
	}
	choices := result["choices"].([]any)
	choice := choices[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %#v", choice["finish_reason"])
	}
	message := choice["message"].(map[string]any)
	if message["content"] != "Hello world" || message["reasoning_content"] != "think done" {
		t.Fatalf("message = %#v", message)
	}
	calls := message["tool_calls"].([]map[string]any)
	fn := calls[0]["function"].(map[string]any)
	if fn["name"] != "lookup" || fn["arguments"] != `{"q":"x"}` {
		t.Fatalf("merged tool call = %#v", calls[0])
	}
	usage := result["usage"].(map[string]any)
	if usage["total_tokens"] != float64(7) {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestStreamToOpenAIIncludesUsageAndDone(t *testing.T) {
	recorder := httptest.NewRecorder()
	if err := StreamToOpenAI(recorder, strings.NewReader(sampleSSE), "glm-5.2", true); err != nil {
		t.Fatal(err)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("content-type = %q", got)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"role":"assistant"`) || !strings.Contains(body, `"content":"Hello "`) {
		t.Fatalf("stream missing content chunks: %s", body)
	}
	if !strings.Contains(body, `"usage":{"completion_tokens":4,"prompt_tokens":3,"total_tokens":7}`) {
		t.Fatalf("stream missing usage chunk: %s", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("stream missing DONE: %q", body)
	}

	// Every non-DONE SSE data line must contain valid JSON.
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var value any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &value); err != nil {
			t.Fatalf("invalid JSON SSE chunk %q: %v", line, err)
		}
	}
}

func TestForEachSSEEventToleratesMissingBlankLines(t *testing.T) {
	input := "event: metadata\ndata: {\"id\":\"1\"}\nevent: output\ndata: {\"response\":\"x\"}\n"
	var names []string
	if err := forEachSSEEvent(strings.NewReader(input), func(name, data string) error {
		names = append(names, name)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "metadata,output" {
		t.Fatalf("events = %v", names)
	}
}
