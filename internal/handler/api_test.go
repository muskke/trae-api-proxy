package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muskke/trae-api-proxy/internal/config"
	"github.com/muskke/trae-api-proxy/internal/service/trae"
)

func TestChatAuthUsesConfiguredUpstreamToken(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Cloud-IDE-JWT upstream-secret" {
			t.Errorf("upstream authorization = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: metadata\ndata: {\"prompt_completion_id\":\"123\"}\n\nevent: output\ndata: {\"response\":\"hello\"}\n\nevent: done\ndata: {}\n\n")
	}))
	defer upstream.Close()

	h := newTestHandler(upstream.URL)

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	request.Header.Set("Authorization", "Bearer client-secret")
	recorder := httptest.NewRecorder()
	h.HandleChatCompletions(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d", calls.Load())
	}
	if !strings.Contains(recorder.Body.String(), `"object":"chat.completion"`) || !strings.Contains(recorder.Body.String(), `"content":"hello"`) {
		t.Fatalf("unexpected response: %s", recorder.Body.String())
	}
	if recorder.Header().Get("X-Upstream-Mode") != "solo" {
		t.Fatalf("upstream mode header = %q", recorder.Header().Get("X-Upstream-Mode"))
	}
}

func TestChatRejectsInvalidClientTokenBeforeUpstream(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	h := newTestHandler(upstream.URL)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer wrong")
	recorder := httptest.NewRecorder()
	h.HandleChatCompletions(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream should not be called, got %d", calls.Load())
	}
	if !strings.Contains(recorder.Body.String(), `"code":"invalid_api_key"`) {
		t.Fatalf("unexpected error body: %s", recorder.Body.String())
	}
}

func TestStreamingResponseSetsSSEHeadersBeforeWrite(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: output\ndata: {\"response\":\"hello\"}\n\nevent: token_usage\ndata: {\"total_tokens\":2}\n\nevent: done\ndata: {}\n\n")
	}))
	defer upstream.Close()

	h := newTestHandler(upstream.URL)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`))
	request.Header.Set("Authorization", "Bearer client-secret")
	recorder := httptest.NewRecorder()
	h.HandleChatCompletions(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("content-type = %q", got)
	}
	if !strings.Contains(recorder.Body.String(), `"usage":{"total_tokens":2}`) || !strings.HasSuffix(recorder.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("unexpected stream: %s", recorder.Body.String())
	}
}

func TestHealthDoesNotRequireAuthentication(t *testing.T) {
	h := newTestHandler("http://127.0.0.1:1")
	recorder := httptest.NewRecorder()
	h.HandleHealth(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "ok" {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func newTestHandler(baseURL string) *APIHandler {
	cfg := &config.Config{
		AuthToken:          "client-secret",
		IdeToken:           "upstream-secret",
		AppID:              config.DefaultAppID,
		AppVersion:         config.DefaultApplicationVer,
		DeviceBrand:        config.DefaultDeviceBrand,
		DeviceType:         config.DefaultDeviceType,
		IDEVersion:         config.DefaultIDEVersion,
		IDEVersionCode:     config.DefaultIDEVersionCode,
		IDEVersionType:     config.DefaultIDEVersionType,
		OSVersion:          config.DefaultOSVersion,
		APIBaseURL:         baseURL,
		Locale:             "zh-cn",
		UpstreamMode:       "solo",
		DefaultModel:       config.DefaultModel,
		ModelAliases:       map[string]string{},
		RequestTrafficType: config.DefaultRequestTraffic,
		RequestTimeout:     3 * time.Second,
		HeaderTimeout:      3 * time.Second,
		ModelCacheTTL:      time.Minute,
		MaxBodyBytes:       1 << 20,
	}
	return NewAPIHandler(cfg, trae.NewClient(cfg))
}
