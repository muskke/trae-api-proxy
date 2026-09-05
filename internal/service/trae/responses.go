package trae

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ResponsesRequestContext contains the OpenAI Responses request metadata that
// is needed both before the TRAE request is sent and while the TRAE SSE stream
// is translated back to Responses events.
type ResponsesRequestContext struct {
	RequestedModel    string
	Stream            bool
	ChatBody          []byte
	Bridge            *ResponsesToolBridge
	Instructions      string
	ParallelToolCalls bool
	ToolChoice        any
	OriginalTools     []any
	ReasoningEffort   string
	ReasoningSummary  string
	MaxOutputTokens   any
	Temperature       any
	TopP              any
	Text              any
	Metadata          map[string]any
}

type responseToolKind string

const (
	responseToolFunction responseToolKind = "function"
	responseToolCustom   responseToolKind = "custom"
	responseToolSearch   responseToolKind = "tool_search"
)

type responseToolMapping struct {
	Kind         responseToolKind
	Name         string
	Namespace    string
	UpstreamName string
	Execution    string
}

// ResponsesToolBridge remembers how Responses API tools were represented as
// ordinary function tools for TRAE. TRAE currently exposes function calling,
// so Responses custom/freeform and namespace tools are losslessly identified
// at the API boundary and represented with a function-shaped transport inside
// the proxy.
type ResponsesToolBridge struct {
	byUpstream map[string]responseToolMapping
	byResponse map[string]responseToolMapping
}

func newResponsesToolBridge() *ResponsesToolBridge {
	return &ResponsesToolBridge{
		byUpstream: make(map[string]responseToolMapping),
		byResponse: make(map[string]responseToolMapping),
	}
}

func responseToolKey(kind responseToolKind, namespace, name string) string {
	return string(kind) + "\x00" + strings.TrimSpace(namespace) + "\x00" + strings.TrimSpace(name)
}

func (b *ResponsesToolBridge) responseMapping(kind responseToolKind, namespace, name string) (responseToolMapping, bool) {
	if b == nil {
		return responseToolMapping{}, false
	}
	mapping, ok := b.byResponse[responseToolKey(kind, namespace, name)]
	return mapping, ok
}

func (b *ResponsesToolBridge) upstreamMapping(name string) (responseToolMapping, bool) {
	if b == nil {
		return responseToolMapping{}, false
	}
	mapping, ok := b.byUpstream[strings.TrimSpace(name)]
	return mapping, ok
}

func (b *ResponsesToolBridge) add(mapping responseToolMapping) {
	if b == nil {
		return
	}
	b.byUpstream[mapping.UpstreamName] = mapping
	b.byResponse[responseToolKey(mapping.Kind, mapping.Namespace, mapping.Name)] = mapping
}

