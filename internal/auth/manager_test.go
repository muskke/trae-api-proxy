package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muskke/trae-api-proxy/internal/config"
)

func TestCompleteLoginExchangesPersistsAndReloads(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var exchangeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/guidance":
			_ = json.NewEncoder(w).Encode(map[string]any{"Result": map[string]any{"LoginHost": serverURLFromRequest(r)}})
		case config.DefaultOAuthExchangePath:
			exchangeCalls.Add(1)
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["ClientID"] != config.DefaultOAuthClientID || body["RefreshToken"] != "refresh-old" {
				t.Fatalf("unexpected exchange body: %#v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Result": map[string]any{
				"Token":           "access-new",
				"RefreshToken":    "refresh-new",
				"TokenExpireAt":   (now.Add(48 * time.Hour).Unix()) * 1000,
				"RefreshExpireAt": (now.Add(30 * 24 * time.Hour).Unix()) * 1000,
			}})
		case config.DefaultOAuthUserInfoPath:
			if got := r.Header.Get("X-Cloudide-Token"); got != "access-new" {
				t.Fatalf("userinfo token = %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Result": map[string]any{
				"UserID": "uid-1", "ScreenName": "tester", "EnterpriseID": "ent-1",
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := testConfig(t, server.URL)
	m := NewManager(cfg)
	m.now = func() time.Time { return now }

	loginURL, err := m.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lu, _ := url.Parse(loginURL)
	callbackURL, _ := url.Parse(lu.Query().Get("auth_callback_url"))
	if callbackURL.Path != "/authorize" || lu.Query().Get("machine_id") == "" || lu.Query().Get("code_challenge") == "" {
		t.Fatalf("login URL missing callback/PKCE/device metadata: %s", loginURL)
	}

	q := url.Values{}
	q.Set("refreshToken", "refresh-old")
	q.Set("loginTraceID", lu.Query().Get("login_trace_id"))
	q.Set("userInfo", `{"UserID":"uid-from-callback","ScreenName":"callback-name"}`)
	st, err := m.CompleteLogin(context.Background(), "", q)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Authenticated || !st.Refreshable || st.UID != "uid-1" || st.Nickname != "tester" {
		t.Fatalf("status = %#v", st)
	}
	if exchangeCalls.Load() != 1 {
		t.Fatalf("exchange calls = %d", exchangeCalls.Load())
	}

	info, err := os.Stat(cfg.AuthFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode = %o", info.Mode().Perm())
	}

	reloaded := NewManager(cfg)
	reloaded.now = func() time.Time { return now }
	session, err := reloaded.ResolveSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if session.Token != "access-new" || session.UID != "uid-1" || session.Source != "oauth" {
		t.Fatalf("session = %#v", session)
	}
	if session.MachineID != lu.Query().Get("machine_id") || session.DeviceID != lu.Query().Get("device_id") {
		t.Fatalf("session device metadata not preserved: %#v", session)
	}

	raw, _ := os.ReadFile(cfg.AuthFile)
	if !strings.Contains(string(raw), "refresh-new") || !strings.Contains(string(raw), "access-new") {
		t.Fatalf("rotated credentials not persisted: %s", raw)
	}
}

func TestGlobalAuthCodeLoginUsesGuidancePKCEAndDeviceInfo(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var authCodeCalls atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/guidance":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["loginTraceID"] == "" || body["login_trace_id"] == "" {
				t.Fatalf("guidance body = %#v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Result": map[string]any{"LoginHost": server.URL}})
		case config.DefaultOAuthAuthCodeExchangePath:
			authCodeCalls.Add(1)
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["ClientID"] != config.DefaultOAuthClientID || body["AuthCode"] != "auth-code-1" || body["CodeVerifier"] == "" {
				t.Fatalf("auth-code exchange body = %#v", body)
			}
			device, _ := body["DeviceInfo"].(map[string]any)
			if device["PlatformCode"] != "IDE_PC" || !strings.Contains(fmt.Sprint(device["DevicePublicKey"]), "BEGIN PUBLIC KEY") {
				t.Fatalf("device info = %#v", device)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Result": map[string]any{
				"Token": "authcode-access", "RefreshToken": "authcode-refresh",
				"TokenExpireAt":   now.Add(48*time.Hour).Unix() * 1000,
				"RefreshExpireAt": now.Add(30*24*time.Hour).Unix() * 1000,
			}})
		case config.DefaultOAuthUserInfoPath:
			_ = json.NewEncoder(w).Encode(map[string]any{"Result": map[string]any{"UserID": "uid-global", "ScreenName": "global-user"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := testConfig(t, server.URL)
	cfg.DeviceID = "not-numeric"
	cfg.DeviceType = "windows"
	cfg.DeviceBrand = "PC"
	cfg.OSVersion = "Windows"
	m := NewManager(cfg)
	m.now = func() time.Time { return now }

	loginURL, err := m.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(loginURL)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	if parsed.Host != strings.TrimPrefix(server.URL, "http://") || parsed.Path != "/authorization" {
		t.Fatalf("login URL = %s", loginURL)
	}
	if q.Get("auth_from") != "trae" || q.Get("client_id") != config.DefaultOAuthClientID || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Fatalf("login query = %#v", q)
	}
	if q.Get("device_id") != "0" || q.Get("auth_callback_url") != "http://127.0.0.1:8000/authorize" {
		t.Fatalf("login callback/device = %#v", q)
	}

	callback := url.Values{}
	callback.Set("loginTraceID", q.Get("login_trace_id"))
	callback.Set("loginHost", server.URL)
	callback.Set("loginRegion", "sg")
	callback.Set("authCodeInfo", fmt.Sprintf(`{"AuthCode":"auth-code-1","ExpireAt":%d}`, time.Now().Add(time.Minute).UnixMilli()))
	status, err := m.CompleteLogin(context.Background(), "", callback)
	if err != nil {
		t.Fatal(err)
	}
	if authCodeCalls.Load() != 1 || !status.Authenticated || !status.Refreshable || status.Platform != "global" || status.UID != "uid-global" {
		t.Fatalf("status=%#v authCodeCalls=%d", status, authCodeCalls.Load())
	}
	session, err := m.ResolveSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if session.Token != "authcode-access" || session.Platform != "global" || session.APIBaseURL != "https://example.com" {
		t.Fatalf("session = %#v", session)
	}
}

func TestConcurrentResolveRefreshesOnlyOnce(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var exchangeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != config.DefaultOAuthExchangePath {
			http.NotFound(w, r)
			return
		}
		exchangeCalls.Add(1)
		time.Sleep(20 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{"Result": map[string]any{
			"Token": "fresh-access", "RefreshToken": "fresh-refresh",
			"TokenExpireAt": now.Add(72*time.Hour).Unix() * 1000,
		}})
	}))
	defer server.Close()

	cfg := testConfig(t, server.URL)
	writeCredential(t, cfg.AuthFile, Credential{Version: 1, Auth: TokenData{
		AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: now.Add(time.Hour).Unix(), APIHost: server.URL,
	}})
	m := NewManager(cfg)
	m.now = func() time.Time { return now }

	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session, err := m.ResolveSession(context.Background(), "")
			if err != nil {
				t.Errorf("resolve: %v", err)
				return
			}
			if session.Token != "fresh-access" {
				t.Errorf("token = %q", session.Token)
			}
		}()
	}
	wg.Wait()
	if exchangeCalls.Load() != 1 {
		t.Fatalf("exchange calls = %d, want 1", exchangeCalls.Load())
	}
}

