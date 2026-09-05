package trae

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muskke/trae-api-proxy/internal/config"
)

func TestChatCompletionAutoFallsBackToLegacy(t *testing.T) {
	var soloCalls atomic.Int32
	var legacyCalls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case soloChatPath:
			soloCalls.Add(1)
			if got := r.Header.Get("Authorization"); got != "Cloud-IDE-JWT upstream-token" {
				t.Errorf("authorization header = %q", got)
			}
			if got := r.Header.Get("X-Ide-Version"); got != config.DefaultIDEVersion {
				t.Errorf("ide version = %q", got)
			}
			http.Error(w, "not available", http.StatusNotFound)
		case legacyChatPath:
			legacyCalls.Add(1)
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode legacy body: %v", err)
			}
			if body["model_name"] != "glm-5.2" || body["user_input"] != "hello" {
				t.Errorf("legacy body = %#v", body)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: output\ndata: {\"response\":\"ok\"}\n\nevent: done\ndata: {}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := testClientConfig(server.URL)
	cfg.UpstreamMode = "auto"
	client := NewClient(cfg)
	defer client.CloseIdleConnections()

	body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	resp, mode, err := client.ChatCompletion(context.Background(), Session{Token: "upstream-token"}, "glm-5.2", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if mode != "legacy" {
		t.Fatalf("mode = %q", mode)
	}
	if soloCalls.Load() != 1 || legacyCalls.Load() != 1 {
		t.Fatalf("calls solo=%d legacy=%d", soloCalls.Load(), legacyCalls.Load())
	}
}

func TestListModelsAutoFallbackAndCache(t *testing.T) {
	var soloCalls atomic.Int32
	var legacyCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case soloModelsPath:
			soloCalls.Add(1)
			http.NotFound(w, r)
		case "/api/ide/v1/model_list":
			legacyCalls.Add(1)
			if r.URL.Query().Get("type") != "llm_raw_chat" {
				t.Errorf("legacy model query = %q", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"model_configs": []any{
				map[string]any{"name": "model-a"},
				map[string]any{"name": "model-b"},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := testClientConfig(server.URL)
	cfg.UpstreamMode = "auto"
	cfg.ModelCacheTTL = time.Hour
	client := NewClient(cfg)
	defer client.CloseIdleConnections()

	for i := 0; i < 2; i++ {
		models, err := client.ListModels(context.Background(), Session{Token: "token"})
		if err != nil {
			t.Fatal(err)
		}
		if len(models) != 2 || models[0].ID != "model-a" {
			t.Fatalf("models = %#v", models)
		}
	}
	if soloCalls.Load() != 1 || legacyCalls.Load() != 1 {
		t.Fatalf("model cache missed: solo=%d legacy=%d", soloCalls.Load(), legacyCalls.Load())
	}
}

func TestSoloModelResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != soloModelsPath || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("accept = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"config_info_list": []any{
			map[string]any{
				"config_name":    "glm-5.2",
				"display_config": map[string]any{"display_name": "GLM 5.2"},
			},
		}})
	}))
	defer server.Close()

	cfg := testClientConfig(server.URL)
	cfg.UpstreamMode = "solo"
	client := NewClient(cfg)
	defer client.CloseIdleConnections()

	models, err := client.ListModels(context.Background(), Session{Token: "token"})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "glm-5.2" || models[0].DisplayName != "GLM 5.2" {
		t.Fatalf("models = %#v", models)
	}
}

func TestCanProtocolFallbackOnlyForEndpointMismatch(t *testing.T) {
	if !canProtocolFallback(&UpstreamError{Status: http.StatusNotFound}) {
		t.Fatal("404 should allow protocol fallback")
	}
	if !canProtocolFallback(&UpstreamError{Status: http.StatusMethodNotAllowed}) {
		t.Fatal("405 should allow protocol fallback")
	}
	if canProtocolFallback(&UpstreamError{Status: http.StatusUnauthorized}) {
		t.Fatal("401 must not fall back")
	}
	if canProtocolFallback(context.DeadlineExceeded) {
		t.Fatal("transport errors must not fall back")
	}
}

func testClientConfig(baseURL string) *config.Config {
	return &config.Config{
		AppID:              config.DefaultAppID,
		AppVersion:         config.DefaultApplicationVer,
		DeviceBrand:        config.DefaultDeviceBrand,
		DeviceType:         config.DefaultDeviceType,
		IDEVersion:         config.DefaultIDEVersion,
		IDEVersionCode:     config.DefaultIDEVersionCode,
		IDEVersionType:     config.DefaultIDEVersionType,
		OSVersion:          config.DefaultOSVersion,
		APIBaseURL:         strings.TrimRight(baseURL, "/"),
		Locale:             "zh-cn",
		RequestTrafficType: config.DefaultRequestTraffic,
		RequestTimeout:     3 * time.Second,
		HeaderTimeout:      3 * time.Second,
		ModelCacheTTL:      time.Minute,
	}
}