var responseToolNameChars = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func normalizeUpstreamToolName(name string) string {
	name = responseToolNameChars.ReplaceAllString(strings.TrimSpace(name), "_")
	name = strings.Trim(name, "_")
	if name == "" {
		name = "tool"
	}
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

func allocateUpstreamToolName(used map[string]bool, namespace, name string) string {
	base := name
	if strings.TrimSpace(namespace) != "" {
		base = namespace + "__" + name
	}
	base = normalizeUpstreamToolName(base)
	candidate := base
	for i := 2; used[candidate]; i++ {
		suffix := fmt.Sprintf("_%d", i)
		trimmed := base
		if len(trimmed)+len(suffix) > 64 {
			trimmed = trimmed[:64-len(suffix)]
		}
		candidate = trimmed + suffix
	}
	used[candidate] = true
	return candidate
}

// ConvertResponsesRequest converts the text/function-call subset of the
// OpenAI Responses API into the Chat Completions shape already understood by
// the TRAE transport. It intentionally rejects server-hosted tool types such
// as MCP/web_search because those require an execution runtime, not merely a
// wire-format translation.
func ConvertResponsesRequest(src []byte) (*ResponsesRequestContext, error) {
	var raw map[string]any
	if err := json.Unmarshal(src, &raw); err != nil {
		return nil, fmt.Errorf("request body must be valid JSON: %w", err)
	}

	if enabled, _ := raw["background"].(bool); enabled {
		return nil, fmt.Errorf("background responses are not supported")
	}
	if enabled, _ := raw["store"].(bool); enabled {
		return nil, fmt.Errorf("store=true is not supported; use stateless Responses requests")
	}
	if value := strings.TrimSpace(scalarString(raw["previous_response_id"])); value != "" {
		return nil, fmt.Errorf("previous_response_id is not supported; send the prior response items in input")
	}
	if value := raw["conversation"]; value != nil {
		return nil, fmt.Errorf("conversation persistence is not supported")
	}
	if err := validateResponsesTextFormat(raw["text"]); err != nil {
		return nil, err
	}

	ctx := &ResponsesRequestContext{
		RequestedModel:    strings.TrimSpace(scalarString(raw["model"])),
		Stream:            false,
		Bridge:            newResponsesToolBridge(),
		Instructions:      strings.TrimSpace(scalarString(raw["instructions"])),
		ParallelToolCalls: true,
		ToolChoice:        raw["tool_choice"],
		MaxOutputTokens:   raw["max_output_tokens"],
		Temperature:       raw["temperature"],
		TopP:              raw["top_p"],
		Text:              raw["text"],
		Metadata:          map[string]any{},
	}
	if stream, ok := raw["stream"].(bool); ok {
		ctx.Stream = stream
	}
	if parallel, ok := raw["parallel_tool_calls"].(bool); ok {
		ctx.ParallelToolCalls = parallel
	}
	if metadata, ok := raw["metadata"].(map[string]any); ok {
		ctx.Metadata = metadata
	}
	if reasoning, ok := raw["reasoning"].(map[string]any); ok {
		ctx.ReasoningEffort = strings.TrimSpace(scalarString(reasoning["effort"]))
		ctx.ReasoningSummary = strings.TrimSpace(scalarString(reasoning["summary"]))
	}

	allTools, err := collectResponsesTools(raw["tools"], raw["input"])
	if err != nil {
		return nil, err
	}
	chatTools, originalTools, err := convertResponsesTools(allTools, ctx.Bridge)
	if err != nil {
		return nil, err
	}
	ctx.OriginalTools = originalTools

	messages, err := convertResponsesInput(raw["input"], ctx.Instructions, ctx.Bridge)
	if err != nil {
		return nil, err
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("input must contain at least one text message or tool result")
	}

	chat := map[string]any{
		"model":    ctx.RequestedModel,
		"messages": messages,
		"stream":   true,
	}
	if len(chatTools) > 0 {
		chat["tools"] = chatTools
	}
	if choice := convertResponsesToolChoice(raw["tool_choice"], ctx.Bridge, len(chatTools) > 0); choice != nil {
		chat["tool_choice"] = choice
	}
	if ctx.MaxOutputTokens != nil {
		chat["max_completion_tokens"] = ctx.MaxOutputTokens
	}
	if ctx.Temperature != nil {
		chat["temperature"] = ctx.Temperature
	}
	if ctx.TopP != nil {
		chat["top_p"] = ctx.TopP
	}

	body, err := json.Marshal(chat)
	if err != nil {
		return nil, fmt.Errorf("encode translated Responses request: %w", err)
	}
	ctx.ChatBody = body
	return ctx, nil
}

func validateResponsesTextFormat(value any) error {
	text, ok := value.(map[string]any)
	if !ok || text == nil {
		return nil
	}
	format, ok := text["format"].(map[string]any)
	if !ok || format == nil {
		return nil
	}
	typeName := strings.ToLower(strings.TrimSpace(scalarString(format["type"])))
	if typeName == "" || typeName == "text" {
		return nil
	}
	return fmt.Errorf("text.format.type=%q is not supported by the TRAE bridge", typeName)
}

func convertResponsesTools(value any, bridge *ResponsesToolBridge) ([]any, []any, error) {
	if value == nil {
		return nil, nil, nil
	}
	list, ok := value.([]any)
	if !ok {
		return nil, nil, fmt.Errorf("tools must be an array")
	}
	used := map[string]bool{}
	chatTools := make([]any, 0, len(list))
	original := append([]any(nil), list...)

	for _, rawTool := range list {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("each tool must be an object")
		}
		typeName := strings.ToLower(strings.TrimSpace(scalarString(tool["type"])))
		switch typeName {
		case "function":
			converted, mapping, err := convertResponsesFunctionTool(tool, "", used)
			if err != nil {
				return nil, nil, err
			}
			bridge.add(mapping)
			chatTools = append(chatTools, converted)
		case "custom":
			converted, mapping, err := convertResponsesCustomTool(tool, "", used)
			if err != nil {
				return nil, nil, err
			}
			bridge.add(mapping)
			chatTools = append(chatTools, converted)
		case "tool_search":
			converted, mapping, err := convertResponsesToolSearch(tool, used)
			if err != nil {
				return nil, nil, err
			}
			bridge.add(mapping)
			chatTools = append(chatTools, converted)
		case "namespace":
			namespace := strings.TrimSpace(scalarString(tool["name"]))
			if namespace == "" {
				return nil, nil, fmt.Errorf("namespace tool requires a name")
			}
			children, ok := tool["tools"].([]any)
			if !ok || len(children) == 0 {
				return nil, nil, fmt.Errorf("namespace tool %q must contain tools", namespace)
			}
			for _, rawChild := range children {
				child, ok := rawChild.(map[string]any)
				if !ok {
					return nil, nil, fmt.Errorf("namespace tool %q contains an invalid child tool", namespace)
				}
				childType := strings.ToLower(strings.TrimSpace(scalarString(child["type"])))
				switch childType {
				case "function":
					converted, mapping, err := convertResponsesFunctionTool(child, namespace, used)
					if err != nil {
						return nil, nil, err
					}
					bridge.add(mapping)
					chatTools = append(chatTools, converted)
				case "custom":
					converted, mapping, err := convertResponsesCustomTool(child, namespace, used)
					if err != nil {
						return nil, nil, err
					}
					bridge.add(mapping)
					chatTools = append(chatTools, converted)
				default:
					return nil, nil, fmt.Errorf("namespace tool %q contains unsupported tool type %q", namespace, childType)
				}
			}
		case "":
			return nil, nil, fmt.Errorf("tool type is required")
		default:
			return nil, nil, fmt.Errorf("tool type %q requires a hosted tool runtime and is not supported by this proxy; use client-executed function/custom tools", typeName)
		}
	}
	return chatTools, original, nil
}