func TestRefreshFailureUsesStillValidAccessToken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporary failure", http.StatusBadGateway)
	}))
	defer server.Close()

	cfg := testConfig(t, server.URL)
	writeCredential(t, cfg.AuthFile, Credential{Version: 1, Auth: TokenData{
		AccessToken: "still-valid", RefreshToken: "refresh", ExpiresAt: now.Add(2 * time.Hour).Unix(), APIHost: server.URL,
	}})
	m := NewManager(cfg)
	m.now = func() time.Time { return now }
	session, err := m.ResolveSession(context.Background(), "")
	if err != nil {
		t.Fatalf("valid access token should be used while proactive refresh fails: %v", err)
	}
	if session.Token != "still-valid" {
		t.Fatalf("token = %q", session.Token)
	}
	if m.Status().LastError == "" {
		t.Fatal("refresh failure should be visible in status")
	}
}

func TestExpiredSessionRefreshFailureRequiresLogin(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "expired refresh", http.StatusUnauthorized)
	}))
	defer server.Close()

	cfg := testConfig(t, server.URL)
	writeCredential(t, cfg.AuthFile, Credential{Version: 1, Auth: TokenData{
		AccessToken: "expired", RefreshToken: "refresh", ExpiresAt: now.Add(-time.Minute).Unix(), APIHost: server.URL,
	}})
	m := NewManager(cfg)
	m.now = func() time.Time { return now }
	_, err := m.ResolveSession(context.Background(), "")
	if !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("error = %v, want ErrReauthRequired", err)
	}
}

