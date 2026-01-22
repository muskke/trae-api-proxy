package trae

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/muskke/trae-api-proxy/internal/config"

	"github.com/google/uuid"
	"github.com/openai/openai-go"
)

type Client struct {
	Config     *config.Config
	HTTPClient *http.Client
}

func NewClient(cfg *config.Config) *Client {
	return &Client{
		Config: cfg,
		HTTPClient: &http.Client{
			Timeout: 0, // No timeout for streaming, handle via context
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   100,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		},
	}
}

func (c *Client) headers(ideToken string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("x-app-id", c.Config.AppID)
	h.Set("x-device-brand", c.Config.DeviceBrand)
	h.Set("x-device-cpu", c.Config.DeviceCPU)
	h.Set("x-device-id", c.Config.DeviceID)
	h.Set("x-device-type", c.Config.DeviceType)
	h.Set("x-ide-token", ideToken)
	h.Set("x-ide-version", c.Config.IDEVersion)
	h.Set("x-ide-version-code", c.Config.IDEVersionCode)
	h.Set("x-ide-version-type", c.Config.IDEVersionType)
	h.Set("x-machine-id", c.Config.MachineID)
	h.Set("x-os-version", c.Config.OSVersion)
	return h
}

func (c *Client) ListModels(ctx context.Context, ideToken string) ([]openai.Model, error) {
	req, _ := http.NewRequestWithContext(
		ctx,
		"GET",
		c.Config.APIBaseURL+"/api/ide/v1/model_list?type=llm_raw_chat",
		nil,
	)
	req.Header = c.headers(ideToken)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream status: %s", resp.Status)
	}

	var raw struct {
		ModelConfigs []struct {
			Name string `json:"name"`
		} `json:"model_configs"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}

	models := make([]openai.Model, 0)
	for _, m := range raw.ModelConfigs {
		models = append(models, openai.Model{
			ID:      m.Name,
			Object:  "model",
			Created: 0,
			OwnedBy: "trae",
		})
	}
	return models, nil
}

func (c *Client) ChatCompletion(ctx context.Context, ideToken string, model string, messages []openai.ChatCompletionMessage, stream bool) (*http.Response, error) {
	currentTurn := 0
	for i := 0; i < len(messages)-1; i++ {
		if messages[i].Role == "user" {
			currentTurn++
		}
	}

	if len(messages) == 0 {
		return nil, fmt.Errorf("no messages")
	}
	lastMsg := messages[len(messages)-1]

	history := []any{}
	for _, m := range messages[:len(messages)-1] {
		history = append(history, map[string]any{
			"role":    m.Role,
			"content": m.Content,
			"status":  "success",
			"locale":  c.Config.Locale,
		})
	}

	payload := map[string]any{
		"chat_history":                 history,
		"context_resolvers":            []any{},
		"conversation_id":              uuid.NewString(),
		"current_turn":                 currentTurn,
		"generate_suggested_questions": false,
		"intent_name":                  "general_qa_intent",
		"is_preset":                    true,
		"model_name":                   model,
		"session_id":                   uuid.NewString(),
		"stream":                       stream,
		"user_input":                   lastMsg.Content,
		"valid_turns":                  []int{},
		"variables": fmt.Sprintf(
			`{"locale":"%s","current_time":"%s"}`,
			c.Config.Locale,
			time.Now().Format("20060102 15:04:05 Monday"),
		),
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	httpReq, _ := http.NewRequestWithContext(
		ctx,
		"POST",
		c.Config.APIBaseURL+"/api/ide/v1/chat",
		bytes.NewReader(body),
	)
	httpReq.Header = c.headers(ideToken)

	return c.HTTPClient.Do(httpReq)
}
