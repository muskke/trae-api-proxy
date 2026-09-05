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
		ModelFailureTTL:    time.Minute,
		ModelSuccessTTL:    time.Hour,
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
		ModelFailureTTL:    time.Minute,
		ModelSuccessTTL:    time.Hour,
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

func TestMinimal4001MarksModelUnavailableAndFiltersCatalog(t *testing.T) {
	var chatCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ide/v1/get_detail_param":
			_ = json.NewEncoder(w).Encode(map[string]any{"config_info_list": []any{
				map[string]any{"config_name": "DeepSeek-V4-Flash"},
				map[string]any{"config_name": "minimax-m3"},
			}})
		case "/api/agent/v3/llm_utils_chat":
			chatCalls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: error\ndata: {\"code\":\"4001\",\"message\":\"We're sorry, the param is invalid. Please try with a valid param.\"}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	h := newTestHandler(upstream.URL)
	body := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hello"}],"stream":true,"stream_options":{"include_usage":true}}`

	first := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	first.Header.Set("Authorization", "Bearer client-secret")
	firstRec := httptest.NewRecorder()
	h.HandleChatCompletions(firstRec, first)
	if chatCalls.Load() != 1 {
		t.Fatalf("first upstream chat calls = %d", chatCalls.Load())
	}
	if !strings.Contains(firstRec.Body.String(), `"code":"4001"`) || !strings.Contains(firstRec.Body.String(), "data: [DONE]") {
		t.Fatalf("first stream = %s", firstRec.Body.String())
	}

	modelsReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	modelsReq.Header.Set("Authorization", "Bearer client-secret")
	modelsRec := httptest.NewRecorder()
	h.HandleModels(modelsRec, modelsReq)
	if modelsRec.Code != http.StatusOK {
		t.Fatalf("models status=%d body=%s", modelsRec.Code, modelsRec.Body.String())
	}
	if strings.Contains(modelsRec.Body.String(), "DeepSeek-V4-Flash") || !strings.Contains(modelsRec.Body.String(), "minimax-m3") {
		t.Fatalf("default filtered models = %s", modelsRec.Body.String())
	}

	unavailableReq := httptest.NewRequest(http.MethodGet, "/v1/models?availability=unavailable", nil)
	unavailableReq.Header.Set("Authorization", "Bearer client-secret")
	unavailableRec := httptest.NewRecorder()
	h.HandleModels(unavailableRec, unavailableReq)
	if !strings.Contains(unavailableRec.Body.String(), "DeepSeek-V4-Flash") || strings.Contains(unavailableRec.Body.String(), "minimax-m3") {
		t.Fatalf("unavailable models = %s", unavailableRec.Body.String())
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/v1/models/status", nil)
	statusReq.Header.Set("Authorization", "Bearer client-secret")
	statusRec := httptest.NewRecorder()
	h.HandleModelStatus(statusRec, statusReq)
	if !strings.Contains(statusRec.Body.String(), `"status":"unavailable"`) || !strings.Contains(statusRec.Body.String(), `"last_error_code":"4001"`) {
		t.Fatalf("model status = %s", statusRec.Body.String())
	}

	second := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	second.Header.Set("Authorization", "Bearer client-secret")
	secondRec := httptest.NewRecorder()
	h.HandleChatCompletions(secondRec, second)
	if secondRec.Code != http.StatusBadRequest || !strings.Contains(secondRec.Body.String(), `"code":"model_unavailable"`) {
		t.Fatalf("second status=%d body=%s", secondRec.Code, secondRec.Body.String())
	}
	if chatCalls.Load() != 1 {
		t.Fatalf("negative cache should prevent repeat upstream call; calls=%d", chatCalls.Load())
	}

	resetReq := httptest.NewRequest(http.MethodDelete, "/v1/models/status?model=deepseek-v4-flash", nil)
	resetReq.Header.Set("Authorization", "Bearer client-secret")
	resetRec := httptest.NewRecorder()
	h.HandleModelStatusReset(resetRec, resetReq)
	if resetRec.Code != http.StatusOK || !strings.Contains(resetRec.Body.String(), `"removed":1`) {
		t.Fatalf("reset status=%d body=%s", resetRec.Code, resetRec.Body.String())
	}

	third := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	third.Header.Set("Authorization", "Bearer client-secret")
	thirdRec := httptest.NewRecorder()
	h.HandleChatCompletions(thirdRec, third)
	if chatCalls.Load() != 2 {
		t.Fatalf("reset should allow recheck; calls=%d", chatCalls.Load())
	}
}

func TestComplex4001DoesNotBlacklistModel(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/v3/llm_utils_chat" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: error\ndata: {\"code\":\"4001\",\"message\":\"param is invalid; try with a valid param\"}\n\n")
	}))
	defer upstream.Close()

	h := newTestHandler(upstream.URL)
	body := `{"model":"minimax-m3","messages":[{"role":"user","content":"hello"}],"temperature":9,"stream":true}`
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer client-secret")
		rec := httptest.NewRecorder()
		h.HandleChatCompletions(rec, req)
	}
	if calls.Load() != 2 {
		t.Fatalf("complex request failure must not blacklist model; calls=%d", calls.Load())
	}
	if got := h.Client.ModelAvailability(trae.Session{Token: "upstream-secret", Source: "token"}, "minimax-m3").Status; got == trae.ModelUnavailable {
		t.Fatalf("complex request blacklisted model: %s", got)
	}
}

