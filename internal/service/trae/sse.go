package trae

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type normalizedEvent struct {
	Name         string
	CompletionID string
	Content      string
	Reasoning    string
	ToolCalls    json.RawMessage
	Usage        map[string]any
	FinishReason string
	ErrorCode    string
	ErrorMessage string
}

type StreamError struct {
	Code    string
	Message string
}

type TransformOptions struct {
	ReasoningMode string
}

func (e *StreamError) Error() string {
	if e.Code == "" {
		return "upstream stream error: " + e.Message
	}
	return fmt.Sprintf("upstream stream error %s: %s", e.Code, e.Message)
}

func AggregateToOpenAI(r io.Reader, model string) (map[string]any, error) {
	return AggregateToOpenAIWithOptions(r, model, TransformOptions{ReasoningMode: "preserve"})
}

func AggregateToOpenAIWithOptions(r io.Reader, model string, options TransformOptions) (map[string]any, error) {
	id := ""
	created := time.Now().Unix()
	var content strings.Builder
	var reasoning strings.Builder
	var merged strings.Builder
	finishReason := ""
	var usage map[string]any
	toolCalls := map[int]map[string]any{}
	var toolOrder []int

	err := forEachSSEEvent(r, func(name, data string) error {
		event, err := parseEvent(name, data)
		if err != nil {
			return err
		}
		if event.CompletionID != "" {
			id = event.CompletionID
		}
		switch event.Name {
		case "output":
			switch normalizeReasoningMode(options.ReasoningMode) {
			case "merge":
				merged.WriteString(event.Reasoning)
				merged.WriteString(event.Content)
			case "drop":
				content.WriteString(event.Content)
			default:
				content.WriteString(event.Content)
				reasoning.WriteString(event.Reasoning)
			}
			mergeToolCalls(toolCalls, &toolOrder, event.ToolCalls)
		case "token_usage":
			usage = event.Usage
		case "done":
			if event.FinishReason != "" {
				finishReason = event.FinishReason
			}
		case "error":
			return &StreamError{Code: event.ErrorCode, Message: event.ErrorMessage}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if id == "" {
		id = "chatcmpl-" + newID()
	}

	if finishReason == "" {
		if len(toolOrder) > 0 {
			finishReason = "tool_calls"
		} else {
			finishReason = "stop"
		}
	}

	reasoningMode := normalizeReasoningMode(options.ReasoningMode)
	if reasoningMode == "merge" {
		content.Reset()
		content.WriteString(merged.String())
	}
	message := map[string]any{"role": "assistant"}
	if content.Len() > 0 || len(toolOrder) == 0 {
		message["content"] = content.String()
	} else {
		message["content"] = nil
	}
	if reasoningMode == "preserve" && reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sort.Ints(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, index := range toolOrder {
			call := toolCalls[index]
			if _, ok := call["type"]; !ok {
				call["type"] = "function"
			}
			calls = append(calls, call)
		}
		message["tool_calls"] = calls
	}

	response := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
	}
	if usage != nil {
		response["usage"] = usage
	}
	return response, nil
}

func StreamToOpenAI(w http.ResponseWriter, r io.Reader, model string, includeUsage bool) error {
	return StreamToOpenAIWithOptions(w, r, model, includeUsage, TransformOptions{ReasoningMode: "preserve"})
}

func StreamToOpenAIWithOptions(w http.ResponseWriter, r io.Reader, model string, includeUsage bool, options TransformOptions) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")

	controller := http.NewResponseController(w)
	id := "chatcmpl-" + newID()
	created := time.Now().Unix()
	roleSent := false
	doneSent := false
	hasToolCalls := false
	var usage map[string]any

	writeChunk := func(delta map[string]any, finishReason string) error {
		choice := map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": nil,
		}
		if finishReason != "" {
			choice["finish_reason"] = finishReason
		}
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{choice},
		}
		return writeSSEData(w, controller, chunk)
	}

	writeUsage := func() error {
		if !includeUsage || usage == nil {
			return nil
		}
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{},
			"usage":   usage,
		}
		return writeSSEData(w, controller, chunk)
	}

	writeDone := func() error {
		if doneSent {
			return nil
		}
		if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
			return err
		}
		doneSent = true
		return controller.Flush()
	}

	err := forEachSSEEvent(r, func(name, data string) error {
		event, err := parseEvent(name, data)
		if err != nil {
			return err
		}
		if event.CompletionID != "" && !roleSent {
			id = event.CompletionID
		}
		switch event.Name {
		case "output":
			delta := map[string]any{}
			if !roleSent {
				delta["role"] = "assistant"
				roleSent = true
			}
			switch normalizeReasoningMode(options.ReasoningMode) {
			case "merge":
				if event.Reasoning != "" || event.Content != "" {
					delta["content"] = event.Reasoning + event.Content
				}
			case "drop":
				if event.Content != "" {
					delta["content"] = event.Content
				}
			default:
				if event.Content != "" {
					delta["content"] = event.Content
				}
				if event.Reasoning != "" {
					delta["reasoning_content"] = event.Reasoning
				}
			}
			if calls := normalizeToolCallDelta(event.ToolCalls); len(calls) > 0 {
				hasToolCalls = true
				delta["tool_calls"] = calls
			}
			if len(delta) > 0 {
				return writeChunk(delta, "")
			}
		case "token_usage":
			usage = event.Usage
		case "done":
			if !roleSent {
				if err := writeChunk(map[string]any{"role": "assistant"}, ""); err != nil {
					return err
				}
				roleSent = true
			}
			finish := event.FinishReason
			if finish == "" {
				if hasToolCalls {
					finish = "tool_calls"
				} else {
					finish = "stop"
				}
			}
			if err := writeChunk(map[string]any{}, finish); err != nil {
				return err
			}
			if err := writeUsage(); err != nil {
				return err
			}
			return writeDone()
		case "error":
			streamErr := &StreamError{Code: event.ErrorCode, Message: event.ErrorMessage}
			errorPayload := map[string]any{"error": map[string]any{
				"message": streamErr.Message,
				"type":    "api_error",
				"code":    streamErr.Code,
			}}
			if err := writeSSEData(w, controller, errorPayload); err != nil {
				return err
			}
			if err := writeDone(); err != nil {
				return err
			}
			return streamErr
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !doneSent {
		finish := "stop"
		if hasToolCalls {
			finish = "tool_calls"
		}
		if !roleSent {
			if err := writeChunk(map[string]any{"role": "assistant"}, finish); err != nil {
				return err
			}
		} else if err := writeChunk(map[string]any{}, finish); err != nil {
			return err
		}
		if err := writeUsage(); err != nil {
			return err
		}
		return writeDone()
	}
	return nil
}

func writeSSEData(w io.Writer, controller *http.ResponseController, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
		return err
	}
	return controller.Flush()
}

func parseEvent(name, data string) (*normalizedEvent, error) {
	event := &normalizedEvent{Name: strings.TrimSpace(name)}
	data = strings.TrimSpace(data)
	if data == "" || data == "[DONE]" {
		if data == "[DONE]" && event.Name == "" {
			event.Name = "done"
		}
		return event, nil
	}

	// Auxiliary and future upstream events are intentionally ignored. TRAE has
	// emitted progress_notice as a JSON string in the wild, and unknown events
	// must never terminate an otherwise healthy completion stream.
	if !requiresObjectPayload(event.Name) {
		return event, nil
	}

	var raw map[string]any
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return nil, fmt.Errorf("decode upstream SSE %q: %w", event.Name, err)
	}

	switch event.Name {
	case "metadata":
		for _, key := range []string{"prompt_completion_id", "completion_id", "id"} {
			if value, ok := raw[key]; ok {
				event.CompletionID = normalizeCompletionID(value)
				if event.CompletionID != "" {
					break
				}
			}
		}
	case "output":
		event.Content, _ = raw["response"].(string)
		event.Reasoning, _ = raw["reasoning_content"].(string)
		if calls, ok := raw["tool_calls"]; ok {
			event.ToolCalls, _ = json.Marshal(calls)
		}
	case "token_usage":
		event.Usage = raw
	case "done":
		event.FinishReason, _ = raw["finish_reason"].(string)
	case "error":
		event.ErrorCode = scalarString(raw["code"])
		event.ErrorMessage, _ = raw["message"].(string)
		if event.ErrorMessage == "" {
			event.ErrorMessage, _ = raw["msg"].(string)
		}
	}
	return event, nil
}

