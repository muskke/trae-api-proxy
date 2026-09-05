package trae

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"
)

type ModelAvailability string

const (
	ModelUnknown     ModelAvailability = "unknown"
	ModelUsable      ModelAvailability = "usable"
	ModelUnavailable ModelAvailability = "unavailable"
)

type ModelStatus struct {
	ID               string            `json:"id"`
	DisplayName      string            `json:"display_name,omitempty"`
	OwnedBy          string            `json:"owned_by,omitempty"`
	ContextLength    int64             `json:"context_length,omitempty"`
	Function         string            `json:"function"`
	Status           ModelAvailability `json:"status"`
	LastErrorCode    string            `json:"last_error_code,omitempty"`
	LastErrorMessage string            `json:"last_error_message,omitempty"`
	CheckedAt        int64             `json:"checked_at,omitempty"`
	ExpiresAt        int64             `json:"expires_at,omitempty"`
}

type modelCapability struct {
	Status           ModelAvailability
	LastErrorCode    string
	LastErrorMessage string
	CheckedAt        time.Time
	ExpiresAt        time.Time
}

func sessionScope(session Session) string {
	identity := strings.TrimSpace(session.UID)
	if identity == "" && strings.TrimSpace(session.Token) != "" {
		sum := sha256.Sum256([]byte(session.Token))
		identity = fmt.Sprintf("token:%x", sum[:8])
	}
	if identity == "" {
		identity = "anonymous"
	}
	return strings.Join([]string{
		strings.ToLower(strings.TrimSpace(session.Platform)),
		strings.ToLower(strings.TrimRight(strings.TrimSpace(session.APIBaseURL), "/")),
		identity,
	}, "|")
}

func capabilityKey(session Session, model string) string {
	return sessionScope(session) + "\x00" + strings.ToLower(strings.TrimSpace(model))
}

func (c *Client) FunctionFor(session Session, model string) string {
	return currentFunction(session, model)
}

func (c *Client) ModelAvailability(session Session, model string) ModelStatus {
	canonical := c.canonicalModel(session, model)
	status := ModelStatus{
		ID:       canonical,
		Function: currentFunction(session, canonical),
		Status:   ModelUnknown,
	}
	if canonical == "" {
		return status
	}

	entry, ok := c.capabilityEntry(session, canonical)
	if !ok {
		return status
	}
	status.Status = entry.Status
	status.LastErrorCode = entry.LastErrorCode
	status.LastErrorMessage = entry.LastErrorMessage
	status.CheckedAt = entry.CheckedAt.Unix()
	status.ExpiresAt = entry.ExpiresAt.Unix()
	return status
}

func (c *Client) ModelStatuses(ctx context.Context, session Session) ([]ModelStatus, error) {
	models, err := c.ListModels(ctx, session)
	if err != nil {
		return nil, err
	}
	out := make([]ModelStatus, 0, len(models))
	for _, model := range models {
		status := c.ModelAvailability(session, model.ID)
		status.ID = model.ID
		status.DisplayName = model.DisplayName
		status.OwnedBy = model.OwnedBy
		status.ContextLength = model.ContextLength
		out = append(out, status)
	}
	return out, nil
}

func (c *Client) FilterModels(session Session, models []Model, availability string) ([]Model, error) {
	availability = strings.ToLower(strings.TrimSpace(availability))
	if availability == "" {
		availability = "available"
	}
	if availability != "available" && availability != "all" && availability != "usable" && availability != "unknown" && availability != "unavailable" {
		return nil, fmt.Errorf("availability must be available, all, usable, unknown, or unavailable")
	}

	out := make([]Model, 0, len(models))
	for _, model := range models {
		status := c.ModelAvailability(session, model.ID).Status
		include := false
		switch availability {
		case "all":
			include = true
		case "available":
			include = status != ModelUnavailable
		case "usable":
			include = status == ModelUsable
		case "unknown":
			include = status == ModelUnknown
		case "unavailable":
			include = status == ModelUnavailable
		}
		if include {
			out = append(out, model)
		}
	}
	return out, nil
}

func (c *Client) RecordModelSuccess(session Session, model string) {
	model = c.canonicalModel(session, model)
	if strings.TrimSpace(model) == "" {
		return
	}
	now := time.Now()
	c.capabilityMu.Lock()
	defer c.capabilityMu.Unlock()
	if c.modelCapabilities == nil {
		c.modelCapabilities = make(map[string]modelCapability)
	}
	c.modelCapabilities[capabilityKey(session, model)] = modelCapability{
		Status:    ModelUsable,
		CheckedAt: now,
		ExpiresAt: now.Add(c.Config.ModelSuccessTTL),
	}
}

func (c *Client) RecordModelFailure(session Session, model, code, message string, definitive bool) {
	model = c.canonicalModel(session, model)
	if strings.TrimSpace(model) == "" {
		return
	}
	now := time.Now()
	message = strings.TrimSpace(message)
	if len(message) > 256 {
		message = message[:256] + "..."
	}
	key := capabilityKey(session, model)

	c.capabilityMu.Lock()
	defer c.capabilityMu.Unlock()
	if c.modelCapabilities == nil {
		c.modelCapabilities = make(map[string]modelCapability)
	}
	previous := c.modelCapabilities[key]
	status := ModelUnknown
	ttl := c.Config.ModelFailureTTL
	if definitive {
		status = ModelUnavailable
	} else if previous.Status == ModelUsable && previous.ExpiresAt.After(now) {
		status = ModelUsable
		ttl = time.Until(previous.ExpiresAt)
		if ttl <= 0 {
			ttl = c.Config.ModelFailureTTL
		}
	}
	c.modelCapabilities[key] = modelCapability{
		Status:           status,
		LastErrorCode:    strings.TrimSpace(code),
		LastErrorMessage: message,
		CheckedAt:        now,
		ExpiresAt:        now.Add(ttl),
	}
}

func (c *Client) InvalidateModelCapabilities() {
	c.capabilityMu.Lock()
	c.modelCapabilities = make(map[string]modelCapability)
	c.capabilityMu.Unlock()
}

func (c *Client) ResetModelAvailability(session Session, model string) int {
	if strings.TrimSpace(model) != "" {
		model = c.canonicalModel(session, model)
	}

	c.capabilityMu.Lock()
	defer c.capabilityMu.Unlock()
	if len(c.modelCapabilities) == 0 {
		return 0
	}

	if strings.TrimSpace(model) != "" {
		key := capabilityKey(session, model)
		if _, ok := c.modelCapabilities[key]; ok {
			delete(c.modelCapabilities, key)
			return 1
		}
		return 0
	}

	prefix := sessionScope(session) + "\x00"
	removed := 0
	for key := range c.modelCapabilities {
		if strings.HasPrefix(key, prefix) {
			delete(c.modelCapabilities, key)
			removed++
		}
	}
	return removed
}

func (c *Client) capabilityEntry(session Session, model string) (modelCapability, bool) {
	key := capabilityKey(session, model)
	now := time.Now()
	c.capabilityMu.RLock()
	entry, ok := c.modelCapabilities[key]
	c.capabilityMu.RUnlock()
	if !ok {
		return modelCapability{}, false
	}
	if !entry.ExpiresAt.IsZero() && !entry.ExpiresAt.After(now) {
		c.capabilityMu.Lock()
		if current, exists := c.modelCapabilities[key]; exists && current.ExpiresAt.Equal(entry.ExpiresAt) {
			delete(c.modelCapabilities, key)
		}
		c.capabilityMu.Unlock()
		return modelCapability{}, false
	}
	return entry, true
}
