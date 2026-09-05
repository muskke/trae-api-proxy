package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	traeauth "github.com/muskke/trae-api-proxy/internal/auth"
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

func TestOAuthSessionRefreshesOnUpstreamUnauthorizedAndRetriesOnce(t *testing.T) {
	var chatCalls atomic.Int32
	var exchangeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/agent/v3/llm_utils_chat":
			call := chatCalls.Add(1)
			if got := r.Header.Get("X-Uid"); got != "uid-oauth" {
				t.Errorf("uid header = %q", got)
			}
			if got := r.Header.Get("X-Machine-Id"); got != "machine-oauth" {
				t.Errorf("machine header = %q", got)
			}
			if got := r.Header.Get("X-Device-Id"); got != "device-oauth" {
				t.Errorf("device header = %q", got)
			}
			if call == 1 {
				if got := r.Header.Get("Authorization"); got != "Cloud-IDE-JWT access-old" {
					t.Errorf("first authorization = %q", got)
				}
				http.Error(w, "token expired", http.StatusUnauthorized)
				return
			}
			if got := r.Header.Get("Authorization"); got != "Cloud-IDE-JWT access-new" {
				t.Errorf("retry authorization = %q", got)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: output\ndata: {\"response\":\"recovered\"}\n\nevent: done\ndata: {}\n\n")
		case config.DefaultOAuthExchangePath:
			exchangeCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"Result": map[string]any{
				"Token": "access-new", "RefreshToken": "refresh-new",
				"TokenExpireAt": time.Now().Add(48*time.Hour).Unix() * 1000,
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	authFile := filepath.Join(dir, "trae-auth.json")
	cred := map[string]any{
		"version": 1,
		"account": map[string]any{"uid": "uid-oauth", "nickname": "tester"},
		"auth": map[string]any{
			"accessToken": "access-old", "refreshToken": "refresh-old",
			"expiresAt": time.Now().Add(48 * time.Hour).Unix(),
			"apiHost":   server.URL, "machineId": "machine-oauth", "deviceId": "device-oauth",
		},
	}
	raw, _ := json.Marshal(cred)
	if err := os.WriteFile(authFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		AuthToken:          "client-secret",
		AuthMode:           "oauth",
		AuthFile:           authFile,
		AppID:              config.DefaultAppID,
		AppVersion:         config.DefaultApplicationVer,
		DeviceBrand:        config.DefaultDeviceBrand,
		DeviceType:         config.DefaultDeviceType,
		IDEVersion:         config.DefaultIDEVersion,
		IDEVersionCode:     config.DefaultIDEVersionCode,
		IDEVersionType:     config.DefaultIDEVersionType,
		OSVersion:          config.DefaultOSVersion,
		APIBaseURL:         server.URL,
		Locale:             "zh-cn",
		UpstreamMode:       "solo",
		DefaultModel:       config.DefaultModel,
		ModelAliases:       map[string]string{},
		RequestTrafficType: config.DefaultRequestTraffic,
		RequestTimeout:     3 * time.Second,
		HeaderTimeout:      3 * time.Second,
		ModelCacheTTL:      time.Minute,
		MaxBodyBytes:       1 << 20,
		OAuthHost:          server.URL,
		OAuthClientID:      config.DefaultOAuthClientID,
		OAuthExchangePath:  config.DefaultOAuthExchangePath,
		OAuthUserInfoPath:  config.DefaultOAuthUserInfoPath,
		OAuthRefreshSkew:   24 * time.Hour,
		OAuthRefreshEvery:  time.Minute,
		OAuthLoginTTL:      10 * time.Minute,
	}
	authManager := traeauth.NewManager(cfg)
	h := NewAPIHandler(cfg, trae.NewClient(cfg), authManager)

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	request.Header.Set("Authorization", "Bearer client-secret")
	recorder := httptest.NewRecorder()
	h.HandleChatCompletions(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if chatCalls.Load() != 2 || exchangeCalls.Load() != 1 {
		t.Fatalf("chat calls=%d exchange calls=%d", chatCalls.Load(), exchangeCalls.Load())
	}
	if !strings.Contains(recorder.Body.String(), `"content":"recovered"`) {
		t.Fatalf("response = %s", recorder.Body.String())
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/auth/status", nil)
	statusReq.Header.Set("Authorization", "Bearer client-secret")
	statusRec := httptest.NewRecorder()
	h.HandleAuthStatus(statusRec, statusReq)
	if strings.Contains(statusRec.Body.String(), "access-new") || strings.Contains(statusRec.Body.String(), "refresh-new") {
		t.Fatalf("auth status leaked credentials: %s", statusRec.Body.String())
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
		AuthMode:           "token",
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
	return NewAPIHandler(cfg, trae.NewClient(cfg), traeauth.NewManager(cfg))
}

func TestLocalBrowserRequestAllowsLoopbackAndDockerBridge(t *testing.T) {
	loopback := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8000/auth/login", nil)
	loopback.RemoteAddr = "127.0.0.1:43210"
	if !isLocalBrowserRequest(loopback) {
		t.Fatal("loopback request should be allowed")
	}

	dockerBridge := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8000/auth/login", nil)
	dockerBridge.RemoteAddr = "172.17.0.1:43210"
	if !isLocalBrowserRequest(dockerBridge) {
		t.Fatal("private Docker bridge request to loopback host should be allowed")
	}
}

func TestLocalBrowserRequestRejectsPublicHostSpoof(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8000/auth/login", nil)
	request.RemoteAddr = "203.0.113.42:43210"
	if isLocalBrowserRequest(request) {
		t.Fatal("public peer with spoofed loopback Host must be rejected")
	}
}
