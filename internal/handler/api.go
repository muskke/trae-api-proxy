package handler

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
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
	resp, err := h.Client.ChatCompletion(r.Context(), ideToken, req.Model, req.Messages, req.Stream)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer resp.Body.Close()

	var flusher http.Flusher
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		var ok bool
		flusher, ok = w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming not supported", 500)
			return
		}

		w.WriteHeader(http.StatusOK)
		flusher.Flush()
	}

	reader := bufio.NewReader(resp.Body)

	// Common response structure
	chunk := openai.ChatCompletionChunk{
		Object:  "chat.completion.chunk",
		Model:   req.Model,
		Created: time.Now().Unix(),
	}

	// For non-streaming, we aggregate the content
	var fullContent strings.Builder
	var completionID string

	eventName := ""
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			if !req.Stream {
				log.Printf("error reading stream for aggregation: %v", err)
				http.Error(w, err.Error(), 500)
				return
			}
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
			if req.Stream {
				fmt.Fprintf(w, "data: [DONE]\n\n")
				flusher.Flush()
			}
			break
		}

		var evtData map[string]any
		if err := json.Unmarshal([]byte(data), &evtData); err != nil {
			log.Printf("stream unmarshal error: %v, data: %s", err, data)
			continue
		}

		switch eventName {
		case "metadata":
			if id, ok := evtData["prompt_completion_id"]; ok {
				completionID = fmt.Sprint(id)
				chunk.ID = completionID
			}

		case "output":
			if response, ok := evtData["response"].(string); ok {
				if req.Stream {
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
				} else {
					fullContent.WriteString(response)
				}
			}

		case "done":
			if req.Stream {
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
			}
			// For non-streaming, we break here and return the aggregated response
			goto Finish
		}
	}

Finish:
	if !req.Stream {
		resp := openai.ChatCompletion{
			ID:      completionID,
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Model:   req.Model,
			Choices: []openai.ChatCompletionChoice{
				{
					Index: 0,
					Message: openai.ChatCompletionMessage{
						Role:    "assistant",
						Content: fullContent.String(),
					},
					FinishReason: "stop",
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Printf("error encoding response: %v", err)
		}
	}
}
