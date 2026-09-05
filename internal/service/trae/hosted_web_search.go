package trae

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/muskke/trae-api-proxy/internal/service/websearch"
)

type HostedWebSearchExecutor interface {
	Execute(context.Context, websearch.Action, websearch.ToolOptions) (websearch.Result, error)
	Backend() string
}

type HostedResponsesExecution struct {
	Response       map[string]any
	WebSearchCalls []map[string]any
	Mode           string
}

func (c *Client) ExecuteResponsesWithHostedTools(ctx context.Context, session Session, model string, request *ResponsesRequestContext, executor HostedWebSearchExecutor, options TransformOptions, maxSteps int) (*HostedResponsesExecution, error) {
	if request == nil || request.HostedWebSearch == nil {
		return nil, errors.New("hosted web search was not requested")
	}
	if executor == nil {
		return nil, errors.New("hosted web search runtime is not configured")
	}
	if maxSteps <= 0 {
		maxSteps = 6
	}

	var chat map[string]any
	if err := json.Unmarshal(request.ChatBody, &chat); err != nil {
		return nil, fmt.Errorf("decode translated Responses chat body: %w", err)
	}
	messages, ok := chat["messages"].([]any)
	if !ok || len(messages) == 0 {
		return nil, errors.New("translated Responses request has no messages")
	}
	// Hosted tools are executed inside this proxy. Serializing tool decisions
	// keeps the hidden tool loop deterministic and avoids mixed client/server
	// tool-call batches that cannot be completed in the same turn.
	chat["parallel_tool_calls"] = false

	var searchCalls []map[string]any
	lastMode := ""
	for step := 0; step < maxSteps; step++ {
		body, err := json.Marshal(chat)
		if err != nil {
			return nil, fmt.Errorf("encode hosted-tool chat turn: %w", err)
		}
		resp, mode, err := c.ChatCompletion(ctx, session, model, body)
		if err != nil {
			return nil, err
		}
		lastMode = mode
		result, aggregateErr := AggregateToOpenAIWithOptions(resp.Body, model, options)
		resp.Body.Close()
		if aggregateErr != nil {
			return nil, aggregateErr
		}

		message, toolCalls, err := chatResultMessage(result)
		if err != nil {
			return nil, err
		}
		hostedIndexes := make([]int, 0, len(toolCalls))
		clientIndexes := make([]int, 0, len(toolCalls))
		for i, call := range toolCalls {
			mapping := mappingForCall(requestBridge(request), call)
			if mapping.Kind == responseToolWebSearch {
				hostedIndexes = append(hostedIndexes, i)
			} else {
				clientIndexes = append(clientIndexes, i)
			}
		}

		if len(hostedIndexes) == 0 {
			response, err := responsesFromChatResult(result, model, request, searchCalls)
			if err != nil {
				return nil, err
			}
			return &HostedResponsesExecution{Response: response, WebSearchCalls: searchCalls, Mode: lastMode}, nil
		}

		if len(clientIndexes) > 0 {
			return nil, errors.New("TRAE returned hosted web_search and client-executed tools in the same turn; retrying with serialized tools is required")
		}

		assistant := map[string]any{"role": "assistant", "content": message["content"], "tool_calls": toolCalls}
		if reasoning := message["reasoning_content"]; reasoning != nil {
			assistant["reasoning_content"] = reasoning
		}
		messages = append(messages, assistant)

		for _, index := range hostedIndexes {
			call := toolCalls[index]
			callID := strings.TrimSpace(scalarString(call["id"]))
			if callID == "" {
				callID = "call_" + newID()
				call["id"] = callID
			}
			action, err := webSearchActionFromCall(call)
			searchItem := map[string]any{
				"id":     "ws_" + newID(),
				"type":   "web_search_call",
				"status": "completed",
			}
			if err != nil {
				searchItem["status"] = "failed"
				searchItem["action"] = map[string]any{"type": "search", "query": ""}
				searchCalls = append(searchCalls, searchItem)
				payload, _ := json.Marshal(map[string]any{"error": err.Error()})
				messages = append(messages, map[string]any{"role": "tool", "tool_call_id": callID, "content": string(payload)})
				continue
			}

			result, executeErr := executor.Execute(ctx, action, *request.HostedWebSearch)
			responseAction := webSearchResponseAction(result)
			if responseAction == nil {
				responseAction = webSearchActionMap(action, nil)
			}
			searchItem["action"] = responseAction
			toolPayload := map[string]any{
				"action":  result.Action,
				"sources": result.Sources,
				"content": result.Content,
			}
			if executeErr != nil {
				searchItem["status"] = "failed"
				toolPayload = map[string]any{"action": action, "error": executeErr.Error()}
			}
			searchCalls = append(searchCalls, searchItem)
			payload, _ := json.Marshal(toolPayload)
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": callID, "content": string(payload)})
		}
		chat["messages"] = messages
		// After a hosted tool result, allow the model to either answer or call
		// another hosted action (open_page/find_in_page/search refinement).
		chat["tool_choice"] = "auto"
	}
	return nil, fmt.Errorf("hosted tool loop exceeded %d steps", maxSteps)
}

