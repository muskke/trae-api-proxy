package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/muskke/trae-api-proxy/internal/config"
)

var ErrReauthRequired = errors.New("TRAE login required")

const credentialVersion = 1

type Account struct {
	UID          string `json:"uid,omitempty"`
	EnterpriseID string `json:"enterpriseId,omitempty"`
	Nickname     string `json:"nickname,omitempty"`
}

type TokenData struct {
	AccessToken      string `json:"accessToken,omitempty"`
	RefreshToken     string `json:"refreshToken,omitempty"`
	ExpiresAt        int64  `json:"expiresAt,omitempty"`
	RefreshExpiresAt int64  `json:"refreshExpiresAt,omitempty"`
	Domain           string `json:"domain,omitempty"`
	APIHost          string `json:"apiHost,omitempty"`
	MachineID        string `json:"machineId,omitempty"`
	DeviceID         string `json:"deviceId,omitempty"`
}

type Credential struct {
	Version int       `json:"version,omitempty"`
	Account Account   `json:"account"`
	Auth    TokenData `json:"auth"`
}

type Session struct {
	Token     string
	UID       string
	MachineID string
	DeviceID  string
	Source    string
}

type Status struct {
	Mode             string `json:"mode"`
	Source           string `json:"source"`
	Authenticated    bool   `json:"authenticated"`
	Refreshable      bool   `json:"refreshable"`
	AutoRefresh      bool   `json:"auto_refresh"`
	NeedsRefresh     bool   `json:"needs_refresh"`
	UID              string `json:"uid,omitempty"`
	Nickname         string `json:"nickname,omitempty"`
	EnterpriseID     string `json:"enterprise_id,omitempty"`
	ExpiresAt        int64  `json:"expires_at,omitempty"`
	RefreshExpiresAt int64  `json:"refresh_expires_at,omitempty"`
	LastRefreshAt    int64  `json:"last_refresh_at,omitempty"`
	LastError        string `json:"last_error,omitempty"`
}

type pendingLogin struct {
	MachineID string
	DeviceID  string
	TraceID   string
	CreatedAt time.Time
}

type callbackInfo struct {
	RefreshToken string
	AccessToken  string
	UID          string
	Nickname     string
	EnterpriseID string
	ExpiresAt    int64
}

type exchangeResult struct {
	AccessToken      string
	RefreshToken     string
	ExpiresAt        int64
	RefreshExpiresAt int64
}

type userInfo struct {
	UID          string
	Nickname     string
	EnterpriseID string
}

type Manager struct {
	cfg        *config.Config
	httpClient *http.Client

	mu            sync.RWMutex
	credential    *Credential
	lastRefreshAt time.Time
	lastError     string

	refreshMu sync.Mutex
	loginMu   sync.Mutex
	pending   map[string]pendingLogin
	now       func() time.Time
}

func NewManager(cfg *config.Config) *Manager {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = cfg.HeaderTimeout
	m := &Manager{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout:   cfg.RequestTimeout,
			Transport: transport,
		},
		pending: make(map[string]pendingLogin),
		now:     time.Now,
	}
	if err := m.loadCredential(); err != nil && !errors.Is(err, os.ErrNotExist) {
		m.lastError = "load credential: " + err.Error()
	}
	return m
}

func (m *Manager) CloseIdleConnections() {
	m.httpClient.CloseIdleConnections()
}

func (m *Manager) ResolveSession(ctx context.Context, requestToken string) (Session, error) {
	switch m.cfg.AuthMode {
	case "oauth":
		return m.oauthSession(ctx)
	case "token":
		return m.staticSession(), nil
	case "passthrough":
		if strings.TrimSpace(requestToken) == "" {
			return Session{}, ErrReauthRequired
		}
		return m.passthroughSession(requestToken), nil
	default: // auto
		if m.hasOAuthCredential() {
			session, err := m.oauthSession(ctx)
			if err == nil {
				return session, nil
			}
			if strings.TrimSpace(m.cfg.IdeToken) == "" {
				return Session{}, err
			}
		}
		if strings.TrimSpace(m.cfg.IdeToken) != "" {
			return m.staticSession(), nil
		}
		if strings.TrimSpace(requestToken) != "" {
			return m.passthroughSession(requestToken), nil
		}
		return Session{}, ErrReauthRequired
	}
}

