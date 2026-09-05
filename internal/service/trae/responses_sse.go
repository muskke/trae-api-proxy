package trae

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// AggregateToResponses translates a TRAE SSE completion into an OpenAI
// Responses API response object.
func AggregateToResponses(r io.Reader, model string, request *ResponsesRequestContext, options TransformOptions) (map[string]any, error) {
	responseID := "resp_" + newID()
	created := time.Now().Unix()
	var content strings.Builder
	var reasoning strings.Builder
	var merged strings.Builder
	var usage map[string]any
	toolCalls := map[int]map[string]any{}
	var toolOrder []int

	err := forEachSSEEvent(r, func(name, data string) error {
		event, err := parseEvent(name, data)
		if err != nil {
			return err
		}
		if event.CompletionID != "" {
			responseID = responseIDFromCompletion(event.CompletionID)
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
		case "error":
			return &StreamError{Code: event.ErrorCode, Message: event.ErrorMessage}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if normalizeReasoningMode(options.ReasoningMode) == "merge" {
		content.Reset()
		content.WriteString(merged.String())
	}

	output := make([]any, 0, 2+len(toolOrder))
	if normalizeReasoningMode(options.ReasoningMode) == "preserve" && reasoning.Len() > 0 {
		output = append(output, responseReasoningItem("rs_"+newID(), reasoning.String(), "completed"))
	}
	if content.Len() > 0 || len(toolOrder) == 0 {
		output = append(output, responseMessageItem("msg_"+newID(), content.String(), "completed"))
	}
	if len(toolOrder) > 0 {
		sort.Ints(toolOrder)
		for _, index := range toolOrder {
			output = append(output, responseToolItem(toolCalls[index], requestBridge(request), "completed"))
		}
	}

	return buildResponsesObject(responseID, created, model, request, output, normalizeResponsesUsage(usage), "completed", nil), nil
}

// StreamToResponses translates TRAE SSE into the event sequence consumed by
// current OpenAI Responses clients, including Codex custom providers.
func StreamToResponses(w http.ResponseWriter, r io.Reader, model string, request *ResponsesRequestContext, options TransformOptions) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")

	controller := http.NewResponseController(w)
	responseID := "resp_" + newID()
	created := time.Now().Unix()
	sequence := int64(0)
	var usage map[string]any

	type messageState struct {
		started     bool
		outputIndex int
		itemID      string
		text        strings.Builder
	}
	type reasoningState struct {
		started     bool
		outputIndex int
		itemID      string
		text        strings.Builder
	}
	type toolState struct {
		started     bool
		outputIndex int
		itemID      string
		callID      string
		upstream    string
		mapping     responseToolMapping
		arguments   strings.Builder
	}

	message := messageState{}
	reasoning := reasoningState{}
	tools := map[int]*toolState{}
	nextOutputIndex := 0
	outputItems := map[int]any{}

	writeEvent := func(event map[string]any) error {
		sequence++
		if _, ok := event["sequence_number"]; !ok {
			event["sequence_number"] = sequence
		}
		return writeResponsesEvent(w, controller, event)
	}

	createdEvent := map[string]any{
		"type":     "response.created",
		"response": buildResponsesObject(responseID, created, model, request, []any{}, nil, "in_progress", nil),
	}
	if err := writeEvent(createdEvent); err != nil {
		return err
	}
	if err := writeEvent(map[string]any{
		"type":     "response.in_progress",
		"response": buildResponsesObject(responseID, created, model, request, []any{}, nil, "in_progress", nil),
	}); err != nil {
		return err
	}

	startMessage := func() error {
		if message.started {
			return nil
		}
		message.started = true
		message.outputIndex = nextOutputIndex
		nextOutputIndex++
		message.itemID = "msg_" + newID()
		if err := writeEvent(map[string]any{
			"type":         "response.output_item.added",
			"output_index": message.outputIndex,
			"item": map[string]any{
				"id": message.itemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{},
			},
		}); err != nil {
			return err
		}
		return writeEvent(map[string]any{
			"type":          "response.content_part.added",
			"item_id":       message.itemID,
			"output_index":  message.outputIndex,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	}

	writeMessageDelta := func(delta string) error {
		if delta == "" {
			return nil
		}
		if err := startMessage(); err != nil {
			return err
		}
		message.text.WriteString(delta)
		return writeEvent(map[string]any{
			"type":          "response.output_text.delta",
			"item_id":       message.itemID,
			"output_index":  message.outputIndex,
			"content_index": 0,
			"delta":         delta,
		})
	}

	startReasoning := func() error {
		if reasoning.started {
			return nil
		}
		reasoning.started = true
		reasoning.outputIndex = nextOutputIndex
		nextOutputIndex++
		reasoning.itemID = "rs_" + newID()
		return writeEvent(map[string]any{
			"type":         "response.output_item.added",
			"output_index": reasoning.outputIndex,
			"item": map[string]any{
				"id": reasoning.itemID, "type": "reasoning", "status": "in_progress", "summary": []any{}, "content": []any{},
			},
		})
	}

	writeReasoningDelta := func(delta string) error {
		if delta == "" {
			return nil
		}
		if err := startReasoning(); err != nil {
			return err
		}
		reasoning.text.WriteString(delta)
		return writeEvent(map[string]any{
			"type":          "response.reasoning_text.delta",
			"item_id":       reasoning.itemID,
			"output_index":  reasoning.outputIndex,
			"content_index": 0,
			"delta":         delta,
		})
	}

	ensureTool := func(index int, call map[string]any) (*toolState, error) {
		state := tools[index]
		if state == nil {
			state = &toolState{outputIndex: -1, itemID: "fc_" + newID(), callID: "call_" + newID()}
			tools[index] = state
		}
		if id := strings.TrimSpace(scalarString(call["id"])); id != "" && !state.started {
			state.callID = id
		}
		fn, _ := call["function"].(map[string]any)
		if fn != nil {
			if name := strings.TrimSpace(scalarString(fn["name"])); name != "" {
				state.upstream = name
				if mapping, ok := requestBridge(request).upstreamMapping(name); ok {
					state.mapping = mapping
				} else {
					state.mapping = responseToolMapping{Kind: responseToolFunction, Name: name, UpstreamName: name}
				}
			}
			if arguments, ok := fn["arguments"].(string); ok && arguments != "" {
				state.arguments.WriteString(arguments)
				if state.mapping.Kind == responseToolFunction && state.started {
					if err := writeEvent(map[string]any{
						"type":         "response.function_call_arguments.delta",
						"item_id":      state.itemID,
						"output_index": state.outputIndex,
						"delta":        arguments,
					}); err != nil {
						return nil, err
					}
				}
			}
		}
		if !state.started && state.upstream != "" {
			state.started = true
			state.outputIndex = nextOutputIndex
			nextOutputIndex++
			item := responseToolItemFromParts(state.itemID, state.callID, state.mapping, "", "in_progress")
			if err := writeEvent(map[string]any{"type": "response.output_item.added", "output_index": state.outputIndex, "item": item}); err != nil {
				return nil, err
			}
			// If argument text arrived before the tool name, publish the buffered
			// arguments now as one delta for ordinary function tools.
			if state.mapping.Kind == responseToolFunction && state.arguments.Len() > 0 {
				if err := writeEvent(map[string]any{
					"type": "response.function_call_arguments.delta", "item_id": state.itemID,
					"output_index": state.outputIndex, "delta": state.arguments.String(),
				}); err != nil {
					return nil, err
				}
			}
		}
		return state, nil
	}

	closeReasoning := func() error {
		if !reasoning.started {
			return nil
		}
		text := reasoning.text.String()
		if err := writeEvent(map[string]any{
			"type": "response.reasoning_text.done", "item_id": reasoning.itemID,
			"output_index": reasoning.outputIndex, "content_index": 0, "text": text,
		}); err != nil {
			return err
		}
		item := responseReasoningItem(reasoning.itemID, text, "completed")
		outputItems[reasoning.outputIndex] = item
		return writeEvent(map[string]any{"type": "response.output_item.done", "output_index": reasoning.outputIndex, "item": item})
	}

	closeMessage := func() error {
		if !message.started {
			return nil
		}
		text := message.text.String()
		if err := writeEvent(map[string]any{
			"type": "response.output_text.done", "item_id": message.itemID,
			"output_index": message.outputIndex, "content_index": 0, "text": text,
		}); err != nil {
			return err
		}
		part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
		if err := writeEvent(map[string]any{
			"type": "response.content_part.done", "item_id": message.itemID,
			"output_index": message.outputIndex, "content_index": 0, "part": part,
		}); err != nil {
			return err
		}
		item := responseMessageItem(message.itemID, text, "completed")
		outputItems[message.outputIndex] = item
		return writeEvent(map[string]any{"type": "response.output_item.done", "output_index": message.outputIndex, "item": item})
	}

	closeTools := func() error {
		indexes := make([]int, 0, len(tools))
		for index := range tools {
			indexes = append(indexes, index)
		}
		sort.Ints(indexes)
		for _, index := range indexes {
			state := tools[index]
			if !state.started {
				if state.upstream == "" {
					continue
				}
				var err error
				state, err = ensureTool(index, map[string]any{"function": map[string]any{"name": state.upstream}})
				if err != nil {
					return err
				}
			}
			arguments := state.arguments.String()
			switch state.mapping.Kind {
			case responseToolCustom:
				input := customToolInput(arguments)
				if err := writeEvent(map[string]any{
					"type": "response.custom_tool_call_input.delta", "item_id": state.itemID,
					"output_index": state.outputIndex, "delta": input,
				}); err != nil {
					return err
				}
				if err := writeEvent(map[string]any{
					"type": "response.custom_tool_call_input.done", "item_id": state.itemID,
					"output_index": state.outputIndex, "input": input,
				}); err != nil {
					return err
				}
			case responseToolSearch:
				// Client-executed tool_search is represented by its completed output
				// item. Codex dispatches that item locally and returns a
				// tool_search_output on the next Responses request.
			default:
				if err := writeEvent(map[string]any{
					"type": "response.function_call_arguments.done", "item_id": state.itemID,
					"output_index": state.outputIndex, "arguments": arguments,
				}); err != nil {
					return err
				}
			}
			item := responseToolItemFromParts(state.itemID, state.callID, state.mapping, arguments, "completed")
			outputItems[state.outputIndex] = item
			if err := writeEvent(map[string]any{"type": "response.output_item.done", "output_index": state.outputIndex, "item": item}); err != nil {
				return err
			}
		}
		return nil
	}

	finalize := func(status string, responseError map[string]any) error {
		if err := closeReasoning(); err != nil {
			return err
		}
		if err := closeMessage(); err != nil {
			return err
		}
		if err := closeTools(); err != nil {
			return err
		}
		indexes := make([]int, 0, len(outputItems))
		for index := range outputItems {
			indexes = append(indexes, index)
		}
		sort.Ints(indexes)
		output := make([]any, 0, len(indexes))
		for _, index := range indexes {
			output = append(output, outputItems[index])
		}
		response := buildResponsesObject(responseID, created, model, request, output, normalizeResponsesUsage(usage), status, responseError)
		eventType := "response.completed"
		if status == "failed" {
			eventType = "response.failed"
		}
		return writeEvent(map[string]any{"type": eventType, "response": response})
	}

	doneSeen := false
	err := forEachSSEEvent(r, func(name, data string) error {
		event, err := parseEvent(name, data)
		if err != nil {
			return err
		}
		if event.CompletionID != "" {
			// response.created has already been emitted, so preserve the public
			// response id for stream consistency rather than changing ids midstream.
		}
		switch event.Name {
		case "output":
			switch normalizeReasoningMode(options.ReasoningMode) {
			case "merge":
				if err := writeMessageDelta(event.Reasoning + event.Content); err != nil {
					return err
				}
			case "drop":
				if err := writeMessageDelta(event.Content); err != nil {
					return err
				}
			default:
				if err := writeReasoningDelta(event.Reasoning); err != nil {
					return err
				}
				if err := writeMessageDelta(event.Content); err != nil {
					return err
				}
			}
			for _, call := range normalizeToolCallDelta(event.ToolCalls) {
				index := responseToolCallIndex(call)
				if _, err := ensureTool(index, call); err != nil {
					return err
				}
			}
		case "token_usage":
			usage = event.Usage
		case "done":
			doneSeen = true
			return finalize("completed", nil)
		case "error":
			streamErr := &StreamError{Code: event.ErrorCode, Message: event.ErrorMessage}
			responseError := map[string]any{"code": streamErr.Code, "message": streamErr.Message}
			if err := finalize("failed", responseError); err != nil {
				return err
			}
			return streamErr
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !doneSeen {
		return finalize("completed", nil)
	}
	return nil
}

func writeResponsesEvent(w io.Writer, controller *http.ResponseController, event map[string]any) error {
	typeName := strings.TrimSpace(scalarString(event["type"]))
	if typeName == "" {
		return fmt.Errorf("Responses SSE event has no type")
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typeName, raw); err != nil {
		return err
	}
	return controller.Flush()
}

func responseIDFromCompletion(completionID string) string {
	id := strings.TrimSpace(completionID)
	id = strings.TrimPrefix(id, "chatcmpl-")
	id = strings.TrimPrefix(id, "resp_")
	if id == "" {
		id = newID()
	}
	return "resp_" + id
}

func requestBridge(request *ResponsesRequestContext) *ResponsesToolBridge {
	if request == nil || request.Bridge == nil {
		return newResponsesToolBridge()
	}
	return request.Bridge
}

func responseMessageItem(id, text, status string) map[string]any {
	return map[string]any{
		"id": id, "type": "message", "status": status, "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
	}
}

func responseReasoningItem(id, text, status string) map[string]any {
	item := map[string]any{
		"id": id, "type": "reasoning", "status": status, "summary": []any{},
	}
	if text != "" {
		item["content"] = []any{map[string]any{"type": "reasoning_text", "text": text}}
	}
	return item
}

func responseToolItem(call map[string]any, bridge *ResponsesToolBridge, status string) map[string]any {
	callID := strings.TrimSpace(scalarString(call["id"]))
	if callID == "" {
		callID = "call_" + newID()
	}
	fn, _ := call["function"].(map[string]any)
	name := strings.TrimSpace(scalarString(fn["name"]))
	arguments := scalarString(fn["arguments"])
	mapping, ok := bridge.upstreamMapping(name)
	if !ok {
		mapping = responseToolMapping{Kind: responseToolFunction, Name: name, UpstreamName: name}
	}
	return responseToolItemFromParts("fc_"+newID(), callID, mapping, arguments, status)
}

func responseToolItemFromParts(itemID, callID string, mapping responseToolMapping, arguments, status string) map[string]any {
	if mapping.Kind == "" {
		mapping.Kind = responseToolFunction
	}
	if mapping.Name == "" {
		mapping.Name = mapping.UpstreamName
	}
	item := map[string]any{
		"id": itemID, "status": status, "call_id": callID, "name": mapping.Name,
	}
	if mapping.Namespace != "" {
		item["namespace"] = mapping.Namespace
	}
	switch mapping.Kind {
	case responseToolCustom:
		item["type"] = "custom_tool_call"
		item["input"] = customToolInput(arguments)
	case responseToolSearch:
		item["type"] = "tool_search_call"
		delete(item, "name")
		execution := mapping.Execution
		if execution == "" {
			execution = "client"
		}
		item["execution"] = execution
		var parsed any
		if err := json.Unmarshal([]byte(arguments), &parsed); err == nil {
			item["arguments"] = parsed
		} else {
			item["arguments"] = map[string]any{"query": arguments}
		}
	default:
		item["type"] = "function_call"
		item["arguments"] = arguments
	}
	return item
}

func responseToolCallIndex(call map[string]any) int {
	switch value := call["index"].(type) {
	case float64:
		return int(value)
	case int:
		return value
	case json.Number:
		if number, err := value.Int64(); err == nil {
			return int(number)
		}
	}
	return 0
}

func normalizeResponsesUsage(raw map[string]any) map[string]any {
	input := numericValue(raw, "input_tokens", "prompt_tokens")
	output := numericValue(raw, "output_tokens", "completion_tokens")
	total := numericValue(raw, "total_tokens")
	if total == float64(0) && (input != float64(0) || output != float64(0)) {
		total = addNumbers(input, output)
	}
	usage := map[string]any{
		"input_tokens":  input,
		"output_tokens": output,
		"total_tokens":  total,
	}
	if raw != nil {
		if details, ok := raw["input_tokens_details"].(map[string]any); ok {
			usage["input_tokens_details"] = details
		}
		if details, ok := raw["output_tokens_details"].(map[string]any); ok {
			usage["output_tokens_details"] = details
		}
		if reasoning := numericValue(raw, "reasoning_tokens"); reasoning != float64(0) {
			usage["output_tokens_details"] = map[string]any{"reasoning_tokens": reasoning}
		}
	}
	if _, ok := usage["input_tokens_details"]; !ok {
		usage["input_tokens_details"] = nil
	}
	if _, ok := usage["output_tokens_details"]; !ok {
		usage["output_tokens_details"] = nil
	}
	return usage
}

func numericValue(raw map[string]any, keys ...string) any {
	for _, key := range keys {
		if raw == nil {
			break
		}
		if value, ok := raw[key]; ok && value != nil {
			switch value.(type) {
			case float64, float32, int, int32, int64, json.Number:
				return value
			}
		}
	}
	return float64(0)
}

func addNumbers(left, right any) float64 {
	toFloat := func(value any) float64 {
		switch v := value.(type) {
		case float64:
			return v
		case float32:
			return float64(v)
		case int:
			return float64(v)
		case int32:
			return float64(v)
		case int64:
			return float64(v)
		case json.Number:
			f, _ := v.Float64()
			return f
		default:
			return 0
		}
	}
	return toFloat(left) + toFloat(right)
}

func buildResponsesObject(id string, created int64, model string, request *ResponsesRequestContext, output []any, usage map[string]any, status string, responseError map[string]any) map[string]any {
	parallel := true
	instructions := ""
	toolChoice := any("none")
	tools := []any{}
	metadata := map[string]any{}
	reasoning := map[string]any{"effort": nil, "summary": nil}
	text := any(map[string]any{"format": map[string]any{"type": "text"}})
	var maxOutput any
	var temperature any
	var topP any
	if request != nil {
		parallel = request.ParallelToolCalls
		instructions = request.Instructions
		tools = append([]any{}, request.OriginalTools...)
		metadata = request.Metadata
		if request.ToolChoice != nil {
			toolChoice = request.ToolChoice
		} else if len(tools) > 0 {
			toolChoice = "auto"
		}
		if request.ReasoningEffort != "" {
			reasoning["effort"] = request.ReasoningEffort
		}
		if request.ReasoningSummary != "" {
			reasoning["summary"] = request.ReasoningSummary
		}
		if request.Text != nil {
			text = request.Text
		}
		maxOutput = request.MaxOutputTokens
		temperature = request.Temperature
		topP = request.TopP
	}

	response := map[string]any{
		"id":                   id,
		"object":               "response",
		"created_at":           created,
		"status":               status,
		"error":                responseError,
		"incomplete_details":   nil,
		"instructions":         instructions,
		"max_output_tokens":    maxOutput,
		"model":                model,
		"output":               output,
		"parallel_tool_calls":  parallel,
		"previous_response_id": nil,
		"reasoning":            reasoning,
		"store":                false,
		"temperature":          temperature,
		"text":                 text,
		"tool_choice":          toolChoice,
		"tools":                tools,
		"top_p":                topP,
		"truncation":           "disabled",
		"usage":                usage,
		"metadata":             metadata,
	}
	if status == "completed" {
		response["completed_at"] = time.Now().Unix()
	}
	if usage == nil && status != "in_progress" {
		response["usage"] = normalizeResponsesUsage(nil)
	}
	return response
}