func chatResultMessage(result map[string]any) (map[string]any, []map[string]any, error) {
	choices, _ := result["choices"].([]any)
	if len(choices) == 0 {
		return nil, nil, errors.New("TRAE completion did not contain choices")
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message == nil {
		return nil, nil, errors.New("TRAE completion did not contain an assistant message")
	}
	rawCalls, _ := message["tool_calls"].([]map[string]any)
	if rawCalls == nil {
		if values, ok := message["tool_calls"].([]any); ok {
			rawCalls = make([]map[string]any, 0, len(values))
			for _, value := range values {
				if call, ok := value.(map[string]any); ok {
					rawCalls = append(rawCalls, call)
				}
			}
		}
	}
	return message, rawCalls, nil
}

func mappingForCall(bridge *ResponsesToolBridge, call map[string]any) responseToolMapping {
	fn, _ := call["function"].(map[string]any)
	name := strings.TrimSpace(scalarString(fn["name"]))
	if mapping, ok := bridge.upstreamMapping(name); ok {
		return mapping
	}
	return responseToolMapping{Kind: responseToolFunction, Name: name, UpstreamName: name}
}

func webSearchActionFromCall(call map[string]any) (websearch.Action, error) {
	fn, _ := call["function"].(map[string]any)
	arguments := strings.TrimSpace(scalarString(fn["arguments"]))
	if arguments == "" {
		return websearch.Action{}, errors.New("web_search call did not include arguments")
	}
	var action websearch.Action
	if err := json.Unmarshal([]byte(arguments), &action); err != nil {
		return websearch.Action{}, fmt.Errorf("decode web_search arguments: %w", err)
	}
	action.Type = strings.ToLower(strings.TrimSpace(action.Type))
	if action.Type == "" && (strings.TrimSpace(action.Query) != "" || len(action.Queries) > 0) {
		action.Type = "search"
	}
	if action.Type == "" {
		return websearch.Action{}, errors.New("web_search action is required")
	}
	return action, nil
}

func webSearchResponseAction(result websearch.Result) map[string]any {
	return webSearchActionMap(result.Action, result.Sources)
}

func webSearchActionMap(action websearch.Action, sources []websearch.Source) map[string]any {
	out := map[string]any{"type": action.Type}
	switch action.Type {
	case "search":
		if action.Query != "" {
			out["query"] = action.Query
		}
		if len(action.Queries) > 0 {
			out["queries"] = action.Queries
		}
		if len(sources) > 0 {
			refs := make([]any, 0, len(sources))
			for _, source := range sources {
				if strings.TrimSpace(source.URL) != "" {
					refs = append(refs, map[string]any{"type": "url", "url": source.URL})
				}
			}
			if len(refs) > 0 {
				out["sources"] = refs
			}
		}
	case "open_page":
		out["url"] = action.URL
	case "find_in_page":
		out["url"] = action.URL
		out["pattern"] = action.Pattern
	}
	return out
}

func responsesFromChatResult(result map[string]any, model string, request *ResponsesRequestContext, webCalls []map[string]any) (map[string]any, error) {
	message, toolCalls, err := chatResultMessage(result)
	if err != nil {
		return nil, err
	}
	output := make([]any, 0, len(webCalls)+2+len(toolCalls))
	for _, item := range webCalls {
		output = append(output, item)
	}
	if reasoning := strings.TrimSpace(scalarString(message["reasoning_content"])); reasoning != "" {
		output = append(output, responseReasoningItem("rs_"+newID(), reasoning, "completed"))
	}
	content := ""
	if value, ok := message["content"].(string); ok {
		content = value
	}
	if content != "" || len(toolCalls) == 0 {
		output = append(output, responseMessageItem("msg_"+newID(), content, "completed"))
	}
	for _, call := range toolCalls {
		mapping := mappingForCall(requestBridge(request), call)
		if mapping.Kind == responseToolWebSearch {
			continue
		}
		output = append(output, responseToolItem(call, requestBridge(request), "completed"))
	}
	responseID := "resp_" + newID()
	if id := strings.TrimSpace(scalarString(result["id"])); id != "" {
		responseID = responseIDFromCompletion(id)
	}
	created := time.Now().Unix()
	if value, ok := result["created"].(float64); ok && value > 0 {
		created = int64(value)
	}
	usage, _ := result["usage"].(map[string]any)
	return buildResponsesObject(responseID, created, model, request, output, normalizeResponsesUsage(usage), "completed", nil), nil
}

func StreamHostedResponses(w http.ResponseWriter, execution *HostedResponsesExecution, request *ResponsesRequestContext) error {
	if execution == nil || execution.Response == nil {
		return errors.New("hosted Responses execution is empty")
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	sequence := int64(0)
	writeEvent := func(event map[string]any) error {
		sequence++
		event["sequence_number"] = sequence
		return writeResponsesEvent(w, controller, event)
	}

	responseID := scalarString(execution.Response["id"])
	model := scalarString(execution.Response["model"])
	created := time.Now().Unix()
	if value, ok := execution.Response["created_at"].(int64); ok {
		created = value
	} else if value, ok := execution.Response["created_at"].(float64); ok {
		created = int64(value)
	}
	if err := writeEvent(map[string]any{"type": "response.created", "response": buildResponsesObject(responseID, created, model, request, []any{}, nil, "in_progress", nil)}); err != nil {
		return err
	}
	if err := writeEvent(map[string]any{"type": "response.in_progress", "response": buildResponsesObject(responseID, created, model, request, []any{}, nil, "in_progress", nil)}); err != nil {
		return err
	}

	output, _ := execution.Response["output"].([]any)
	for outputIndex, rawItem := range output {
		item, _ := rawItem.(map[string]any)
		if item == nil {
			continue
		}
		typeName := strings.TrimSpace(scalarString(item["type"]))
		switch typeName {
		case "web_search_call":
			partial := map[string]any{"id": item["id"], "type": "web_search_call", "status": "in_progress"}
			if err := writeEvent(map[string]any{"type": "response.output_item.added", "output_index": outputIndex, "item": partial}); err != nil {
				return err
			}
			itemID := scalarString(item["id"])
			if err := writeEvent(map[string]any{"type": "response.web_search_call.in_progress", "item_id": itemID, "output_index": outputIndex}); err != nil {
				return err
			}
			if action, _ := item["action"].(map[string]any); strings.EqualFold(scalarString(action["type"]), "search") {
				if err := writeEvent(map[string]any{"type": "response.web_search_call.searching", "item_id": itemID, "output_index": outputIndex}); err != nil {
					return err
				}
			}
			if err := writeEvent(map[string]any{"type": "response.web_search_call.completed", "item_id": itemID, "output_index": outputIndex}); err != nil {
				return err
			}
			if err := writeEvent(map[string]any{"type": "response.output_item.done", "output_index": outputIndex, "item": item}); err != nil {
				return err
			}
		case "message":
			if err := streamCompletedMessageItem(writeEvent, outputIndex, item); err != nil {
				return err
			}
		case "reasoning":
			if err := streamCompletedReasoningItem(writeEvent, outputIndex, item); err != nil {
				return err
			}
		case "function_call", "custom_tool_call", "tool_search_call":
			if err := streamCompletedToolItem(writeEvent, outputIndex, item); err != nil {
				return err
			}
		default:
			if err := writeEvent(map[string]any{"type": "response.output_item.added", "output_index": outputIndex, "item": item}); err != nil {
				return err
			}
			if err := writeEvent(map[string]any{"type": "response.output_item.done", "output_index": outputIndex, "item": item}); err != nil {
				return err
			}
		}
	}
	return writeEvent(map[string]any{"type": "response.completed", "response": execution.Response})
}

func streamCompletedMessageItem(writeEvent func(map[string]any) error, outputIndex int, item map[string]any) error {
	itemID := scalarString(item["id"])
	if err := writeEvent(map[string]any{"type": "response.output_item.added", "output_index": outputIndex, "item": map[string]any{"id": itemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}}); err != nil {
		return err
	}
	content, _ := item["content"].([]any)
	text := ""
	if len(content) > 0 {
		if part, ok := content[0].(map[string]any); ok {
			text = scalarString(part["text"])
		}
	}
	part := map[string]any{"type": "output_text", "text": "", "annotations": []any{}}
	if err := writeEvent(map[string]any{"type": "response.content_part.added", "item_id": itemID, "output_index": outputIndex, "content_index": 0, "part": part}); err != nil {
		return err
	}
	if text != "" {
		if err := writeEvent(map[string]any{"type": "response.output_text.delta", "item_id": itemID, "output_index": outputIndex, "content_index": 0, "delta": text}); err != nil {
			return err
		}
	}
	if err := writeEvent(map[string]any{"type": "response.output_text.done", "item_id": itemID, "output_index": outputIndex, "content_index": 0, "text": text}); err != nil {
		return err
	}
	donePart := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
	if err := writeEvent(map[string]any{"type": "response.content_part.done", "item_id": itemID, "output_index": outputIndex, "content_index": 0, "part": donePart}); err != nil {
		return err
	}
	return writeEvent(map[string]any{"type": "response.output_item.done", "output_index": outputIndex, "item": item})
}

func streamCompletedReasoningItem(writeEvent func(map[string]any) error, outputIndex int, item map[string]any) error {
	itemID := scalarString(item["id"])
	partial := map[string]any{"id": itemID, "type": "reasoning", "status": "in_progress", "summary": []any{}, "content": []any{}}
	if err := writeEvent(map[string]any{"type": "response.output_item.added", "output_index": outputIndex, "item": partial}); err != nil {
		return err
	}
	text := ""
	if content, ok := item["content"].([]any); ok && len(content) > 0 {
		if part, ok := content[0].(map[string]any); ok {
			text = scalarString(part["text"])
		}
	}
	if text != "" {
		if err := writeEvent(map[string]any{"type": "response.reasoning_text.delta", "item_id": itemID, "output_index": outputIndex, "content_index": 0, "delta": text}); err != nil {
			return err
		}
		if err := writeEvent(map[string]any{"type": "response.reasoning_text.done", "item_id": itemID, "output_index": outputIndex, "content_index": 0, "text": text}); err != nil {
			return err
		}
	}
	return writeEvent(map[string]any{"type": "response.output_item.done", "output_index": outputIndex, "item": item})
}

func streamCompletedToolItem(writeEvent func(map[string]any) error, outputIndex int, item map[string]any) error {
	typeName := scalarString(item["type"])
	if err := writeEvent(map[string]any{"type": "response.output_item.added", "output_index": outputIndex, "item": item}); err != nil {
		return err
	}
	itemID := scalarString(item["id"])
	switch typeName {
	case "function_call":
		arguments := scalarString(item["arguments"])
		if arguments != "" {
			if err := writeEvent(map[string]any{"type": "response.function_call_arguments.delta", "item_id": itemID, "output_index": outputIndex, "delta": arguments}); err != nil {
				return err
			}
		}
		if err := writeEvent(map[string]any{"type": "response.function_call_arguments.done", "item_id": itemID, "output_index": outputIndex, "arguments": arguments}); err != nil {
			return err
		}
	case "custom_tool_call":
		input := scalarString(item["input"])
		if input != "" {
			if err := writeEvent(map[string]any{"type": "response.custom_tool_call_input.delta", "item_id": itemID, "output_index": outputIndex, "delta": input}); err != nil {
				return err
			}
		}
		if err := writeEvent(map[string]any{"type": "response.custom_tool_call_input.done", "item_id": itemID, "output_index": outputIndex, "input": input}); err != nil {
			return err
		}
	}
	return writeEvent(map[string]any{"type": "response.output_item.done", "output_index": outputIndex, "item": item})
}