func (m *Manager) oauthSession(ctx context.Context) (Session, error) {
	if !m.hasOAuthCredential() {
		return Session{}, ErrReauthRequired
	}
	_, refreshErr := m.RefreshIfNeeded(ctx)
	s := m.oauthSnapshot()
	if strings.TrimSpace(s.Token) == "" {
		if refreshErr != nil {
			return Session{}, fmt.Errorf("%w: %v", ErrReauthRequired, refreshErr)
		}
		return Session{}, ErrReauthRequired
	}
	if refreshErr != nil && m.sessionExpired() {
		return Session{}, fmt.Errorf("%w: %v", ErrReauthRequired, refreshErr)
	}
	return s, nil
}

func (m *Manager) staticSession() Session {
	return Session{
		Token:     m.cfg.IdeToken,
		UID:       m.cfg.UID,
		MachineID: m.cfg.MachineID,
		DeviceID:  m.cfg.DeviceID,
		Source:    "token",
	}
}

func (m *Manager) passthroughSession(token string) Session {
	return Session{
		Token:     token,
		UID:       m.cfg.UID,
		MachineID: m.cfg.MachineID,
		DeviceID:  m.cfg.DeviceID,
		Source:    "passthrough",
	}
}

func (m *Manager) oauthSnapshot() Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.credential == nil {
		return Session{Source: "oauth"}
	}
	return Session{
		Token:     m.credential.Auth.AccessToken,
		UID:       m.credential.Account.UID,
		MachineID: m.credential.Auth.MachineID,
		DeviceID:  m.credential.Auth.DeviceID,
		Source:    "oauth",
	}
}

func (m *Manager) hasOAuthCredential() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.credential != nil && (strings.TrimSpace(m.credential.Auth.AccessToken) != "" || strings.TrimSpace(m.credential.Auth.RefreshToken) != "")
}

func (m *Manager) sessionExpired() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.credential == nil || m.credential.Auth.ExpiresAt <= 0 {
		return false
	}
	return m.now().Unix() >= m.credential.Auth.ExpiresAt
}

func (m *Manager) RefreshIfNeeded(ctx context.Context) (bool, error) {
	return m.refresh(ctx, false)
}

func (m *Manager) ForceRefresh(ctx context.Context) error {
	_, err := m.refresh(ctx, true)
	return err
}

func (m *Manager) refresh(ctx context.Context, force bool) (bool, error) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	m.mu.RLock()
	if m.credential == nil {
		m.mu.RUnlock()
		return false, ErrReauthRequired
	}
	cred := *m.credential
	m.mu.RUnlock()

	if strings.TrimSpace(cred.Auth.RefreshToken) == "" {
		if force || cred.Auth.AccessToken == "" || (cred.Auth.ExpiresAt > 0 && m.now().Unix() >= cred.Auth.ExpiresAt) {
			return false, ErrReauthRequired
		}
		return false, nil
	}
	if cred.Auth.RefreshExpiresAt > 0 && m.now().Unix() >= cred.Auth.RefreshExpiresAt {
		m.setLastError("refresh token expired; login required")
		return false, ErrReauthRequired
	}
	if !force && !needsRefresh(cred.Auth.ExpiresAt, m.cfg.OAuthRefreshSkew, m.now()) {
		return false, nil
	}

	result, err := m.exchange(ctx, cred.Auth.APIHost, cred.Auth.RefreshToken)
	if err != nil {
		m.setLastError(err.Error())
		return false, err
	}
	cred.Auth.AccessToken = result.AccessToken
	if result.RefreshToken != "" {
		cred.Auth.RefreshToken = result.RefreshToken
	}
	if result.ExpiresAt > 0 {
		cred.Auth.ExpiresAt = result.ExpiresAt
	}
	if result.RefreshExpiresAt > 0 {
		cred.Auth.RefreshExpiresAt = result.RefreshExpiresAt
	}
	if cred.Auth.APIHost == "" {
		cred.Auth.APIHost = m.cfg.OAuthHost
	}
	if cred.Version == 0 {
		cred.Version = credentialVersion
	}

	m.mu.Lock()
	m.credential = &cred
	m.lastRefreshAt = m.now()
	m.lastError = ""
	m.mu.Unlock()

	if err := saveCredential(m.cfg.AuthFile, &cred); err != nil {
		// The freshly rotated token remains usable in memory. Keep serving and make
		// the persistence problem visible through /auth/status and logs.
		m.setLastError("credential persistence failed: " + err.Error())
		log.Printf("TRAE auth refresh persistence failed: %v", err)
	}
	return true, nil
}