func TestParseCallbackFallsBackToUserJWT(t *testing.T) {
	q := url.Values{}
	q.Set("userInfo", `{"UserID":"u","ScreenName":"n","TenantID":"e"}`)
	q.Set("userJwt", `{"Token":"access","RefreshToken":"refresh","TokenExpireAt":1800000000000}`)
	info, err := parseCallback(q)
	if err != nil {
		t.Fatal(err)
	}
	if info.RefreshToken != "refresh" || info.UID != "u" || info.Nickname != "n" || info.EnterpriseID != "e" {
		t.Fatalf("callback = %#v", info)
	}
}

func TestAutoModeFallsBackToStaticTokenWhenOAuthSessionIsDead(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "dead", http.StatusUnauthorized)
	}))
	defer server.Close()
	cfg := testConfig(t, server.URL)
	cfg.AuthMode = "auto"
	cfg.IdeToken = "static-fallback"
	writeCredential(t, cfg.AuthFile, Credential{Version: 1, Auth: TokenData{
		AccessToken: "expired", RefreshToken: "dead-refresh", ExpiresAt: now.Add(-time.Minute).Unix(), APIHost: server.URL,
	}})
	m := NewManager(cfg)
	m.now = func() time.Time { return now }
	session, err := m.ResolveSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if session.Token != "static-fallback" || session.Source != "token" {
		t.Fatalf("session = %#v", session)
	}
}

func testConfig(t *testing.T, oauthHost string) *config.Config {
	t.Helper()
	return &config.Config{
		AuthMode:                  "oauth",
		AuthFile:                  filepath.Join(t.TempDir(), "trae-auth.json"),
		IdeToken:                  "",
		APIBaseURL:                "https://example.com",
		Bind:                      "127.0.0.1",
		Port:                      "8000",
		UpstreamMode:              "solo",
		OAuthPlatform:             "global",
		OAuthHost:                 oauthHost,
		OAuthConsoleURL:           "https://www.trae.ai/authorization",
		OAuthClientID:             config.DefaultOAuthClientID,
		OAuthGuidanceURLs:         []string{oauthHost + "/guidance"},
		OAuthExchangePath:         config.DefaultOAuthExchangePath,
		OAuthAuthCodeExchangePath: config.DefaultOAuthAuthCodeExchangePath,
		OAuthUserInfoPath:         config.DefaultOAuthUserInfoPath,
		OAuthPluginVersion:        config.DefaultOAuthPluginVer,
		OAuthCallbackBase:         "http://127.0.0.1:8000",
		OAuthRefreshSkew:          24 * time.Hour,
		OAuthRefreshEvery:         time.Minute,
		OAuthLoginTTL:             10 * time.Minute,
		IDEVersion:                config.DefaultIDEVersion,
		IDEVersionType:            config.DefaultIDEVersionType,
		RequestTimeout:            2 * time.Second,
		HeaderTimeout:             2 * time.Second,
	}
}

func writeCredential(t *testing.T, path string, cred Credential) {
	t.Helper()
	if err := saveCredential(path, &cred); err != nil {
		t.Fatal(err)
	}
}

func serverURLFromRequest(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
