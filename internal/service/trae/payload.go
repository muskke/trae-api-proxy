package trae

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func prepareCurrentBody(src []byte, model, function string) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return nil, fmt.Errorf("invalid JSON request: %w", err)
	}
	messages, ok := obj["messages"].([]any)
	if !ok || len(messages) == 0 {
		return nil, fmt.Errorf("messages must be a non-empty array")
	}

	obj["stream"] = true
	obj["function"] = function
	if function == autoChatFunction || strings.EqualFold(strings.TrimSpace(model), "auto") {
		delete(obj, "config_name")
		delete(obj, "model")
	} else {
		obj["model"] = model
		obj["config_name"] = model
	}
	delete(obj, "stream_options")

	if _, exists := obj["max_tokens"]; !exists {
		if value, ok := obj["max_completion_tokens"]; ok {
			obj["max_tokens"] = value
		}
	}
	delete(obj, "max_completion_tokens")

	normalizeMessages(messages)
	normalizeToolChoice(obj)
	normalizeTools(obj)

	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("encode solo request: %w", err)
	}
	return out, nil
}

func normalizeMessages(messages []any) {
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}

		if role, _ := message["role"].(string); role == "assistant" {
			if rawCalls, ok := message["tool_calls"].([]any); ok {
				calls := make([]any, 0, len(rawCalls))
				for _, rawCall := range rawCalls {
					call, ok := rawCall.(map[string]any)
					if !ok {
						continue
					}
					if fn, ok := call["function"].(map[string]any); ok {
						call["function_call"] = fn
						delete(call, "function")
					}
					if fn, ok := call["function_call"].(map[string]any); ok {
						name, _ := fn["name"].(string)
						if strings.TrimSpace(name) == "" {
							continue
						}
					}
					calls = append(calls, call)
				}
				if len(calls) == 0 {
					delete(message, "tool_calls")
				} else {
					message["tool_calls"] = calls
				}
			}
		}

		content, present := message["content"]
		if !present || content == nil {
			continue
		}
		if text, ok := content.(string); ok {
			message["content"] = []any{map[string]any{"type": "text", "text": text}}
		}
	}
}

func normalizeToolChoice(obj map[string]any) {
	suppressTools := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}

	value, ok := obj["tool_choice"]
	if !ok {
		return
	}
	switch choice := value.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(choice), "none") {
			delete(obj, "tool_choice")
			suppressTools()
		}
	case map[string]any:
		typeName, _ := choice["type"].(string)
		switch strings.ToLower(strings.TrimSpace(typeName)) {
		case "none":
			delete(obj, "tool_choice")
			suppressTools()
		case "auto", "required":
			obj["tool_choice"] = strings.ToLower(strings.TrimSpace(typeName))
		case "function":
			name := ""
			if fn, ok := choice["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = choice["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

func normalizeTools(obj map[string]any) {
	raw, ok := obj["tools"]
	if !ok {
		return
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return
	}
	tools := make([]any, 0, len(list))
	for _, item := range list {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		if params, exists := fn["parameters"]; exists {
			switch value := params.(type) {
			case map[string]any, []any:
				if rawParams, err := json.Marshal(value); err == nil {
					fn["parameters"] = string(rawParams)
				}
			}
		}
		tools = append(tools, tool)
	}
	if len(tools) == 0 {
		delete(obj, "tools")
		return
	}
	obj["tools"] = tools
}

func prepareLegacyBody(src []byte, model, locale string) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return nil, fmt.Errorf("invalid JSON request: %w", err)
	}
	messages, ok := obj["messages"].([]any)
	if !ok || len(messages) == 0 {
		return nil, fmt.Errorf("messages must be a non-empty array")
	}

	currentTurn := 0
	history := make([]any, 0, len(messages)-1)
	for _, rawMessage := range messages[:len(messages)-1] {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		if role == "user" {
			currentTurn++
		}
		history = append(history, map[string]any{
			"role":    role,
			"content": flattenContent(message["content"]),
			"status":  "success",
			"locale":  locale,
		})
	}

	last, _ := messages[len(messages)-1].(map[string]any)
	vars, _ := json.Marshal(map[string]string{
		"locale":       locale,
		"current_time": time.Now().Format("20060102 15:04:05 Monday"),
	})

	payload := map[string]any{
		"chat_history":                 history,
		"context_resolvers":            []any{},
		"conversation_id":              newID(),
		"current_turn":                 currentTurn,
		"generate_suggested_questions": false,
		"intent_name":                  "general_qa_intent",
		"is_preset":                    true,
		"model_name":                   model,
		"session_id":                   newID(),
		"stream":                       true,
		"user_input":                   flattenContent(last["content"]),
		"valid_turns":                  []int{},
		"variables":                    string(vars),
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode legacy request: %w", err)
	}
	return out, nil
}

func flattenContent(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		var b strings.Builder
		for _, rawPart := range value {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			typeName, _ := part["type"].(string)
			if typeName != "" && typeName != "text" && typeName != "input_text" {
				continue
			}
			text, _ := part["text"].(string)
			b.WriteString(text)
		}
		return b.String()
	default:
		return ""
	}
}

func newID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}