func collectResponsesTools(topLevel any, input any) ([]any, error) {
	var tools []any
	if topLevel != nil {
		list, ok := topLevel.([]any)
		if !ok {
			return nil, fmt.Errorf("tools must be an array")
		}
		tools = append(tools, list...)
	}
	items, ok := input.([]any)
	if !ok {
		return tools, nil
	}
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		typeName := strings.ToLower(strings.TrimSpace(scalarString(item["type"])))
		if typeName != "tool_search_output" && typeName != "additional_tools" {
			continue
		}
		loaded, ok := item["tools"].([]any)
		if !ok {
			if item["tools"] == nil {
				continue
			}
			return nil, fmt.Errorf("%s.tools must be an array", typeName)
		}
		tools = append(tools, loaded...)
	}
	return tools, nil
}

func convertResponsesToolSearch(tool map[string]any, used map[string]bool) (map[string]any, responseToolMapping, error) {
	execution := strings.ToLower(strings.TrimSpace(scalarString(tool["execution"])))
	if execution == "" {
		execution = "client"
	}
	if execution != "client" {
		return nil, responseToolMapping{}, fmt.Errorf("tool_search execution=%q requires a hosted tool runtime; only client execution is supported", execution)
	}
	upstreamName := allocateUpstreamToolName(used, "", "tool_search")
	parameters := tool["parameters"]
	if parameters == nil {
		parameters = map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string"},
				"limit": map[string]any{"type": "integer", "minimum": 1},
			},
			"required": []any{"query"},
		}
	}
	fn := map[string]any{
		"name":       upstreamName,
		"parameters": parameters,
	}
	if description := strings.TrimSpace(scalarString(tool["description"])); description != "" {
		fn["description"] = description
	}
	mapping := responseToolMapping{
		Kind: responseToolSearch, Name: "tool_search", UpstreamName: upstreamName, Execution: execution,
	}
	return map[string]any{"type": "function", "function": fn}, mapping, nil
}

func convertResponsesFunctionTool(tool map[string]any, namespace string, used map[string]bool) (map[string]any, responseToolMapping, error) {
	name := strings.TrimSpace(scalarString(tool["name"]))
	if name == "" {
		return nil, responseToolMapping{}, fmt.Errorf("function tool requires a name")
	}
	upstreamName := allocateUpstreamToolName(used, namespace, name)
	fn := map[string]any{
		"name":       upstreamName,
		"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
	}
	if description := strings.TrimSpace(scalarString(tool["description"])); description != "" {
		fn["description"] = description
	}
	if parameters := tool["parameters"]; parameters != nil {
		fn["parameters"] = parameters
	}
	if strict, ok := tool["strict"].(bool); ok {
		fn["strict"] = strict
	}
	mapping := responseToolMapping{Kind: responseToolFunction, Name: name, Namespace: namespace, UpstreamName: upstreamName}
	return map[string]any{"type": "function", "function": fn}, mapping, nil
}

