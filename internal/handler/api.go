package handler

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/muskke/trae-api-proxy/internal/service/trae"

	"github.com/openai/openai-go"
)

type APIHandler struct {
	Client *trae.Client
}

func NewAPIHandler(client *trae.Client) *APIHandler {
	return &APIHandler{Client: client}
}

func bearerToToken(v string) string {
	return strings.TrimPrefix(v, "Bearer ")
}

func (h *APIHandler) HandleModels(w http.ResponseWriter, r *http.Request) {
	ideToken := bearerToToken(r.Header.Get("Authorization"))

	models, err := h.Client.ListModels(r.Context(), ideToken)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   models,
	})
}

func (h *APIHandler) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	ideToken := bearerToToken(r.Header.Get("Authorization"))

	// We decode into a map first or the openai struct.
	// Since openai-go request structs often have specific pointer fields,
	// let's use a simpler custom struct for decoding the incoming request
	// OR use the one from the lib if it JSON unmarshals cleanly.
	// The official lib uses strict typing.
	// Let's decode into a struct that matches what we need for Trae,
	// but mapping from OpenAI format.
	var req struct {
		Model    string                         `json:"model"`
		Messages []openai.ChatCompletionMessage `json:"messages"`
		Stream   bool                           `json:"stream"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	w.Header().Set("X-Proxied-Model", req.Model)

	// Forward to Trae Client
	resp, err := h.Client.ChatCompletionStream(r.Context(), ideToken, req.Model, req.Messages)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", 500)
		return
	}

	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	reader := bufio.NewReader(resp.Body)

	// Use openai-go types for response
	chunk := openai.ChatCompletionChunk{
		Object:  "chat.completion.chunk",
		Model:   req.Model,
		Created: time.Now().Unix(),
	}

	eventName := ""
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}

		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))

		if data == "[DONE]" {
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}

		var evtData map[string]any
		if err := json.Unmarshal([]byte(data), &evtData); err != nil {
			log.Printf("stream unmarshal error: %v, data: %s", err, data)
			continue
		}

		switch eventName {
		case "metadata":
			if id, ok := evtData["prompt_completion_id"]; ok {
				chunk.ID = fmt.Sprint(id)
			}

		case "output":
			if response, ok := evtData["response"].(string); ok {
				chunk.Choices = []openai.ChatCompletionChunkChoice{
					{
						Delta: openai.ChatCompletionChunkChoiceDelta{
							Role:    "assistant",
							Content: response,
						},
					},
				}
				out, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", out)
				flusher.Flush()
			}

		case "done":
			chunk.Choices = []openai.ChatCompletionChunkChoice{
				{
					Delta: openai.ChatCompletionChunkChoiceDelta{
						Role: "assistant",
					},
					FinishReason: "stop",
				},
			}
			out, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", out)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}
	}
}
