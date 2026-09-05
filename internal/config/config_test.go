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

func TestValidateRequiresIdeTokenInTokenMode(t *testing.T) {
	cfg := validConfig()
	cfg.AuthMode = "token"
	cfg.IdeToken = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestValidateAllowsOAuthWithClientAuthAndNoStaticToken(t *testing.T) {
	cfg := validConfig()
	cfg.AuthToken = "client-secret"
	cfg.AuthMode = "oauth"
	cfg.IdeToken = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("oauth config should start before first login: %v", err)
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

func TestValidateRejectsNonLocalOAuthCallbackBind(t *testing.T) {
	cfg := validConfig()
	cfg.OAuthCallbackBind = "192.0.2.10"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected callback bind validation error")
	}
}

func TestOAuthCallbackUsesDedicatedLoopbackListener(t *testing.T) {
	cfg := validConfig()
	cfg.OAuthCallbackBase = "http://127.0.0.1:18080"
	addr, ok := cfg.OAuthCallbackListenAddr()
	if !ok || addr != "127.0.0.1:18080" {
		t.Fatalf("callback addr=%q ok=%v", addr, ok)
	}
	cfg.OAuthCallbackBind = "0.0.0.0"
	addr, ok = cfg.OAuthCallbackListenAddr()
	if !ok || addr != "0.0.0.0:18080" {
		t.Fatalf("docker callback addr=%q ok=%v", addr, ok)
	}
	cfg.OAuthCallbackBind = ""
	if cfg.PrimaryServesOAuthCallback() {
		t.Fatal("port 18080 should require a dedicated callback listener when API is on 8000")
	}

	cfg.OAuthCallbackBase = "http://127.0.0.1:8000"
	if !cfg.PrimaryServesOAuthCallback() {
		t.Fatal("primary loopback listener should serve a callback on the same port")
	}
}

func validConfig() *Config {
	return &Config{
		AuthMode:          "auto",
		AuthFile:          DefaultAuthFile,
		APIBaseURL:        "https://example.com",
		OAuthHost:         DefaultOAuthHost,
		OAuthConsoleURL:   DefaultOAuthConsoleURL,
		OAuthCallbackBase: "http://127.0.0.1:8000",
		Bind:              "127.0.0.1",
		Port:              "8000",
		UpstreamMode:      "auto",
		RequestTimeout:    time.Second,
		HeaderTimeout:     time.Second,
		ShutdownTimeout:   time.Second,
		ModelCacheTTL:     time.Minute,
		OAuthRefreshSkew:  time.Hour,
		OAuthRefreshEvery: time.Minute,
		OAuthLoginTTL:     time.Minute,
		MaxBodyBytes:      1024,
	}
}