func TestSuccessfulChatMarksModelUsable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ide/v1/get_detail_param":
			_ = json.NewEncoder(w).Encode(map[string]any{"config_info_list": []any{map[string]any{"config_name": "minimax-m3"}}})
		case "/api/agent/v3/llm_utils_chat":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: output\ndata: {\"response\":\"ok\"}\n\nevent: done\ndata: {}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	h := newTestHandler(upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"minimax-m3","messages":[{"role":"user","content":"hello"}],"stream":false}`))
	req.Header.Set("Authorization", "Bearer client-secret")
	rec := httptest.NewRecorder()
	h.HandleChatCompletions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status=%d body=%s", rec.Code, rec.Body.String())
	}

	modelsReq := httptest.NewRequest(http.MethodGet, "/v1/models?availability=usable", nil)
	modelsReq.Header.Set("Authorization", "Bearer client-secret")
	modelsRec := httptest.NewRecorder()
	h.HandleModels(modelsRec, modelsReq)
	if modelsRec.Code != http.StatusOK || !strings.Contains(modelsRec.Body.String(), "minimax-m3") {
		t.Fatalf("usable models status=%d body=%s", modelsRec.Code, modelsRec.Body.String())
	}
}

func TestProxyPreservesUTF8MessageContent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/v3/llm_utils_chat" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Messages []struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		if len(body.Messages) != 1 || len(body.Messages[0].Content) != 1 || body.Messages[0].Content[0].Text != "用三句话介绍 Go" {
			t.Fatalf("UTF-8 content changed: %#v", body.Messages)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: output\ndata: {\"response\":\"正常\"}\n\nevent: done\ndata: {}\n\n")
	}))
	defer upstream.Close()

	h := newTestHandler(upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"minimax-m3","messages":[{"role":"user","content":"用三句话介绍 Go"}],"stream":false}`))
	req.Header.Set("Authorization", "Bearer client-secret")
	rec := httptest.NewRecorder()
	h.HandleChatCompletions(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "正常") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlerAppliesReasoningMode(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: progress_notice\ndata: \"warming up\"\n\nevent: output\ndata: {\"reasoning_content\":\"thought:\",\"response\":\"answer\"}\n\nevent: done\ndata: {}\n\n")
	}))
	defer upstream.Close()

	h := newTestHandler(upstream.URL)
	h.Config.ReasoningMode = "merge"
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"minimax-m3","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("Authorization", "Bearer client-secret")
	rec := httptest.NewRecorder()
	h.HandleChatCompletions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"content":"thought:answer"`) || strings.Contains(rec.Body.String(), "reasoning_content") {
		t.Fatalf("response = %s", rec.Body.String())
	}
	if got := rec.Header().Get("X-Trae-Reasoning-Mode"); got != "merge" {
		t.Fatalf("reasoning header = %q", got)
	}
}
