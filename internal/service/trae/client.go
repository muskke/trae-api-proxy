package trae

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/muskke/trae-api-proxy/internal/config"
)

const (
	soloChatPath   = "/api/agent/v3/llm_utils_chat"
	soloModelsPath = "/api/ide/v1/get_detail_param"
	legacyChatPath = "/api/ide/v1/chat"
	legacyModels   = "/api/ide/v1/model_list?type=llm_raw_chat"
	soloFunction   = "solo_work_lite"
)

type Session struct {
	Token      string
	UID        string
	MachineID  string
	DeviceID   string
	APIBaseURL string
	Platform   string
	Source     string
}

type Model struct {
	ID            string `json:"id"`
	Object        string `json:"object"`
	Created       int64  `json:"created"`
	OwnedBy       string `json:"owned_by"`
	DisplayName   string `json:"display_name,omitempty"`
	ContextLength int64  `json:"context_length,omitempty"`
}

type UpstreamError struct {
	Mode   string
	Status int
	Body   string
}

func (e *UpstreamError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 512 {
		body = body[:512] + "..."
	}
	if body == "" {
		body = http.StatusText(e.Status)
	}
	return fmt.Sprintf("upstream %s returned HTTP %d: %s", e.Mode, e.Status, body)
}

type Client struct {
	Config       *config.Config
	HTTPClient   *http.Client
	StreamClient *http.Client

	modelMu       sync.RWMutex
	modelCache    []Model
	modelCachedAt time.Time
}

func NewClient(cfg *config.Config) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyFromEnvironment
	transport.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext
	transport.ForceAttemptHTTP2 = true
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 20
	transport.IdleConnTimeout = 90 * time.Second
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.ExpectContinueTimeout = time.Second
	transport.ResponseHeaderTimeout = cfg.HeaderTimeout

	return &Client{
		Config: cfg,
		HTTPClient: &http.Client{
			Timeout:   cfg.RequestTimeout,
			Transport: transport,
		},
		StreamClient: &http.Client{Transport: transport},
	}
}

func (c *Client) CloseIdleConnections() {
	c.HTTPClient.CloseIdleConnections()
	c.StreamClient.CloseIdleConnections()
}

func (c *Client) headers(session Session, accept string) http.Header {
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	if accept != "" {
		h.Set("Accept", accept)
	}
	if c.Config.IDEVersion != "" {
		h.Set("User-Agent", "Trae/"+c.Config.IDEVersion)
		h.Set("X-Ide-Version", c.Config.IDEVersion)
	}
	if session.Token != "" {
		h.Set("Authorization", "Cloud-IDE-JWT "+session.Token)
		h.Set("X-Cloudide-Token", session.Token)
		h.Set("X-Ide-Token", session.Token)
	}
	setIfNotEmpty(h, "X-Uid", firstNonEmpty(session.UID, c.Config.UID))
	setIfNotEmpty(h, "X-App-Id", c.Config.AppID)
	setIfNotEmpty(h, "X-App-Version", c.Config.AppVersion)
	setIfNotEmpty(h, "X-App-Version-Code", c.Config.IDEVersionCode)
	setIfNotEmpty(h, "X-Ide-Version-Code", c.Config.IDEVersionCode)
	setIfNotEmpty(h, "X-Ide-Version-Type", c.Config.IDEVersionType)
	setIfNotEmpty(h, "X-Device-Brand", c.Config.DeviceBrand)
	setIfNotEmpty(h, "X-Device-Cpu", c.Config.DeviceCPU)
	setIfNotEmpty(h, "X-Device-Id", firstNonEmpty(session.DeviceID, c.Config.DeviceID))
	setIfNotEmpty(h, "X-Device-Type", c.Config.DeviceType)
	setIfNotEmpty(h, "X-Machine-Id", firstNonEmpty(session.MachineID, c.Config.MachineID))
	setIfNotEmpty(h, "X-OS-Version", c.Config.OSVersion)
	setIfNotEmpty(h, "Request-Traffic-Type", c.Config.RequestTrafficType)
	return h
}