func needsRefresh(expiresAt int64, within time.Duration, now time.Time) bool {
	if expiresAt <= 0 {
		return true
	}
	return now.Add(within).Unix() >= expiresAt
}

func (m *Manager) Run(ctx context.Context) {
	m.refreshBackground(ctx)
	ticker := time.NewTicker(m.cfg.OAuthRefreshEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.refreshBackground(ctx)
		}
	}
}

func (m *Manager) refreshBackground(parent context.Context) {
	if !m.hasRefreshToken() {
		return
	}
	ctx, cancel := context.WithTimeout(parent, m.cfg.RequestTimeout)
	defer cancel()
	refreshed, err := m.RefreshIfNeeded(ctx)
	if err != nil {
		log.Printf("TRAE auth background refresh failed: %v", err)
		return
	}
	if refreshed {
		log.Printf("TRAE auth token refreshed automatically")
	}
}

func (m *Manager) hasRefreshToken() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.credential != nil && strings.TrimSpace(m.credential.Auth.RefreshToken) != ""
}

func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()

	st := Status{Mode: m.cfg.AuthMode, Source: "none", LastError: m.lastError}
	if !m.lastRefreshAt.IsZero() {
		st.LastRefreshAt = m.lastRefreshAt.Unix()
	}
	if m.credential != nil && (m.credential.Auth.AccessToken != "" || m.credential.Auth.RefreshToken != "") {
		st.Source = "oauth"
		st.Authenticated = m.credential.Auth.AccessToken != "" && (m.credential.Auth.ExpiresAt <= 0 || m.now().Unix() < m.credential.Auth.ExpiresAt)
		st.Refreshable = m.credential.Auth.RefreshToken != "" && (m.credential.Auth.RefreshExpiresAt <= 0 || m.now().Unix() < m.credential.Auth.RefreshExpiresAt)
		st.AutoRefresh = st.Refreshable
		st.NeedsRefresh = st.Refreshable && needsRefresh(m.credential.Auth.ExpiresAt, m.cfg.OAuthRefreshSkew, m.now())
		st.UID = m.credential.Account.UID
		st.Nickname = m.credential.Account.Nickname
		st.EnterpriseID = m.credential.Account.EnterpriseID
		st.ExpiresAt = m.credential.Auth.ExpiresAt
		st.RefreshExpiresAt = m.credential.Auth.RefreshExpiresAt
		return st
	}
	if m.cfg.AuthMode == "token" || (m.cfg.AuthMode == "auto" && m.cfg.IdeToken != "") {
		st.Source = "token"
		st.Authenticated = m.cfg.IdeToken != ""
		return st
	}
	if m.cfg.AuthMode == "passthrough" || m.cfg.AuthMode == "auto" {
		st.Source = "passthrough"
	}
	return st
}

