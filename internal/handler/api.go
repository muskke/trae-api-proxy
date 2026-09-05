package handler

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"

	traeauth "github.com/muskke/trae-api-proxy/internal/auth"
	"github.com/muskke/trae-api-proxy/internal/config"
	"github.com/muskke/trae-api-proxy/internal/service/trae"
	"github.com/muskke/trae-api-proxy/internal/service/websearch"
)

type APIHandler struct {
	Config    *config.Config
	Client    *trae.Client
	Auth      *traeauth.Manager
	WebSearch trae.HostedWebSearchExecutor
}

func NewAPIHandler(cfg *config.Config, client *trae.Client, authManager *traeauth.Manager) *APIHandler {
	searchRuntime := websearch.New(websearch.Config{
		Backend:      cfg.WebSearchBackend,
		Endpoint:     cfg.WebSearchEndpoint,
		Timeout:      cfg.WebSearchTimeout,
		MaxResults:   cfg.WebSearchMaxResults,
		MaxPageBytes: cfg.WebSearchMaxPageBytes,
	})
	return &APIHandler{Config: cfg, Client: client, Auth: authManager, WebSearch: searchRuntime}
}

func (h *APIHandler) HandleRoot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":   "trae-api-proxy",
		"status": "ok",
		"endpoints": []string{
			"GET /healthz",
			"GET /status",
			"GET /auth/status",
			"GET /auth/login",
			"POST /auth/refresh",
			"POST /auth/logout",
			"GET /v1/models",
			"GET /v1/models/status",
			"DELETE /v1/models/status",
			"POST /v1/chat/completions",
			"POST /v1/responses",
		},
	})
}

func (h *APIHandler) HandleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

func (h *APIHandler) HandleStatus(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeClient(r, w) {
		return
	}
	authStatus := h.Auth.Status()
	upstreamURL := authStatus.APIBaseURL
	if upstreamURL == "" {
		upstreamURL = h.Config.APIBaseURL
	}
	host := upstreamURL
	if parsed, err := url.Parse(upstreamURL); err == nil && parsed.Host != "" {
		host = parsed.Host
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":                    "ok",
		"wire_apis":                 []string{"chat_completions", "responses"},
		"upstream_mode":             h.Config.UpstreamMode,
		"upstream_host":             host,
		"upstream_url":              upstreamURL,
		"upstream_function":         h.Client.FunctionFor(trae.Session{Platform: authStatus.Platform}, h.Config.DefaultModel),
		"reasoning_mode":            h.Config.ReasoningMode,
		"model_status_persistent":   strings.TrimSpace(h.Config.ModelStatusFile) != "",
		"upstream_token_configured": authStatus.Authenticated,
		"upstream_auth_source":      authStatus.Source,
		"client_auth_enabled":       h.Config.AuthToken != "",
		"default_model":             h.Config.DefaultModel,
		"trae_auth":                 authStatus,
		"hosted_tools": map[string]any{
			"web_search": map[string]any{
				"enabled": true, "backend": h.Config.WebSearchBackend, "max_steps": h.Config.HostedToolMaxSteps,
			},
		},
	})
}

func (h *APIHandler) HandleAuthStatus(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeClient(r, w) {
		return
	}
	writeJSON(w, http.StatusOK, h.Auth.Status())
}

func (h *APIHandler) HandleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if !isLocalBrowserRequest(r) {
		writeOpenAIError(w, http.StatusForbidden, "local_login_only", "browser login must be started through localhost")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	loginURL, err := h.Auth.StartLogin(r.Context())
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "login_start_failed", err.Error())
		return
	}
	http.Redirect(w, r, loginURL, http.StatusFound)
}

func (h *APIHandler) HandleAuthCallback(w http.ResponseWriter, r *http.Request) {
	state := strings.TrimSpace(r.PathValue("state"))
	status, err := h.Auth.CompleteLogin(r.Context(), state, r.URL.Query())
	if err != nil {
		renderAuthResult(w, http.StatusBadRequest, "TRAE login failed", err.Error(), traeauth.Status{})
		return
	}
	renderAuthResult(w, http.StatusOK, "TRAE login complete", "Credentials were stored locally and automatic refresh is active.", status)
}