func (c *Client) baseURL(session Session) string {
	if strings.TrimSpace(session.APIBaseURL) != "" {
		return strings.TrimRight(session.APIBaseURL, "/")
	}
	return strings.TrimRight(c.Config.APIBaseURL, "/")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func setIfNotEmpty(h http.Header, key, value string) {
	if strings.TrimSpace(value) != "" {
		h.Set(key, value)
	}
}

func (c *Client) ListModels(ctx context.Context, session Session) ([]Model, error) {
	c.modelMu.RLock()
	if len(c.modelCache) > 0 && time.Since(c.modelCachedAt) < c.Config.ModelCacheTTL {
		cached := append([]Model(nil), c.modelCache...)
		c.modelMu.RUnlock()
		return cached, nil
	}
	stale := append([]Model(nil), c.modelCache...)
	c.modelMu.RUnlock()

	models, err := c.fetchModels(ctx, session)
	if err != nil {
		if len(stale) > 0 && !IsUpstreamAuthError(err) {
			return stale, nil
		}
		return nil, err
	}

	c.modelMu.Lock()
	c.modelCache = append([]Model(nil), models...)
	c.modelCachedAt = time.Now()
	c.modelMu.Unlock()
	return models, nil
}

func (c *Client) fetchModels(ctx context.Context, session Session) ([]Model, error) {
	switch c.Config.UpstreamMode {
	case "solo":
		return c.fetchModelsMode(ctx, session, "solo")
	case "legacy":
		return c.fetchModelsMode(ctx, session, "legacy")
	default:
		models, err := c.fetchModelsMode(ctx, session, "solo")
		if err == nil || !canProtocolFallback(err) {
			return models, err
		}
		return c.fetchModelsMode(ctx, session, "legacy")
	}
}

func (c *Client) fetchModelsMode(ctx context.Context, session Session, mode string) ([]Model, error) {
	var req *http.Request
	var err error

	if mode == "solo" {
		payload := map[string]any{
			"function":            soloFunction,
			"config_names":        nil,
			"need_prompt":         false,
			"current_config_info": nil,
			"poly_prompt":         true,
			"mode_type":           nil,
			"agent_type":          nil,
		}
		body, _ := json.Marshal(payload)
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL(session)+soloModelsPath, bytes.NewReader(body))
	} else {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL(session)+legacyModels, nil)
	}
	if err != nil {
		return nil, err
	}
	req.Header = c.headers(session, "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, &UpstreamError{Mode: mode, Status: resp.StatusCode, Body: string(body)}
	}

	if mode == "solo" {
		var raw struct {
			ConfigInfoList []struct {
				ConfigName     string `json:"config_name"`
				MaxInputTokens int64  `json:"max_input_tokens"`
				DisplayConfig  struct {
					DisplayName string `json:"display_name"`
				} `json:"display_config"`
			} `json:"config_info_list"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			return nil, fmt.Errorf("decode solo model list: %w", err)
		}
		models := make([]Model, 0, len(raw.ConfigInfoList))
		for _, item := range raw.ConfigInfoList {
			if strings.TrimSpace(item.ConfigName) == "" {
				continue
			}
			models = append(models, Model{
				ID:            item.ConfigName,
				Object:        "model",
				OwnedBy:       "trae-solo",
				DisplayName:   item.DisplayConfig.DisplayName,
				ContextLength: item.MaxInputTokens,
			})
		}
		if len(models) == 0 {
			return nil, fmt.Errorf("solo model list was empty")
		}
		return models, nil
	}

	var raw struct {
		ModelConfigs []struct {
			Name string `json:"name"`
		} `json:"model_configs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode legacy model list: %w", err)
	}
	models := make([]Model, 0, len(raw.ModelConfigs))
	for _, item := range raw.ModelConfigs {
		if strings.TrimSpace(item.Name) == "" {
			continue
		}
		models = append(models, Model{ID: item.Name, Object: "model", OwnedBy: "trae"})
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("legacy model list was empty")
	}
	return models, nil
}

// ChatCompletion sends an OpenAI-compatible request to the selected upstream
// protocol. The upstream is always asked for SSE; non-streaming client requests
// are aggregated by the handler so both modes share the same parser.
func (c *Client) ChatCompletion(ctx context.Context, session Session, model string, rawBody []byte) (*http.Response, string, error) {
	switch c.Config.UpstreamMode {
	case "solo":
		resp, err := c.chatMode(ctx, session, model, rawBody, "solo")
		return resp, "solo", err
	case "legacy":
		resp, err := c.chatMode(ctx, session, model, rawBody, "legacy")
		return resp, "legacy", err
	default:
		resp, err := c.chatMode(ctx, session, model, rawBody, "solo")
		if err == nil || !canProtocolFallback(err) {
			return resp, "solo", err
		}
		resp, err = c.chatMode(ctx, session, model, rawBody, "legacy")
		return resp, "legacy", err
	}
}

func (c *Client) chatMode(ctx context.Context, session Session, model string, rawBody []byte, mode string) (*http.Response, error) {
	var body []byte
	var err error
	path := soloChatPath
	if mode == "solo" {
		body, err = prepareSoloBody(rawBody, model)
	} else {
		path = legacyChatPath
		body, err = prepareLegacyBody(rawBody, model, c.Config.Locale)
	}
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL(session)+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = c.headers(session, "text/event-stream")

	resp, err := c.StreamClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, &UpstreamError{Mode: mode, Status: resp.StatusCode, Body: string(raw)}
	}
	return resp, nil
}

func (c *Client) InvalidateModelCache() {
	c.modelMu.Lock()
	c.modelCache = nil
	c.modelCachedAt = time.Time{}
	c.modelMu.Unlock()
}

func IsUpstreamAuthError(err error) bool {
	upstreamErr, ok := err.(*UpstreamError)
	if !ok {
		return false
	}
	return upstreamErr.Status == http.StatusUnauthorized || upstreamErr.Status == http.StatusForbidden
}

func canProtocolFallback(err error) bool {
	upstreamErr, ok := err.(*UpstreamError)
	if !ok {
		return false
	}
	return upstreamErr.Status == http.StatusNotFound || upstreamErr.Status == http.StatusMethodNotAllowed
}