func (m *Manager) StartLogin() (string, error) {
	state, err := randomHex(16)
	if err != nil {
		return "", err
	}
	machineID, err := randomHex(16)
	if err != nil {
		return "", err
	}
	deviceID, err := randomHex(16)
	if err != nil {
		return "", err
	}
	traceID := machineTraceID(machineID, deviceID)
	callbackURL := strings.TrimRight(m.cfg.OAuthCallbackBase, "/") + "/auth/callback/" + state

	loginURL, err := url.Parse(m.cfg.OAuthConsoleURL)
	if err != nil {
		return "", err
	}
	q := loginURL.Query()
	q.Set("login_version", "1")
	q.Set("auth_from", "solo")
	q.Set("login_channel", "native_ide")
	q.Set("plugin_version", m.cfg.OAuthPluginVersion)
	q.Set("auth_type", "local")
	q.Set("client_id", m.cfg.OAuthClientID)
	q.Set("redirect", "0")
	q.Set("login_trace_id", traceID)
	q.Set("auth_callback_url", callbackURL)
	q.Set("machine_id", machineID)
	q.Set("device_id", deviceID)
	q.Set("x_device_id", deviceID)
	q.Set("x_machine_id", machineID)
	q.Set("x_device_brand", "PC")
	q.Set("x_device_type", "PC")
	q.Set("x_os_version", "1.0")
	q.Set("x_app_version", m.cfg.IDEVersion)
	q.Set("x_app_type", m.cfg.IDEVersionType)
	loginURL.RawQuery = q.Encode()

	m.loginMu.Lock()
	m.prunePendingLocked()
	m.pending[state] = pendingLogin{
		MachineID: machineID,
		DeviceID:  deviceID,
		TraceID:   traceID,
		CreatedAt: m.now(),
	}
	m.loginMu.Unlock()
	return loginURL.String(), nil
}

func (m *Manager) CompleteLogin(ctx context.Context, state string, query url.Values) (Status, error) {
	pending, err := m.consumePending(state)
	if err != nil {
		return Status{}, err
	}
	if got := strings.TrimSpace(query.Get("loginTraceID")); got != "" && !strings.EqualFold(got, pending.TraceID) {
		return Status{}, fmt.Errorf("login callback trace mismatch")
	}

	info, err := parseCallback(query)
	if err != nil {
		return Status{}, err
	}
	cred := Credential{
		Version: credentialVersion,
		Account: Account{
			UID:          info.UID,
			Nickname:     info.Nickname,
			EnterpriseID: info.EnterpriseID,
		},
		Auth: TokenData{
			AccessToken:  info.AccessToken,
			RefreshToken: info.RefreshToken,
			ExpiresAt:    info.ExpiresAt,
			Domain:       "trae.cn",
			APIHost:      m.cfg.OAuthHost,
			MachineID:    pending.MachineID,
			DeviceID:     pending.DeviceID,
		},
	}

	if cred.Auth.RefreshToken != "" {
		result, exErr := m.exchange(ctx, cred.Auth.APIHost, cred.Auth.RefreshToken)
		if exErr != nil {
			return Status{}, exErr
		}
		cred.Auth.AccessToken = result.AccessToken
		if result.RefreshToken != "" {
			cred.Auth.RefreshToken = result.RefreshToken
		}
		cred.Auth.ExpiresAt = result.ExpiresAt
		cred.Auth.RefreshExpiresAt = result.RefreshExpiresAt
	}
	if cred.Auth.AccessToken == "" {
		return Status{}, fmt.Errorf("login callback did not provide a usable access token")
	}

	ui, infoErr := m.getUserInfo(ctx, cred.Auth.APIHost, cred.Auth.AccessToken)
	if infoErr == nil {
		if ui.UID != "" {
			cred.Account.UID = ui.UID
		}
		if ui.Nickname != "" {
			cred.Account.Nickname = ui.Nickname
		}
		if ui.EnterpriseID != "" {
			cred.Account.EnterpriseID = ui.EnterpriseID
		}
	}
	if cred.Account.UID == "" {
		if infoErr != nil {
			return Status{}, fmt.Errorf("cannot determine TRAE UID: %w", infoErr)
		}
		return Status{}, fmt.Errorf("cannot determine TRAE UID")
	}
	if err := saveCredential(m.cfg.AuthFile, &cred); err != nil {
		return Status{}, fmt.Errorf("save credential: %w", err)
	}

	m.mu.Lock()
	m.credential = &cred
	m.lastRefreshAt = m.now()
	m.lastError = ""
	m.mu.Unlock()
	return m.Status(), nil
}

