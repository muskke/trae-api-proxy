package handler

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/muskke/trae-api-proxy/internal/config"
	"github.com/muskke/trae-api-proxy/internal/service/trae"
)

type APIHandler struct {
	Config *config.Config
	Client *trae.Client
}

func NewAPIHandler(cfg *config.Config, client *trae.Client) *APIHandler {
	return &APIHandler{Config: cfg, Client: client}
}

func (h *APIHandler) HandleRoot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":   "trae-api-proxy",
		"status": "ok",
		"endpoints": []string{
			"GET /healthz",
			"GET /status",
			"GET /v1/models",
			"POST /v1/chat/completions",
		},
	})
}

func (h *APIHandler) HandleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

func (h *APIHandler) HandleStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.authorize(r, w); !ok {
		return
	}
	host := h.Config.APIBaseURL
	if parsed, err := url.Parse(h.Config.APIBaseURL); err == nil && parsed.Host != "" {
		host = parsed.Host
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":                    "ok",
		"upstream_mode":             h.Config.UpstreamMode,
		"upstream_host":             host,
		"upstream_token_configured": h.Config.IdeToken != "",
		"client_auth_enabled":       h.Config.AuthToken != "",
		"default_model":             h.Config.DefaultModel,
	})
}

func (h *APIHandler) HandleModels(w http.ResponseWriter, r *http.Request) {
	upstreamToken, ok := h.authorize(r, w)
	if !ok {
		return
	}

	models, err := h.Client.ListModels(r.Context(), upstreamToken)
	if err != nil {
		h.writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": models})
}

func (h *APIHandler) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	upstreamToken, ok := h.authorize(r, w)
	if !ok {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, h.Config.MaxBodyBytes+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "failed to read request body")
		return
	}
	if int64(len(body)) > h.Config.MaxBodyBytes {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", fmt.Sprintf("request body exceeds %d bytes", h.Config.MaxBodyBytes))
		return
	}

	var request struct {
		Model         string            `json:"model"`
		Messages      []json.RawMessage `json:"messages"`
		Stream        bool              `json:"stream"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_json", "request body must be valid JSON")
		return
	}
	if len(request.Messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "messages must be a non-empty array")
		return
	}

	model := h.resolveModel(request.Model)
	if model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	w.Header().Set("X-Proxied-Model", model)

	resp, mode, err := h.Client.ChatCompletion(r.Context(), upstreamToken, model, body)
	if err != nil {
		h.writeUpstreamError(w, err)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("X-Upstream-Mode", mode)

	if request.Stream {
		// StreamToOpenAI sets SSE headers before the first write. Errors that happen
		// after streaming starts are encoded inside the SSE body because status 200
		// has already been committed by then.
		if err := trae.StreamToOpenAI(w, resp.Body, model, request.StreamOptions.IncludeUsage); err != nil {
			log.Printf("stream conversion error: %v", err)
		}
		return
	}

	result, err := trae.AggregateToOpenAI(resp.Body, model)
	if err != nil {
		var streamErr *trae.StreamError
		if errors.As(err, &streamErr) {
			writeOpenAIError(w, http.StatusBadGateway, streamErr.Code, streamErr.Message)
			return
		}
		writeOpenAIError(w, http.StatusBadGateway, "upstream_parse_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *APIHandler) resolveModel(requested string) string {
	model := strings.TrimSpace(requested)
	if index := strings.Index(model, "__"); index >= 0 {
		model = model[:index]
	}
	if model == "" || strings.EqualFold(model, "auto") {
		model = h.Config.DefaultModel
	}
	if target, ok := h.Config.ModelAliases[strings.ToLower(model)]; ok {
		return target
	}
	return model
}

func (h *APIHandler) authorize(r *http.Request, w http.ResponseWriter) (string, bool) {
	clientToken := requestToken(r)
	if h.Config.AuthToken != "" {
		if clientToken == "" || subtle.ConstantTimeCompare([]byte(clientToken), []byte(h.Config.AuthToken)) != 1 {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return "", false
		}
		return h.Config.IdeToken, true
	}
	if h.Config.IdeToken != "" {
		return h.Config.IdeToken, true
	}
	if clientToken == "" {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing upstream token; configure TRAE_IDE_TOKEN or send a Bearer token")
		return "", false
	}
	return clientToken, true
}

func requestToken(r *http.Request) string {
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	if authorization != "" {
		parts := strings.Fields(authorization)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			return strings.TrimSpace(parts[1])
		}
	}
	return strings.TrimSpace(r.Header.Get("X-API-Key"))
}

func (h *APIHandler) writeUpstreamError(w http.ResponseWriter, err error) {
	var upstreamErr *trae.UpstreamError
	if !errors.As(err, &upstreamErr) {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}

	status := http.StatusBadGateway
	code := "upstream_error"
	switch upstreamErr.Status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		status = http.StatusBadRequest
		code = "invalid_request"
	case http.StatusTooManyRequests:
		status = http.StatusTooManyRequests
		code = "rate_limit_exceeded"
	case http.StatusUnauthorized, http.StatusForbidden:
		if h.Config.AuthToken == "" && h.Config.IdeToken == "" {
			status = http.StatusUnauthorized
			code = "invalid_api_key"
		} else {
			code = "upstream_auth_error"
		}
	case http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout:
		status = http.StatusServiceUnavailable
		code = "upstream_unavailable"
	}
	writeOpenAIError(w, status, code, upstreamErr.Error())
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("encode response: %v", err)
	}
}

func writeOpenAIError(w http.ResponseWriter, status int, code, message string) {
	typeName := "api_error"
	switch status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		typeName = "invalid_request_error"
	case http.StatusUnauthorized, http.StatusForbidden:
		typeName = "authentication_error"
	case http.StatusTooManyRequests:
		typeName = "rate_limit_error"
	}
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"message": message,
		"type":    typeName,
		"code":    code,
	}})
}