func requiresObjectPayload(name string) bool {
	switch strings.TrimSpace(name) {
	case "metadata", "output", "token_usage", "done", "error":
		return true
	default:
		return false
	}
}

func normalizeReasoningMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "merge", "drop":
		return strings.ToLower(strings.TrimSpace(mode))
	default:
		return "preserve"
	}
}

func normalizeCompletionID(value any) string {
	id := scalarString(value)
	if id == "" || id == "0" || id == "<nil>" {
		return ""
	}
	if strings.HasPrefix(id, "chatcmpl-") {
		return id
	}
	return "chatcmpl-" + id
}

func scalarString(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	case json.Number:
		return v.String()
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func forEachSSEEvent(r io.Reader, fn func(name, data string) error) error {
	reader := bufio.NewReaderSize(r, 64*1024)
	var eventName string
	var dataLines []string

	dispatch := func() error {
		if eventName == "" && len(dataLines) == 0 {
			return nil
		}
		data := strings.Join(dataLines, "\n")
		if err := fn(eventName, data); err != nil {
			return err
		}
		eventName = ""
		dataLines = dataLines[:0]
		return nil
	}

	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if dispatchErr := dispatch(); dispatchErr != nil {
					return dispatchErr
				}
			case strings.HasPrefix(line, ":"):
				// SSE comment / keepalive.
			default:
				field, value, found := strings.Cut(line, ":")
				if found && strings.HasPrefix(value, " ") {
					value = value[1:]
				}
				switch field {
				case "event":
					// Some upstream builds omit the blank line between events.
					if eventName != "" && len(dataLines) > 0 {
						if dispatchErr := dispatch(); dispatchErr != nil {
							return dispatchErr
						}
					}
					eventName = value
				case "data":
					dataLines = append(dataLines, value)
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return dispatch()
			}
			return err
		}
	}
}