func (h *APIHandler) HandleAuthRefresh(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeClient(r, w) {
		return
	}
	if err := h.Auth.ForceRefresh(r.Context()); err != nil {
		h.writeAuthLifecycleError(w, err)
		return
	}
	h.Client.InvalidateModelCache()
	writeJSON(w, http.StatusOK, h.Auth.Status())
}

func (h *APIHandler) HandleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeClient(r, w) {
		return
	}
	if err := h.Auth.Logout(); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "logout_failed", err.Error())
		return
	}
	h.Client.InvalidateModelCache()
	h.Client.InvalidateModelCapabilities()
	writeJSON(w, http.StatusOK, map[string]any{"status": "logged_out"})
}

func (h *APIHandler) HandleModels(w http.ResponseWriter, r *http.Request) {
	session, ok := h.authorizeUpstream(r, w)
	if !ok {
		return
	}

	models, session, err := h.listModelsWithRefresh(r, w, session)
	if err != nil {
		h.writeModelListError(w, err)
		return
	}
	availability := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("availability")))
	if availability == "" {
		availability = "available"
	}
	models, err = h.Client.FilterModels(session, models, availability)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	w.Header().Set("X-Trae-Model-Filter", availability)
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": models})
}

func (h *APIHandler) HandleModelStatus(w http.ResponseWriter, r *http.Request) {
	session, ok := h.authorizeUpstream(r, w)
	if !ok {
		return
	}
	_, session, err := h.listModelsWithRefresh(r, w, session)
	if err != nil {
		h.writeModelListError(w, err)
		return
	}
	statuses, err := h.Client.ModelStatuses(r.Context(), session)
	if err != nil {
		h.writeUpstreamError(w, err)
		return
	}
	summary := map[string]int{"usable": 0, "unknown": 0, "unavailable": 0}
	for _, status := range statuses {
		summary[string(status.Status)]++
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":  "list",
		"data":    statuses,
		"summary": summary,
	})
}

