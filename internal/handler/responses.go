package handler

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"

	"github.com/muskke/trae-api-proxy/internal/service/trae"
)

// HandleResponses implements the stateless text/function-tool subset of the
// OpenAI Responses API. It shares the TRAE transport and model capability
// learning layer with /v1/chat/completions.
func (h *APIHandler) HandleResponses(w http.ResponseWriter, r *http.Request) {
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

	responseRequest, err := trae.ConvertResponsesRequest(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	model := h.resolveModel(responseRequest.RequestedModel)
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
	w.Header().Set("X-Trae-Wire-API", "responses")
	if status.Status == trae.ModelUnavailable {
		writeOpenAIError(w, http.StatusBadRequest, "model_unavailable", fmt.Sprintf("model %q is temporarily marked unavailable for this TRAE account; retry after the cache expires or clear it with DELETE /v1/models/status?model=%s", model, url.QueryEscape(model)))
		return
	}

	capabilitySafe := isModelCapabilitySafeRequest(responseRequest.ChatBody)
	resp, mode, err := h.Client.ChatCompletion(r.Context(), session, model, responseRequest.ChatBody)
	if trae.IsUpstreamAuthError(err) && session.Source == "oauth" {
		if refreshErr := h.Auth.ForceRefresh(r.Context()); refreshErr != nil {
			h.writeAuthLifecycleError(w, refreshErr)
			return
		}
		session, ok = h.authorizeUpstream(r, w)
		if !ok {
			return
		}
		resp, mode, err = h.Client.ChatCompletion(r.Context(), session, model, responseRequest.ChatBody)
	}
	if err != nil {
		if h.recordModelError(session, model, err, capabilitySafe) {
			writeOpenAIError(w, http.StatusBadRequest, "model_unavailable", fmt.Sprintf("model %q was rejected by TRAE for a minimal Responses request", model))
			return
		}
		h.writeUpstreamError(w, err)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("X-Upstream-Mode", mode)

	options := trae.TransformOptions{ReasoningMode: h.Config.ReasoningMode}
	if responseRequest.Stream {
		if err := trae.StreamToResponses(w, resp.Body, model, responseRequest, options); err != nil {
			h.recordModelError(session, model, err, capabilitySafe)
			log.Printf("responses stream conversion error: %v", err)
			return
		}
		h.Client.RecordModelSuccess(session, model)
		return
	}

	result, err := trae.AggregateToResponses(resp.Body, model, responseRequest, options)
	if err != nil {
		if h.recordModelError(session, model, err, capabilitySafe) {
			writeOpenAIError(w, http.StatusBadRequest, "model_unavailable", fmt.Sprintf("model %q was rejected by TRAE for a minimal Responses request", model))
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