func convertResponsesCustomTool(tool map[string]any, namespace string, used map[string]bool) (map[string]any, responseToolMapping, error) {
	name := strings.TrimSpace(scalarString(tool["name"]))
	if name == "" {
		return nil, responseToolMapping{}, fmt.Errorf("custom tool requires a name")
	}
	upstreamName := allocateUpstreamToolName(used, namespace, name)
	description := strings.TrimSpace(scalarString(tool["description"]))
	if description != "" {
		description += "\n\n"
	}
	description += "This is a freeform tool transported through a function-compatible backend. Put the complete raw tool input in the JSON string field named input."
	fn := map[string]any{
		"name":        upstreamName,
		"description": description,
		"parameters": map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"input": map[string]any{"type": "string"}},
			"required":             []any{"input"},
			"additionalProperties": false,
		},
	}
	mapping := responseToolMapping{Kind: responseToolCustom, Name: name, Namespace: namespace, UpstreamName: upstreamName}
	return map[string]any{"type": "function", "function": fn}, mapping, nil
}

func convertResponsesToolChoice(value any, bridge *ResponsesToolBridge, hasTools bool) any {
	if value == nil {
		if hasTools {
			return "auto"
		}
		return nil
	}
	if text, ok := value.(string); ok {
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "none":
			return "none"
		case "required":
			return "required"
		case "auto":
			if hasTools {
				return "auto"
			}
			return nil
		}
		return nil
	}
	choice, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	typeName := strings.ToLower(strings.TrimSpace(scalarString(choice["type"])))
	name := strings.TrimSpace(scalarString(choice["name"]))
	namespace := strings.TrimSpace(scalarString(choice["namespace"]))
	kind := responseToolKind(typeName)
	if typeName == "function" || typeName == "custom" {
		if mapping, ok := bridge.responseMapping(kind, namespace, name); ok {
			return map[string]any{"type": "function", "function": map[string]any{"name": mapping.UpstreamName}}
		}
	}
	if typeName == "tool_search" {
		if mapping, ok := bridge.responseMapping(responseToolSearch, "", "tool_search"); ok {
			return map[string]any{"type": "function", "function": map[string]any{"name": mapping.UpstreamName}}
		}
	}
	return "auto"
}

func convertResponsesInput(value any, instructions string, bridge *ResponsesToolBridge) ([]any, error) {
	messages := make([]any, 0, 8)
	if strings.TrimSpace(instructions) != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}

	if text, ok := value.(string); ok {
		if strings.TrimSpace(text) != "" {
			messages = append(messages, map[string]any{"role": "user", "content": text})
		}
		return messages, nil
	}
	items, ok := value.([]any)
	if !ok {
		if value == nil {
			return messages, nil
		}
		return nil, fmt.Errorf("input must be a string or an array of Responses input items")
	}

	pendingCalls := make([]any, 0, 2)
	flushCalls := func() {
		if len(pendingCalls) == 0 {
			return
		}
		messages = append(messages, map[string]any{"role": "assistant", "content": nil, "tool_calls": pendingCalls})
		pendingCalls = nil
	}

	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("each input item must be an object")
		}
		typeName := strings.ToLower(strings.TrimSpace(scalarString(item["type"])))
		if typeName == "" && item["role"] != nil {
			typeName = "message"
		}
		switch typeName {
		case "message", "easy_input_message":
			flushCalls()
			role := strings.ToLower(strings.TrimSpace(scalarString(item["role"])))
			switch role {
			case "developer", "system":
				role = "system"
			case "user", "assistant":
			default:
				return nil, fmt.Errorf("unsupported message role %q", role)
			}
			content, err := responsesContentToText(item["content"])
			if err != nil {
				return nil, err
			}
			messages = append(messages, map[string]any{"role": role, "content": content})
		case "function_call":
			name := strings.TrimSpace(scalarString(item["name"]))
			namespace := strings.TrimSpace(scalarString(item["namespace"]))
			upstreamName := name
			if mapping, ok := bridge.responseMapping(responseToolFunction, namespace, name); ok {
				upstreamName = mapping.UpstreamName
			}
			callID := strings.TrimSpace(scalarString(item["call_id"]))
			if callID == "" {
				callID = "call_" + newID()
			}
			arguments := scalarString(item["arguments"])
			pendingCalls = append(pendingCalls, map[string]any{
				"id": callID, "type": "function", "function": map[string]any{"name": upstreamName, "arguments": arguments},
			})
		case "custom_tool_call":
			name := strings.TrimSpace(scalarString(item["name"]))
			namespace := strings.TrimSpace(scalarString(item["namespace"]))
			upstreamName := name
			if mapping, ok := bridge.responseMapping(responseToolCustom, namespace, name); ok {
				upstreamName = mapping.UpstreamName
			}
			callID := strings.TrimSpace(scalarString(item["call_id"]))
			if callID == "" {
				callID = "call_" + newID()
			}
			argumentBytes, _ := json.Marshal(map[string]any{"input": scalarString(item["input"])})
			pendingCalls = append(pendingCalls, map[string]any{
				"id": callID, "type": "function", "function": map[string]any{"name": upstreamName, "arguments": string(argumentBytes)},
			})
		case "tool_search_call":
			callID := strings.TrimSpace(scalarString(item["call_id"]))
			if callID == "" {
				callID = "call_" + newID()
			}
			mapping, ok := bridge.responseMapping(responseToolSearch, "", "tool_search")
			upstreamName := "tool_search"
			if ok {
				upstreamName = mapping.UpstreamName
			}
			arguments := item["arguments"]
			argumentBytes, err := json.Marshal(arguments)
			if err != nil {
				return nil, fmt.Errorf("encode tool_search arguments: %w", err)
			}
			pendingCalls = append(pendingCalls, map[string]any{
				"id": callID, "type": "function", "function": map[string]any{"name": upstreamName, "arguments": string(argumentBytes)},
			})
		case "function_call_output", "custom_tool_call_output":
			flushCalls()
			callID := strings.TrimSpace(scalarString(item["call_id"]))
			if callID == "" {
				return nil, fmt.Errorf("%s requires call_id", typeName)
			}
			output, err := responsesToolOutputToText(item["output"])
			if err != nil {
				return nil, err
			}
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": callID, "content": output})
		case "tool_search_output":
			flushCalls()
			callID := strings.TrimSpace(scalarString(item["call_id"]))
			if callID == "" {
				return nil, fmt.Errorf("tool_search_output requires call_id")
			}
			payload := map[string]any{
				"status": item["status"], "execution": item["execution"], "tools": item["tools"],
			}
			rawPayload, err := json.Marshal(payload)
			if err != nil {
				return nil, fmt.Errorf("encode tool_search output: %w", err)
			}
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": callID, "content": string(rawPayload)})
		case "additional_tools":
			// Definitions are merged into the translated top-level tool list by
			// collectResponsesTools. No conversational message is needed.
		case "reasoning":
			// Reasoning items are provider continuity artifacts. TRAE does not
			// accept encrypted reasoning state, so they are intentionally omitted.
		case "":
			return nil, fmt.Errorf("input item type is required")
		default:
			return nil, fmt.Errorf("unsupported Responses input item type %q", typeName)
		}
	}
	flushCalls()
	return messages, nil
}