func (h *APIHandler) HandleModelStatusReset(w http.ResponseWriter, r *http.Request) {
	session, ok := h.authorizeUpstream(r, w)
	if !ok {
		return
	}
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	removed := h.Client.ResetModelAvailability(session, model)
	response := map[string]any{"status": "ok", "removed": removed}
	if model != "" {
		response["model"] = model
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *APIHandler) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	session, ok := h.authorizeUpstream(r, w)
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
	status := h.Client.ModelAvailability(session, model)
	if status.ID != "" {
		model = status.ID
	}
	w.Header().Set("X-Proxied-Model", model)
	w.Header().Set("X-Trae-Function", h.Client.FunctionFor(session, model))
	w.Header().Set("X-Trae-Model-Status", string(status.Status))
	w.Header().Set("X-Trae-Reasoning-Mode", h.Config.ReasoningMode)
	if status.Status == trae.ModelUnavailable {
		writeOpenAIError(w, http.StatusBadRequest, "model_unavailable", fmt.Sprintf("model %q is temporarily marked unavailable for this TRAE account; retry after the cache expires or clear it with DELETE /v1/models/status?model=%s", model, url.QueryEscape(model)))
		return
	}

	capabilitySafe := isModelCapabilitySafeRequest(body)
	resp, mode, err := h.Client.ChatCompletion(r.Context(), session, model, body)
	if trae.IsUpstreamAuthError(err) && session.Source == "oauth" {
		if refreshErr := h.Auth.ForceRefresh(r.Context()); refreshErr != nil {
			h.writeAuthLifecycleError(w, refreshErr)
			return
		}
		session, ok = h.authorizeUpstream(r, w)
		if !ok {
			return
		}
		resp, mode, err = h.Client.ChatCompletion(r.Context(), session, model, body)
	}
	if err != nil {
		if h.recordModelError(session, model, err, capabilitySafe) {
			writeOpenAIError(w, http.StatusBadRequest, "model_unavailable", fmt.Sprintf("model %q was rejected by TRAE for a minimal chat request", model))
			return
		}
		h.writeUpstreamError(w, err)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("X-Upstream-Mode", mode)

	if request.Stream {
		// StreamToOpenAI sets SSE headers before the first write. Errors that happen
		// after streaming starts are encoded inside the SSE body because status 200
		// has already been committed by then.
		if err := trae.StreamToOpenAIWithOptions(w, resp.Body, model, request.StreamOptions.IncludeUsage, trae.TransformOptions{ReasoningMode: h.Config.ReasoningMode}); err != nil {
			h.recordModelError(session, model, err, capabilitySafe)
			log.Printf("stream conversion error: %v", err)
			return
		}
		h.Client.RecordModelSuccess(session, model)
		return
	}

	result, err := trae.AggregateToOpenAIWithOptions(resp.Body, model, trae.TransformOptions{ReasoningMode: h.Config.ReasoningMode})
	if err != nil {
		if h.recordModelError(session, model, err, capabilitySafe) {
			writeOpenAIError(w, http.StatusBadRequest, "model_unavailable", fmt.Sprintf("model %q was rejected by TRAE for a minimal chat request", model))
			return
		}
		var streamErr *trae.StreamError
		if errors.As(err, &streamErr) {
			writeOpenAIError(w, http.StatusBadGateway, streamErr.Code, streamErr.Message)
			return
		}
		writeOpenAIError(w, http.StatusBadGateway, "upstream_parse_error", err.Error())
		return
	}
	h.Client.RecordModelSuccess(session, model)
	writeJSON(w, http.StatusOK, result)
}

type modelListAuthError struct {
	err error
}

func (e *modelListAuthError) Error() string { return e.err.Error() }
func (e *modelListAuthError) Unwrap() error { return e.err }

func (h *APIHandler) listModelsWithRefresh(r *http.Request, w http.ResponseWriter, session trae.Session) ([]trae.Model, trae.Session, error) {
	models, err := h.Client.ListModels(r.Context(), session)
	if trae.IsUpstreamAuthError(err) && session.Source == "oauth" {
		if refreshErr := h.Auth.ForceRefresh(r.Context()); refreshErr != nil {
			return nil, session, &modelListAuthError{err: refreshErr}
		}
		h.Client.InvalidateModelCache()
		refreshed, ok := h.authorizeUpstream(r, w)
		if !ok {
			return nil, session, &modelListAuthError{err: traeauth.ErrReauthRequired}
		}
		session = refreshed
		models, err = h.Client.ListModels(r.Context(), session)
	}
	return models, session, err
}

func (h *APIHandler) writeModelListError(w http.ResponseWriter, err error) {
	var authErr *modelListAuthError
	if errors.As(err, &authErr) {
		h.writeAuthLifecycleError(w, authErr.err)
		return
	}
	h.writeUpstreamError(w, err)
}

func (h *APIHandler) recordModelError(session trae.Session, model string, err error, capabilitySafe bool) bool {
	code, message := modelErrorDetails(err)
	definitive := capabilitySafe && isModelParameterError(code, message)
	h.Client.RecordModelFailure(session, model, code, message, definitive)
	return definitive
}

func modelErrorDetails(err error) (string, string) {
	var streamErr *trae.StreamError
	if errors.As(err, &streamErr) {
		return strings.TrimSpace(streamErr.Code), strings.TrimSpace(streamErr.Message)
	}
	var upstreamErr *trae.UpstreamError
	if errors.As(err, &upstreamErr) {
		body := strings.TrimSpace(upstreamErr.Body)
		var raw map[string]any
		if json.Unmarshal([]byte(body), &raw) == nil {
			if nested, ok := raw["error"].(map[string]any); ok {
				code := scalarErrorValue(nested["code"])
				message := scalarErrorValue(nested["message"])
				if code != "" || message != "" {
					return code, message
				}
			}
			return scalarErrorValue(raw["code"]), firstErrorValue(raw["message"], raw["msg"])
		}
		return fmt.Sprintf("http_%d", upstreamErr.Status), body
	}
	return "", strings.TrimSpace(err.Error())
}

func scalarErrorValue(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return fmt.Sprintf("%.0f", v)
	case json.Number:
		return v.String()
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func firstErrorValue(values ...any) string {
	for _, value := range values {
		if text := scalarErrorValue(value); text != "" {
			return text
		}
	}
	return ""
}

func isModelParameterError(code, message string) bool {
	if strings.TrimSpace(code) == "4001" {
		return true
	}
	message = strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(message, "param is invalid") && strings.Contains(message, "valid param")
}

func isModelCapabilitySafeRequest(body []byte) bool {
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return false
	}
	allowed := map[string]bool{
		"model": true, "messages": true, "stream": true, "stream_options": true,
	}
	for key := range raw {
		if !allowed[key] {
			return false
		}
	}
	var messages []map[string]json.RawMessage
	if json.Unmarshal(raw["messages"], &messages) != nil || len(messages) == 0 {
		return false
	}
	for _, message := range messages {
		for key := range message {
			if key != "role" && key != "content" {
				return false
			}
		}
		var role string
		var content string
		if json.Unmarshal(message["role"], &role) != nil || strings.TrimSpace(role) == "" {
			return false
		}
		if json.Unmarshal(message["content"], &content) != nil {
			return false
		}
	}
	return true
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

func (h *APIHandler) authorizeUpstream(r *http.Request, w http.ResponseWriter) (trae.Session, bool) {
	if !h.authorizeClient(r, w) {
		return trae.Session{}, false
	}
	requestUpstreamToken := ""
	if h.Config.AuthToken == "" {
		requestUpstreamToken = requestToken(r)
	}
	session, err := h.Auth.ResolveSession(r.Context(), requestUpstreamToken)
	if err != nil {
		h.writeAuthLifecycleError(w, err)
		return trae.Session{}, false
	}
	if strings.TrimSpace(session.Token) == "" {
		h.writeAuthLifecycleError(w, traeauth.ErrReauthRequired)
		return trae.Session{}, false
	}
	return trae.Session{
		Token:      session.Token,
		UID:        session.UID,
		MachineID:  session.MachineID,
		DeviceID:   session.DeviceID,
		APIBaseURL: session.APIBaseURL,
		Platform:   session.Platform,
		Source:     session.Source,
	}, true
}

func (h *APIHandler) authorizeClient(r *http.Request, w http.ResponseWriter) bool {
	if h.Config.AuthToken == "" {
		return true
	}
	clientToken := requestToken(r)
	if clientToken == "" || subtle.ConstantTimeCompare([]byte(clientToken), []byte(h.Config.AuthToken)) != 1 {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
		return false
	}
	return true
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

func (h *APIHandler) writeAuthLifecycleError(w http.ResponseWriter, err error) {
	if errors.Is(err, traeauth.ErrReauthRequired) {
		writeOpenAIError(w, http.StatusServiceUnavailable, "reauth_required", "TRAE session requires login; open /auth/login locally")
		return
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "auth_refresh_failed", "TRAE session refresh failed; check /auth/status and re-login if needed")
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
		if h.Config.AuthMode == "passthrough" && h.Config.AuthToken == "" {
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

func isLocalBrowserRequest(r *http.Request) bool {
	host := r.Host
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}
	hostIsLoopback := isLoopbackHost(host)

	remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	remoteIP := net.ParseIP(strings.Trim(remoteHost, "[]"))
	if remoteIP == nil {
		return false
	}
	if remoteIP.IsLoopback() {
		return true
	}
	// Docker's localhost port publishing reaches the container from a private
	// bridge address. Requiring both a loopback Host header and a private peer
	// keeps that deployment working while rejecting public Host-header spoofing.
	return hostIsLoopback && remoteIP.IsPrivate()
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

var authResultTemplate = template.Must(template.New("auth-result").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>{{.Title}}</title>
<meta name="viewport" content="width=device-width, initial-scale=1"></head>
<body style="font-family:system-ui,sans-serif;max-width:720px;margin:48px auto;padding:0 20px;line-height:1.6">
<h1>{{.Title}}</h1><p>{{.Message}}</p>{{if .UID}}<p>UID: <code>{{.UID}}</code></p>{{end}}
{{if .Nickname}}<p>Account: {{.Nickname}}</p>{{end}}
{{if .ExpiresAt}}<p>Access token expires at Unix time <code>{{.ExpiresAt}}</code>.</p>{{end}}
<p>You can close this window and return to the proxy.</p></body></html>`))

func renderAuthResult(w http.ResponseWriter, statusCode int, title, message string, status traeauth.Status) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(statusCode)
	if err := authResultTemplate.Execute(w, map[string]any{
		"Title": title, "Message": message, "UID": status.UID, "Nickname": status.Nickname, "ExpiresAt": status.ExpiresAt,
	}); err != nil {
		log.Printf("render auth result: %v", err)
	}
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
