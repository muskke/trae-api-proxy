package trae

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const modelStatusFileVersion = 1

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

type persistedModelCapability struct {
	Status           ModelAvailability `json:"status"`
	LastErrorCode    string            `json:"last_error_code,omitempty"`
	LastErrorMessage string            `json:"last_error_message,omitempty"`
	CheckedAt        int64             `json:"checked_at"`
	ExpiresAt        int64             `json:"expires_at"`
}

type persistedModelStatus struct {
	Version int                                 `json:"version"`
	Entries map[string]persistedModelCapability `json:"entries"`
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
	if c.modelCapabilities == nil {
		c.modelCapabilities = make(map[string]modelCapability)
	}
	c.modelCapabilities[capabilityKey(session, model)] = modelCapability{
		Status:    ModelUsable,
		CheckedAt: now,
		ExpiresAt: now.Add(c.Config.ModelSuccessTTL),
	}
	c.capabilityMu.Unlock()
	c.persistModelCapabilities()
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
	c.capabilityMu.Unlock()
	c.persistModelCapabilities()
}

func (c *Client) InvalidateModelCapabilities() {
	c.capabilityMu.Lock()
	c.modelCapabilities = make(map[string]modelCapability)
	c.capabilityMu.Unlock()
	c.persistModelCapabilities()
}

func (c *Client) ResetModelAvailability(session Session, model string) int {
	if strings.TrimSpace(model) != "" {
		model = c.canonicalModel(session, model)
	}

	c.capabilityMu.Lock()
	if len(c.modelCapabilities) == 0 {
		c.capabilityMu.Unlock()
		return 0
	}

	if strings.TrimSpace(model) != "" {
		key := capabilityKey(session, model)
		if _, ok := c.modelCapabilities[key]; ok {
			delete(c.modelCapabilities, key)
			c.capabilityMu.Unlock()
			c.persistModelCapabilities()
			return 1
		}
		c.capabilityMu.Unlock()
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
	c.capabilityMu.Unlock()
	if removed > 0 {
		c.persistModelCapabilities()
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
		removed := false
		c.capabilityMu.Lock()
		if current, exists := c.modelCapabilities[key]; exists && current.ExpiresAt.Equal(entry.ExpiresAt) {
			delete(c.modelCapabilities, key)
			removed = true
		}
		c.capabilityMu.Unlock()
		if removed {
			c.persistModelCapabilities()
		}
		return modelCapability{}, false
	}
	return entry, true
}

func (c *Client) loadModelCapabilities() {
	path := strings.TrimSpace(c.Config.ModelStatusFile)
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("model status cache load failed: %v", err)
		}
		return
	}
	var stored persistedModelStatus
	if err := json.Unmarshal(raw, &stored); err != nil {
		log.Printf("model status cache decode failed: %v", err)
		return
	}
	if stored.Version != modelStatusFileVersion {
		log.Printf("model status cache version %d is unsupported; starting fresh", stored.Version)
		return
	}
	now := time.Now()
	loaded := make(map[string]modelCapability, len(stored.Entries))
	for key, entry := range stored.Entries {
		if entry.Status != ModelUsable && entry.Status != ModelUnavailable && entry.Status != ModelUnknown {
			continue
		}
		expiresAt := time.Unix(entry.ExpiresAt, 0)
		if entry.ExpiresAt <= 0 || !expiresAt.After(now) {
			continue
		}
		loaded[key] = modelCapability{
			Status:           entry.Status,
			LastErrorCode:    entry.LastErrorCode,
			LastErrorMessage: entry.LastErrorMessage,
			CheckedAt:        time.Unix(entry.CheckedAt, 0),
			ExpiresAt:        expiresAt,
		}
	}
	c.capabilityMu.Lock()
	c.modelCapabilities = loaded
	c.capabilityMu.Unlock()
	if len(loaded) > 0 {
		log.Printf("loaded %d model availability entries from %s", len(loaded), path)
	}
}

func (c *Client) persistModelCapabilities() {
	path := strings.TrimSpace(c.Config.ModelStatusFile)
	if path == "" {
		return
	}
	c.capabilityPersist.Lock()
	defer c.capabilityPersist.Unlock()
	now := time.Now()
	c.capabilityMu.RLock()
	entries := make(map[string]persistedModelCapability, len(c.modelCapabilities))
	for key, entry := range c.modelCapabilities {
		if !entry.ExpiresAt.IsZero() && !entry.ExpiresAt.After(now) {
			continue
		}
		entries[key] = persistedModelCapability{
			Status:           entry.Status,
			LastErrorCode:    entry.LastErrorCode,
			LastErrorMessage: entry.LastErrorMessage,
			CheckedAt:        entry.CheckedAt.Unix(),
			ExpiresAt:        entry.ExpiresAt.Unix(),
		}
	}
	c.capabilityMu.RUnlock()

	stored := persistedModelStatus{Version: modelStatusFileVersion, Entries: entries}
	raw, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		log.Printf("model status cache encode failed: %v", err)
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("model status cache mkdir failed: %v", err)
		return
	}
	tmp, err := os.CreateTemp(dir, ".model-status-*.tmp")
	if err != nil {
		log.Printf("model status cache temp file failed: %v", err)
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		log.Printf("model status cache chmod failed: %v", err)
		return
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		log.Printf("model status cache write failed: %v", err)
		return
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		log.Printf("model status cache sync failed: %v", err)
		return
	}
	if err := tmp.Close(); err != nil {
		log.Printf("model status cache close failed: %v", err)
		return
	}
	if err := os.Rename(tmpName, path); err == nil {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("model status cache replace failed: %v", err)
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		log.Printf("model status cache rename failed: %v", err)
	}
}
