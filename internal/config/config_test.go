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
		OAuthPlatform:     DefaultOAuthPlatform,
		AuthFile:          DefaultAuthFile,
		APIBaseURL:        "https://example.com",
		OAuthHost:         DefaultOAuthHost,
		OAuthConsoleURL:   DefaultOAuthConsoleURL,
		OAuthClientID:     DefaultOAuthClientID,
		OAuthGuidanceURLs: append([]string(nil), DefaultGlobalLoginGuidanceURLs...),
		OAuthCallbackBase: "http://127.0.0.1:8000",
		Bind:              "127.0.0.1",
		Port:              "8000",
		UpstreamMode:      "auto",
		RequestTimeout:    time.Second,
		HeaderTimeout:     time.Second,
		ShutdownTimeout:   time.Second,
		ModelCacheTTL:     time.Minute,
		ModelFailureTTL:   time.Minute,
		ModelSuccessTTL:   time.Hour,
		OAuthRefreshSkew:  time.Hour,
		OAuthRefreshEvery: time.Minute,
		OAuthLoginTTL:     time.Minute,
		MaxBodyBytes:      1024,
	}
}

func TestParseInstalledProductSelectsPlatformClient(t *testing.T) {
	root := map[string]any{
		"tronBuildVersion": "3.5.99-build",
		"appVersion":       "3.5.99",
		"quality":          "stable",
		"iCubeApp": map[string]any{
			"authConfig": map[string]any{
				"TRAE": map[string]any{
					"stable": "trae-global-client",
				},
				"SOLO": map[string]any{
					"stable": "solo-client",
				},
			},
		},
	}

	trae := parseInstalledProduct(root, "global")
	if trae.PluginVersion != "3.5.99-build" || trae.AppVersion != "3.5.99" || trae.ClientID != "trae-global-client" {
		t.Fatalf("global TRAE product = %+v", trae)
	}

	solo := parseInstalledProduct(root, "global-solo")
	if solo.ClientID != "solo-client" {
		t.Fatalf("global SOLO client id = %q", solo.ClientID)
	}
}

func TestValidateRejectsNonPositiveModelCapabilityTTLs(t *testing.T) {
	cfg := validConfig()
	cfg.ModelFailureTTL = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected zero TRAE_MODEL_FAILURE_TTL to fail validation")
	}

	cfg = validConfig()
	cfg.ModelSuccessTTL = -time.Second
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected negative TRAE_MODEL_SUCCESS_TTL to fail validation")
	}
}
