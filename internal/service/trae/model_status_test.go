package trae

import (
	"testing"
	"time"

	"github.com/muskke/trae-api-proxy/internal/config"
)

func TestModelAvailabilityLifecycleAndFiltering(t *testing.T) {
	cfg := &config.Config{
		ModelFailureTTL: time.Minute,
		ModelSuccessTTL: time.Hour,
	}
	client := NewClient(cfg)
	session := Session{UID: "u1", Platform: "global", APIBaseURL: "https://example.test"}

	client.modelMu.Lock()
	client.modelCache[sessionScope(session)] = modelCacheEntry{
		Models: []Model{
			{ID: "minimax-m3", Object: "model", OwnedBy: "trae"},
			{ID: "DeepSeek-V4-Flash", Object: "model", OwnedBy: "trae"},
		},
		CachedAt: time.Now(),
	}
	client.modelMu.Unlock()

	if got := client.ModelAvailability(session, "minimax-m3").Status; got != ModelUnknown {
		t.Fatalf("initial status = %q", got)
	}

	client.RecordModelSuccess(session, "minimax-m3")
	if got := client.ModelAvailability(session, "MINIMAX-M3").Status; got != ModelUsable {
		t.Fatalf("success status = %q", got)
	}

	client.RecordModelFailure(session, "deepseek-v4-flash", "4001", "param invalid", true)
	status := client.ModelAvailability(session, "DeepSeek-V4-Flash")
	if status.Status != ModelUnavailable || status.LastErrorCode != "4001" {
		t.Fatalf("failure status = %+v", status)
	}

	all := []Model{{ID: "minimax-m3"}, {ID: "DeepSeek-V4-Flash"}}
	available, err := client.FilterModels(session, all, "available")
	if err != nil {
		t.Fatal(err)
	}
	if len(available) != 1 || available[0].ID != "minimax-m3" {
		t.Fatalf("available models = %#v", available)
	}
	usable, err := client.FilterModels(session, all, "usable")
	if err != nil {
		t.Fatal(err)
	}
	if len(usable) != 1 || usable[0].ID != "minimax-m3" {
		t.Fatalf("usable models = %#v", usable)
	}
	unavailable, err := client.FilterModels(session, all, "unavailable")
	if err != nil {
		t.Fatal(err)
	}
	if len(unavailable) != 1 || unavailable[0].ID != "DeepSeek-V4-Flash" {
		t.Fatalf("unavailable models = %#v", unavailable)
	}

	if removed := client.ResetModelAvailability(session, "deepseek-v4-flash"); removed != 1 {
		t.Fatalf("removed = %d", removed)
	}
	if got := client.ModelAvailability(session, "DeepSeek-V4-Flash").Status; got != ModelUnknown {
		t.Fatalf("status after reset = %q", got)
	}
}

func TestTransientFailureDoesNotDemoteKnownUsableModel(t *testing.T) {
	cfg := &config.Config{ModelFailureTTL: time.Minute, ModelSuccessTTL: time.Hour}
	client := NewClient(cfg)
	session := Session{UID: "u1", Platform: "global"}

	client.RecordModelSuccess(session, "minimax-m3")
	client.RecordModelFailure(session, "minimax-m3", "4001", "bad optional parameter", false)
	status := client.ModelAvailability(session, "minimax-m3")
	if status.Status != ModelUsable {
		t.Fatalf("status = %+v", status)
	}
	if status.LastErrorCode != "4001" {
		t.Fatalf("last error code = %q", status.LastErrorCode)
	}
}

func TestModelAvailabilityIsScopedByAccount(t *testing.T) {
	cfg := &config.Config{ModelFailureTTL: time.Minute, ModelSuccessTTL: time.Hour}
	client := NewClient(cfg)
	a := Session{UID: "account-a", Platform: "global", APIBaseURL: "https://example.test"}
	b := Session{UID: "account-b", Platform: "global", APIBaseURL: "https://example.test"}

	client.RecordModelFailure(a, "model-a", "4001", "param invalid", true)
	if got := client.ModelAvailability(a, "model-a").Status; got != ModelUnavailable {
		t.Fatalf("account A status = %q", got)
	}
	if got := client.ModelAvailability(b, "model-a").Status; got != ModelUnknown {
		t.Fatalf("account B inherited status = %q", got)
	}
}

func TestExpiredAvailabilityReturnsToUnknown(t *testing.T) {
	cfg := &config.Config{ModelFailureTTL: 5 * time.Millisecond, ModelSuccessTTL: time.Hour}
	client := NewClient(cfg)
	session := Session{UID: "u1", Platform: "global"}
	client.RecordModelFailure(session, "model-a", "4001", "param invalid", true)
	time.Sleep(15 * time.Millisecond)
	if got := client.ModelAvailability(session, "model-a").Status; got != ModelUnknown {
		t.Fatalf("expired status = %q", got)
	}
}

func TestModelAvailabilityPersistsAcrossClientRestart(t *testing.T) {
	path := t.TempDir() + "/model-status.json"
	cfg := &config.Config{
		ModelFailureTTL: time.Hour,
		ModelSuccessTTL: time.Hour,
		ModelStatusFile: path,
	}
	session := Session{UID: "persisted-user", Platform: "global", APIBaseURL: "https://example.test"}

	first := NewClient(cfg)
	first.RecordModelSuccess(session, "minimax-m3")
	first.RecordModelFailure(session, "deepseek-v4-flash", "4001", "param invalid", true)

	second := NewClient(cfg)
	if got := second.ModelAvailability(session, "minimax-m3").Status; got != ModelUsable {
		t.Fatalf("persisted usable status = %q", got)
	}
	failure := second.ModelAvailability(session, "deepseek-v4-flash")
	if failure.Status != ModelUnavailable || failure.LastErrorCode != "4001" {
		t.Fatalf("persisted unavailable status = %+v", failure)
	}

	if removed := second.ResetModelAvailability(session, "deepseek-v4-flash"); removed != 1 {
		t.Fatalf("removed = %d", removed)
	}
	third := NewClient(cfg)
	if got := third.ModelAvailability(session, "deepseek-v4-flash").Status; got != ModelUnknown {
		t.Fatalf("reset should persist, status = %q", got)
	}
}