func (m *Manager) Logout() error {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	m.mu.Lock()
	m.credential = nil
	m.lastRefreshAt = time.Time{}
	m.lastError = ""
	m.mu.Unlock()
	if err := os.Remove(m.cfg.AuthFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (m *Manager) consumePending(state string) (pendingLogin, error) {
	m.loginMu.Lock()
	defer m.loginMu.Unlock()
	m.prunePendingLocked()
	p, ok := m.pending[state]
	if !ok {
		return pendingLogin{}, fmt.Errorf("login state is invalid or expired")
	}
	delete(m.pending, state)
	return p, nil
}

func (m *Manager) prunePendingLocked() {
	for state, p := range m.pending {
		if m.now().Sub(p.CreatedAt) > m.cfg.OAuthLoginTTL {
			delete(m.pending, state)
		}
	}
}

func (m *Manager) loadCredential() error {
	raw, err := os.ReadFile(m.cfg.AuthFile)
	if err != nil {
		return err
	}
	var cred Credential
	if err := json.Unmarshal(raw, &cred); err != nil {
		return fmt.Errorf("decode %s: %w", m.cfg.AuthFile, err)
	}
	if cred.Version == 0 {
		cred.Version = credentialVersion
	}
	if cred.Auth.AccessToken == "" && cred.Auth.RefreshToken == "" {
		return fmt.Errorf("credential contains neither accessToken nor refreshToken")
	}
	if cred.Auth.APIHost == "" {
		cred.Auth.APIHost = m.cfg.OAuthHost
	}
	m.credential = &cred
	return nil
}

func saveCredential(path string, cred *Credential) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("empty credential path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cred, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".trae-auth-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err == nil {
		return nil
	}
	// Windows may not replace an existing destination with os.Rename. This
	// fallback keeps the new rotated refresh token rather than losing it.
	if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return removeErr
	}
	return os.Rename(tmpName, path)
}

func (m *Manager) exchange(ctx context.Context, apiHost, refreshToken string) (exchangeResult, error) {
	if apiHost == "" {
		apiHost = m.cfg.OAuthHost
	}
	body := map[string]any{
		"ClientID":     m.cfg.OAuthClientID,
		"RefreshToken": refreshToken,
		"ClientSecret": "-",
		"UserID":       "",
	}
	var envelope struct {
		Result struct {
			Token                 string `json:"Token"`
			RefreshToken          string `json:"RefreshToken"`
			TokenExpireAt         int64  `json:"TokenExpireAt"`
			TokenExpireDuration   int64  `json:"TokenExpireDuration"`
			RefreshExpireAt       int64  `json:"RefreshExpireAt"`
			RefreshExpireDuration int64  `json:"RefreshExpireDuration"`
		} `json:"Result"`
	}
	if err := m.postOAuthJSON(ctx, apiHost+m.cfg.OAuthExchangePath, body, "", &envelope); err != nil {
		return exchangeResult{}, err
	}
	if envelope.Result.Token == "" {
		return exchangeResult{}, fmt.Errorf("ExchangeToken response did not contain Token; login required")
	}
	return exchangeResult{
		AccessToken:      envelope.Result.Token,
		RefreshToken:     envelope.Result.RefreshToken,
		ExpiresAt:        expiryFrom(envelope.Result.TokenExpireAt, envelope.Result.TokenExpireDuration, m.now()),
		RefreshExpiresAt: expiryFrom(envelope.Result.RefreshExpireAt, envelope.Result.RefreshExpireDuration, m.now()),
	}, nil
}

func (m *Manager) getUserInfo(ctx context.Context, apiHost, accessToken string) (userInfo, error) {
	if apiHost == "" {
		apiHost = m.cfg.OAuthHost
	}
	body := map[string]any{"ReqSource": "IDE", "IDEVersion": m.cfg.IDEVersion}
	var envelope struct {
		Result struct {
			UserID       string `json:"UserID"`
			ScreenName   string `json:"ScreenName"`
			EnterpriseID string `json:"EnterpriseID"`
		} `json:"Result"`
	}
	if err := m.postOAuthJSON(ctx, apiHost+m.cfg.OAuthUserInfoPath, body, accessToken, &envelope); err != nil {
		return userInfo{}, err
	}
	return userInfo{
		UID:          envelope.Result.UserID,
		Nickname:     envelope.Result.ScreenName,
		EnterpriseID: envelope.Result.EnterpriseID,
	}, nil
}

func (m *Manager) postOAuthJSON(ctx context.Context, endpoint string, body any, accessToken string, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Trae/"+m.cfg.IDEVersion)
	if accessToken != "" {
		req.Header.Set("X-Cloudide-Token", accessToken)
	}
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Keep provider response bodies out of logs/status. OAuth error payloads can
		// contain session metadata and are not needed to classify refresh failures.
		return fmt.Errorf("OAuth endpoint returned HTTP %d (%s)", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	if err := json.Unmarshal(responseBody, out); err != nil {
		return fmt.Errorf("decode OAuth response: %w", err)
	}
	return nil
}

func parseCallback(q url.Values) (callbackInfo, error) {
	info := callbackInfo{RefreshToken: strings.TrimSpace(q.Get("refreshToken"))}
	userInfoMap := parseJSONParam(q.Get("userInfo"))
	info.UID = jsonString(userInfoMap, "UserID")
	info.Nickname = jsonString(userInfoMap, "ScreenName")
	info.EnterpriseID = jsonString(userInfoMap, "TenantID")

	jwt := parseJSONParam(q.Get("userJwt"))
	if info.RefreshToken == "" {
		info.RefreshToken = jsonString(jwt, "RefreshToken")
	}
	if info.RefreshToken == "" {
		info.AccessToken = jsonString(jwt, "Token")
		info.ExpiresAt = normalizeExpiry(jsonInt64(jwt, "TokenExpireAt"))
		if info.AccessToken == "" {
			return callbackInfo{}, fmt.Errorf("login callback missing refreshToken and userJwt.Token")
		}
	}
	return info, nil
}

func parseJSONParam(raw string) map[string]any {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	candidates := []string{raw}
	if decoded, err := url.QueryUnescape(raw); err == nil && decoded != raw {
		candidates = append(candidates, decoded)
	}
	for _, candidate := range candidates {
		dec := json.NewDecoder(strings.NewReader(candidate))
		dec.UseNumber()
		var out map[string]any
		if err := dec.Decode(&out); err == nil && out != nil {
			return out
		}
	}
	return nil
}

func jsonString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	default:
		return fmt.Sprint(v)
	}
}

func jsonInt64(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch x := v.(type) {
	case json.Number:
		n, _ := x.Int64()
		return n
	case float64:
		return int64(x)
	case string:
		var n int64
		_, _ = fmt.Sscan(x, &n)
		return n
	default:
		return 0
	}
}

func expiryFrom(expireAt, duration int64, now time.Time) int64 {
	if expireAt > 0 {
		normalized := normalizeExpiry(expireAt)
		if normalized > now.Unix() {
			return normalized
		}
	}
	if duration > 0 {
		return now.Add(time.Duration(duration) * time.Second).Unix()
	}
	return 0
}

func normalizeExpiry(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func machineTraceID(machineID, deviceID string) string {
	joined := machineID + deviceID
	if len(joined) >= 16 {
		return joined[len(joined)-16:]
	}
	return strings.Repeat("0", 16-len(joined)) + joined
}

func (m *Manager) setLastError(message string) {
	m.mu.Lock()
	m.lastError = message
	m.mu.Unlock()
}