func responsesContentToText(value any) (string, error) {
	if value == nil {
		return "", nil
	}
	if text, ok := value.(string); ok {
		return text, nil
	}
	parts, ok := value.([]any)
	if !ok {
		return "", fmt.Errorf("message content must be a string or an array")
	}
	var b strings.Builder
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			return "", fmt.Errorf("message content item must be an object")
		}
		typeName := strings.ToLower(strings.TrimSpace(scalarString(part["type"])))
		switch typeName {
		case "input_text", "output_text", "text":
			b.WriteString(scalarString(part["text"]))
		case "":
			if text := scalarString(part["text"]); text != "" {
				b.WriteString(text)
				continue
			}
			return "", fmt.Errorf("message content item type is required")
		default:
			return "", fmt.Errorf("message content type %q is not supported by the text-only TRAE bridge", typeName)
		}
	}
	return b.String(), nil
}

func responsesToolOutputToText(value any) (string, error) {
	switch output := value.(type) {
	case nil:
		return "", nil
	case string:
		return output, nil
	case []any:
		var parts []string
		for _, rawPart := range output {
			part, ok := rawPart.(map[string]any)
			if !ok {
				return "", fmt.Errorf("tool output array items must be objects")
			}
			typeName := strings.ToLower(strings.TrimSpace(scalarString(part["type"])))
			if typeName != "input_text" && typeName != "output_text" && typeName != "text" {
				return "", fmt.Errorf("tool output content type %q is not supported", typeName)
			}
			parts = append(parts, scalarString(part["text"]))
		}
		return strings.Join(parts, "\n"), nil
	case map[string]any:
		if content := scalarString(output["content"]); content != "" {
			return content, nil
		}
		raw, err := json.Marshal(output)
		if err != nil {
			return "", fmt.Errorf("encode tool output: %w", err)
		}
		return string(raw), nil
	default:
		return scalarString(output), nil
	}
}

func customToolInput(arguments string) string {
	var value map[string]any
	if json.Unmarshal([]byte(arguments), &value) == nil {
		if input, ok := value["input"].(string); ok {
			return input
		}
	}
	return arguments
}
