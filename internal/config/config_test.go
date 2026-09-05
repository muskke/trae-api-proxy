package config

import (
	"testing"
	"time"
)

func TestParseAliases(t *testing.T) {
	aliases := parseAliases("fast=glm-5.2; DS = DeepSeek-V4-Pro,broken,empty=")
	if got := aliases["fast"]; got != "glm-5.2" {
		t.Fatalf("fast alias = %q", got)
	}
	if got := aliases["ds"]; got != "DeepSeek-V4-Pro" {
		t.Fatalf("case-insensitive alias = %q", got)
	}
	if _, ok := aliases["broken"]; ok {
		t.Fatal("malformed alias should be ignored")
	}
}

func TestValidateRequiresIdeTokenForClientAuth(t *testing.T) {
	cfg := validConfig()
	cfg.AuthToken = "client-secret"
	cfg.IdeToken = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestValidateAcceptsSupportedModes(t *testing.T) {
	for _, mode := range []string{"auto", "solo", "legacy"} {
		cfg := validConfig()
		cfg.UpstreamMode = mode
		if err := cfg.Validate(); err != nil {
			t.Fatalf("mode %s: %v", mode, err)
		}
	}
}

func validConfig() *Config {
	return &Config{
		APIBaseURL:      "https://example.com",
		Bind:            "127.0.0.1",
		Port:            "8000",
		UpstreamMode:    "auto",
		RequestTimeout:  time.Second,
		HeaderTimeout:   time.Second,
		ShutdownTimeout: time.Second,
		ModelCacheTTL:   time.Minute,
		MaxBodyBytes:    1024,
	}
}