func normalizeToolCallDelta(raw json.RawMessage) []map[string]any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var calls []map[string]any
	if err := json.Unmarshal(raw, &calls); err != nil {
		var one map[string]any
		if json.Unmarshal(raw, &one) != nil || one == nil {
			return nil
		}
		calls = []map[string]any{one}
	}
	for _, call := range calls {
		if fn, ok := call["function_call"].(map[string]any); ok {
			call["function"] = fn
			delete(call, "function_call")
		}
		if fn, ok := call["function"].(map[string]any); ok {
			delete(fn, "namespace")
			delete(fn, "partial_arguments")
		}
		if _, ok := call["type"]; !ok {
			call["type"] = "function"
		}
	}
	return calls
}

func mergeToolCalls(toolCalls map[int]map[string]any, order *[]int, raw json.RawMessage) {
	for _, call := range normalizeToolCallDelta(raw) {
		index := 0
		switch value := call["index"].(type) {
		case float64:
			index = int(value)
		case int:
			index = value
		}
		merged, exists := toolCalls[index]
		if !exists {
			merged = map[string]any{"index": index}
			toolCalls[index] = merged
			*order = append(*order, index)
		}
		mergeToolCall(merged, call)
	}
}

func mergeToolCall(dst, src map[string]any) {
	if value, ok := src["id"].(string); ok && value != "" {
		dst["id"] = value
	}
	if value, ok := src["type"].(string); ok && value != "" {
		dst["type"] = value
	}
	srcFn, _ := src["function"].(map[string]any)
	if srcFn == nil {
		return
	}
	dstFn, _ := dst["function"].(map[string]any)
	if dstFn == nil {
		dstFn = map[string]any{}
		dst["function"] = dstFn
	}
	if value, ok := srcFn["name"].(string); ok && value != "" {
		dstFn["name"] = value
	}
	if value, ok := srcFn["arguments"].(string); ok && value != "" {
		previous, _ := dstFn["arguments"].(string)
		dstFn["arguments"] = previous + value
	}
}
