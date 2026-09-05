package auth

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
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

const credentialVersion = 3

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
	APIBaseURL       string `json:"apiBaseUrl,omitempty"`
	LoginHost        string `json:"loginHost,omitempty"`
	LoginRegion      string `json:"loginRegion,omitempty"`
	Platform         string `json:"platform,omitempty"`
	ClientID         string `json:"clientId,omitempty"`
	MachineID        string `json:"machineId,omitempty"`
	DeviceID         string `json:"deviceId,omitempty"`
}

type Credential struct {
	Version int       `json:"version,omitempty"`
	Account Account   `json:"account"`
	Auth    TokenData `json:"auth"`
}

type Session struct {
	Token      string
	UID        string
	MachineID  string
	DeviceID   string
	APIBaseURL string
	Platform   string
	Source     string
}

type Status struct {
	Mode             string `json:"mode"`
	Source           string `json:"source"`
	Platform         string `json:"platform,omitempty"`
	LoginRegion      string `json:"login_region,omitempty"`
	APIBaseURL       string `json:"api_base_url,omitempty"`
	OAuthHost        string `json:"oauth_host,omitempty"`
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
	Platform      string
	ClientID      string
	MachineID     string
	DeviceID      string
	TraceID       string
	LoginHost     string
	CodeVerifier  string
	AppVersion    string
	PluginVersion string
	DeviceBrand   string
	DeviceType    string
	OSVersion     string
	CreatedAt     time.Time
}

type callbackInfo struct {
	RefreshToken string
	AccessToken  string
	AuthCode     string
	LoginHost    string
	LoginRegion  string
	APIHost      string
	UserTag      string
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
	APIHost          string
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
		Token:      m.cfg.IdeToken,
		UID:        m.cfg.UID,
		MachineID:  m.cfg.MachineID,
		DeviceID:   m.cfg.DeviceID,
		APIBaseURL: m.cfg.APIBaseURL,
		Platform:   m.cfg.OAuthPlatform,
		Source:     "token",
	}
}

func (m *Manager) passthroughSession(token string) Session {
	return Session{
		Token:      token,
		UID:        m.cfg.UID,
		MachineID:  m.cfg.MachineID,
		DeviceID:   m.cfg.DeviceID,
		APIBaseURL: m.cfg.APIBaseURL,
		Platform:   m.cfg.OAuthPlatform,
		Source:     "passthrough",
	}
}

func (m *Manager) oauthSnapshot() Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.credential == nil {
		return Session{Source: "oauth"}
	}
	return Session{
		Token:      m.credential.Auth.AccessToken,
		UID:        m.credential.Account.UID,
		MachineID:  m.credential.Auth.MachineID,
		DeviceID:   m.credential.Auth.DeviceID,
		APIBaseURL: firstNonEmpty(m.credential.Auth.APIBaseURL, m.cfg.APIBaseURL),
		Platform:   firstNonEmpty(m.credential.Auth.Platform, m.cfg.OAuthPlatform),
		Source:     "oauth",
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

	result, err := m.exchangeRefresh(ctx, cred.Auth, cred.Auth.RefreshToken)
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
	if result.APIHost != "" {
		cred.Auth.APIHost = result.APIHost
	}
	if cred.Auth.APIHost == "" {
		cred.Auth.APIHost = m.cfg.OAuthHost
	}
	if cred.Auth.Platform == "" {
		cred.Auth.Platform = m.cfg.OAuthPlatform
	}
	if cred.Auth.ClientID == "" {
		cred.Auth.ClientID = m.cfg.OAuthClientID
	}
	if cred.Auth.APIBaseURL == "" {
		cred.Auth.APIBaseURL = apiBaseURLForRegion(cred.Auth.LoginRegion, cred.Auth.Platform, m.cfg.APIBaseURL)
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

	st := Status{
		Mode:       m.cfg.AuthMode,
		Source:     "none",
		Platform:   m.cfg.OAuthPlatform,
		APIBaseURL: m.cfg.APIBaseURL,
		OAuthHost:  m.cfg.OAuthHost,
		LastError:  m.lastError,
	}
	if !m.lastRefreshAt.IsZero() {
		st.LastRefreshAt = m.lastRefreshAt.Unix()
	}
	if m.credential != nil && (m.credential.Auth.AccessToken != "" || m.credential.Auth.RefreshToken != "") {
		st.Source = "oauth"
		st.Platform = firstNonEmpty(m.credential.Auth.Platform, m.cfg.OAuthPlatform)
		st.LoginRegion = m.credential.Auth.LoginRegion
		st.APIBaseURL = firstNonEmpty(m.credential.Auth.APIBaseURL, m.cfg.APIBaseURL)
		st.OAuthHost = firstNonEmpty(m.credential.Auth.APIHost, m.cfg.OAuthHost)
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

func (m *Manager) StartLogin(ctx context.Context) (string, error) {
	state, err := randomHex(16)
	if err != nil {
		return "", err
	}
	machineID := strings.TrimSpace(m.cfg.MachineID)
	if machineID == "" {
		machineID, err = randomUUID()
		if err != nil {
			return "", err
		}
	}
	deviceID := loginDeviceID(m.cfg.DeviceID)
	traceID, err := randomUUID()
	if err != nil {
		return "", err
	}
	codeVerifier, codeChallenge, err := generatePKCE()
	if err != nil {
		return "", err
	}
	loginHost, err := m.requestLoginGuidance(ctx, traceID)
	if err != nil {
		return "", err
	}
	callbackURL := strings.TrimRight(m.cfg.OAuthCallbackBase, "/") + "/authorize"
	loginURL, err := m.buildVerificationURL(loginHost, traceID, callbackURL, machineID, deviceID, codeChallenge)
	if err != nil {
		return "", err
	}

	m.loginMu.Lock()
	m.prunePendingLocked()
	m.pending[state] = pendingLogin{
		Platform:      m.cfg.OAuthPlatform,
		ClientID:      m.cfg.OAuthClientID,
		MachineID:     machineID,
		DeviceID:      deviceID,
		TraceID:       traceID,
		LoginHost:     loginHost,
		CodeVerifier:  codeVerifier,
		AppVersion:    m.cfg.IDEVersion,
		PluginVersion: m.cfg.OAuthPluginVersion,
		DeviceBrand:   m.cfg.DeviceBrand,
		DeviceType:    m.cfg.DeviceType,
		OSVersion:     m.cfg.OSVersion,
		CreatedAt:     m.now(),
	}
	m.loginMu.Unlock()
	return loginURL, nil
}

func (m *Manager) requestLoginGuidance(ctx context.Context, traceID string) (string, error) {
	body := map[string]any{"loginTraceID": traceID, "login_trace_id": traceID}
	var failures []string
	for _, endpoint := range m.cfg.OAuthGuidanceURLs {
		var response map[string]any
		if err := m.postOAuthJSON(ctx, endpoint, body, "", &response); err != nil {
			failures = append(failures, endpoint+": "+err.Error())
			continue
		}
		if host := findStringByKeys(response, "LoginHost", "loginHost", "LoginURL", "loginUrl"); host != "" {
			return host, nil
		}
		failures = append(failures, endpoint+": response missing LoginHost")
	}
	if m.cfg.OAuthIsCN() {
		u, err := url.Parse(m.cfg.OAuthConsoleURL)
		if err == nil && u.Scheme != "" && u.Host != "" {
			return u.Scheme + "://" + u.Host, nil
		}
	}
	return "", fmt.Errorf("TRAE LoginGuidance failed for %s: %s", m.cfg.OAuthPlatform, strings.Join(failures, " | "))
}

func (m *Manager) buildVerificationURL(loginHost, traceID, callbackURL, machineID, deviceID, codeChallenge string) (string, error) {
	loginHost = strings.TrimSpace(loginHost)
	if !strings.Contains(loginHost, "://") {
		loginHost = "https://" + loginHost
	}
	loginURL, err := url.Parse(loginHost)
	if err != nil || loginURL.Host == "" {
		return "", fmt.Errorf("invalid LoginGuidance host %q", loginHost)
	}
	loginURL.Path = "/authorization"
	loginURL.RawQuery = ""
	loginURL.Fragment = ""
	q := loginURL.Query()
	q.Set("login_version", "1")
	if m.cfg.OAuthIsSolo() {
		q.Set("auth_from", "solo")
		q.Set("hide_saas_login", "true")
	} else {
		q.Set("auth_from", "trae")
	}
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
	q.Set("x_device_brand", m.cfg.DeviceBrand)
	q.Set("x_device_type", m.cfg.DeviceType)
	q.Set("x_os_version", m.cfg.OSVersion)
	q.Set("x_env", "")
	q.Set("x_app_version", m.cfg.IDEVersion)
	q.Set("x_app_type", m.cfg.IDEVersionType)
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")
	loginURL.RawQuery = q.Encode()
	return loginURL.String(), nil
}

func (m *Manager) CompleteLogin(ctx context.Context, state string, query url.Values) (Status, error) {
	pending, err := m.consumePendingForCallback(state, query)
	if err != nil {
		return Status{}, err
	}
	if got := firstNonEmpty(query.Get("loginTraceID"), query.Get("login_trace_id")); got != "" && !strings.EqualFold(got, pending.TraceID) {
		return Status{}, fmt.Errorf("login callback trace mismatch")
	}

	info, err := parseCallback(query)
	if err != nil {
		return Status{}, err
	}
	if info.LoginHost == "" {
		info.LoginHost = pending.LoginHost
	}
	if info.LoginRegion == "" {
		info.LoginRegion = inferLoginRegion(info.LoginHost, pending.Platform)
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
			Domain:       domainForPlatform(pending.Platform),
			APIHost:      info.APIHost,
			LoginHost:    info.LoginHost,
			LoginRegion:  info.LoginRegion,
			Platform:     pending.Platform,
			ClientID:     pending.ClientID,
			MachineID:    pending.MachineID,
			DeviceID:     pending.DeviceID,
			APIBaseURL:   apiBaseURLForRegion(info.LoginRegion, pending.Platform, m.cfg.APIBaseURL),
		},
	}

	if info.AuthCode != "" {
		result, exErr := m.exchangeAuthCode(ctx, pending, info.AuthCode, info.UserTag)
		if exErr != nil {
			return Status{}, exErr
		}
		applyExchangeResult(&cred.Auth, result)
	} else if cred.Auth.RefreshToken != "" {
		result, exErr := m.exchangeRefresh(ctx, cred.Auth, cred.Auth.RefreshToken)
		if exErr != nil {
			return Status{}, exErr
		}
		applyExchangeResult(&cred.Auth, result)
	}
	if cred.Auth.AccessToken == "" {
		return Status{}, fmt.Errorf("login callback did not provide a usable access token")
	}

	ui, userInfoHost, infoErr := m.getUserInfo(ctx, cred.Auth, cred.Auth.AccessToken)
	if infoErr == nil {
		if userInfoHost != "" {
			cred.Auth.APIHost = userInfoHost
		}
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
	if cred.Account.UID == "" && infoErr != nil {
		// Some current Global callbacks do not expose UserID, while the access token
		// is still completely usable. Keep the session rather than throwing away a
		// successful authorization; /auth/status will simply omit UID.
		m.setLastError("GetUserInfo after login: " + infoErr.Error())
	}
	if err := saveCredential(m.cfg.AuthFile, &cred); err != nil {
		return Status{}, fmt.Errorf("save credential: %w", err)
	}

	m.mu.Lock()
	m.credential = &cred
	m.lastRefreshAt = m.now()
	if infoErr == nil {
		m.lastError = ""
	}
	m.mu.Unlock()
	return m.Status(), nil
}

func (m *Manager) exchangeAuthCode(ctx context.Context, pending pendingLogin, authCode, userTag string) (exchangeResult, error) {
	publicKey, err := generateDevicePublicKey()
	if err != nil {
		return exchangeResult{}, err
	}
	platformCode := "IDE_PC"
	if strings.HasSuffix(pending.Platform, "solo") {
		platformCode = "SOLO_PC"
	}
	deviceName := firstNonEmpty(os.Getenv("USERNAME"), os.Getenv("USER"), os.Getenv("HOSTNAME"), "PC")
	body := map[string]any{
		"ClientID":     pending.ClientID,
		"AuthCode":     authCode,
		"CodeVerifier": pending.CodeVerifier,
		"IDEVersion":   pending.AppVersion,
		"DeviceInfo": map[string]any{
			"DeviceID":        pending.DeviceID,
			"MachineID":       pending.MachineID,
			"PlatformCode":    platformCode,
			"DeviceType":      "PC",
			"DeviceName":      deviceName,
			"DeviceModel":     pending.DeviceBrand,
			"ClientVersion":   pending.AppVersion,
			"DevicePublicKey": publicKey,
			"DeviceBrand":     deviceBrandForType(pending.DeviceType, pending.DeviceBrand),
			"DeviceCPU":       "",
			"OSInfo":          pending.DeviceType,
			"OSVersion":       pending.OSVersion,
		},
	}
	var failures []string
	for _, host := range m.authCodeAPIHosts(pending.Platform, userTag) {
		var response map[string]any
		endpoint := strings.TrimRight(host, "/") + m.cfg.OAuthAuthCodeExchangePath
		if err := m.postOAuthJSON(ctx, endpoint, body, "", &response); err != nil {
			failures = append(failures, host+": "+err.Error())
			continue
		}
		result := parseExchangeResult(response, m.now())
		if result.AccessToken == "" {
			failures = append(failures, host+": response missing access token")
			continue
		}
		result.APIHost = host
		return result, nil
	}
	return exchangeResult{}, fmt.Errorf("TRAE AuthCode ExchangeToken failed: %s", strings.Join(failures, " | "))
}

func (m *Manager) authCodeAPIHosts(platform, userTag string) []string {
	var hosts []string
	if isCustomOAuthHost(m.cfg.OAuthHost, platform) {
		hosts = append(hosts, m.cfg.OAuthHost)
	}
	if strings.HasPrefix(platform, "cn") {
		hosts = append(hosts, "https://api.trae.cn", "https://api.trae.com.cn")
	} else if strings.EqualFold(strings.TrimSpace(userTag), "usttp") || strings.EqualFold(strings.TrimSpace(userTag), "us_ttp") || strings.EqualFold(strings.TrimSpace(userTag), "us-ttp") {
		hosts = append(hosts, "https://grow-normal.traeapi.us")
	} else {
		hosts = append(hosts, "https://growsg-normal.trae.ai", "https://grow-normal.trae.ai")
	}
	return dedupeStrings(hosts)
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

func (m *Manager) consumePendingForCallback(state string, query url.Values) (pendingLogin, error) {
	if strings.TrimSpace(state) != "" {
		return m.consumePending(state)
	}
	m.loginMu.Lock()
	defer m.loginMu.Unlock()
	m.prunePendingLocked()
	traceID := firstNonEmpty(query.Get("loginTraceID"), query.Get("login_trace_id"))
	var foundKey string
	var found pendingLogin
	for key, candidate := range m.pending {
		if traceID != "" && strings.EqualFold(traceID, candidate.TraceID) {
			foundKey, found = key, candidate
			break
		}
	}
	if foundKey == "" && traceID == "" && len(m.pending) == 1 {
		for key, candidate := range m.pending {
			foundKey, found = key, candidate
		}
	}
	if foundKey == "" {
		return pendingLogin{}, fmt.Errorf("login callback does not match an active login session")
	}
	delete(m.pending, foundKey)
	return found, nil
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
	oldVersion := cred.Version
	if cred.Auth.AccessToken == "" && cred.Auth.RefreshToken == "" {
		return fmt.Errorf("credential contains neither accessToken nor refreshToken")
	}
	if cred.Auth.Platform == "" {
		if strings.Contains(strings.ToLower(cred.Auth.Domain+" "+cred.Auth.APIHost), ".cn") {
			if oldVersion <= 1 {
				cred.Auth.Platform = "cn-solo"
			} else {
				cred.Auth.Platform = "cn"
			}
		} else {
			cred.Auth.Platform = firstNonEmpty(m.cfg.OAuthPlatform, config.DefaultOAuthPlatform)
		}
	}
	if cred.Auth.ClientID == "" {
		if strings.HasSuffix(cred.Auth.Platform, "solo") {
			cred.Auth.ClientID = config.DefaultSoloOAuthClientID
		} else {
			cred.Auth.ClientID = m.cfg.OAuthClientID
		}
	}
	if cred.Auth.APIHost == "" {
		cred.Auth.APIHost = m.cfg.OAuthHost
	}
	if cred.Auth.LoginHost == "" {
		if strings.HasPrefix(cred.Auth.Platform, "cn") {
			cred.Auth.LoginHost = "https://www.trae.cn"
		} else {
			cred.Auth.LoginHost = "https://www.trae.ai"
		}
	}
	if cred.Auth.LoginRegion == "" {
		cred.Auth.LoginRegion = inferLoginRegion(cred.Auth.LoginHost, cred.Auth.Platform)
	}
	if cred.Auth.APIBaseURL == "" || isLegacyGeneratedCoreHost(cred.Auth.APIBaseURL) {
		cred.Auth.APIBaseURL = apiBaseURLForRegion(cred.Auth.LoginRegion, cred.Auth.Platform, m.cfg.APIBaseURL)
	}
	cred.Version = credentialVersion
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

func (m *Manager) exchangeRefresh(ctx context.Context, auth TokenData, refreshToken string) (exchangeResult, error) {
	body := map[string]any{
		"ClientID":     firstNonEmpty(auth.ClientID, m.cfg.OAuthClientID),
		"RefreshToken": refreshToken,
		"ClientSecret": "-",
		"UserID":       "",
	}
	var failures []string
	for _, apiHost := range m.oauthAPIHosts(auth) {
		var response map[string]any
		endpoint := strings.TrimRight(apiHost, "/") + m.cfg.OAuthExchangePath
		if err := m.postOAuthJSON(ctx, endpoint, body, auth.AccessToken, &response); err != nil {
			failures = append(failures, apiHost+": "+err.Error())
			continue
		}
		result := parseExchangeResult(response, m.now())
		if result.AccessToken == "" {
			failures = append(failures, apiHost+": response missing Token")
			continue
		}
		result.APIHost = apiHost
		return result, nil
	}
	return exchangeResult{}, fmt.Errorf("TRAE refresh ExchangeToken failed: %s", strings.Join(failures, " | "))
}

func (m *Manager) getUserInfo(ctx context.Context, auth TokenData, accessToken string) (userInfo, string, error) {
	body := map[string]any{"ReqSource": "IDE", "IDEVersion": m.cfg.IDEVersion}
	var failures []string
	for _, apiHost := range m.oauthAPIHosts(auth) {
		var response map[string]any
		endpoint := strings.TrimRight(apiHost, "/") + m.cfg.OAuthUserInfoPath
		if err := m.postOAuthJSON(ctx, endpoint, body, accessToken, &response); err != nil {
			failures = append(failures, apiHost+": "+err.Error())
			continue
		}
		ui := userInfo{
			UID:          firstNonEmpty(nestedString(response, "Result", "UserID"), nestedString(response, "result", "userId"), findStringByKeys(response, "UserID", "userId", "uid")),
			Nickname:     firstNonEmpty(nestedString(response, "Result", "ScreenName"), nestedString(response, "result", "screenName"), findStringByKeys(response, "ScreenName", "screenName", "nickname")),
			EnterpriseID: firstNonEmpty(nestedString(response, "Result", "EnterpriseID"), nestedString(response, "result", "enterpriseId"), findStringByKeys(response, "EnterpriseID", "enterpriseId", "TenantID")),
		}
		return ui, apiHost, nil
	}
	return userInfo{}, "", fmt.Errorf("TRAE GetUserInfo failed: %s", strings.Join(failures, " | "))
}

func (m *Manager) oauthAPIHosts(auth TokenData) []string {
	var hosts []string
	platform := firstNonEmpty(auth.Platform, m.cfg.OAuthPlatform, config.DefaultOAuthPlatform)
	if isCustomOAuthHost(m.cfg.OAuthHost, platform) {
		hosts = append(hosts, auth.APIHost, m.cfg.OAuthHost)
		return dedupeStrings(hosts)
	}
	if auth.APIHost != "" {
		hosts = append(hosts, auth.APIHost)
	}
	if auth.LoginHost != "" {
		if origin := urlOrigin(auth.LoginHost); origin != "" {
			hosts = append(hosts, origin)
			if u, err := url.Parse(origin); err == nil {
				host := u.Hostname()
				if strings.HasPrefix(host, "www.") {
					hosts = append(hosts, u.Scheme+"://api."+strings.TrimPrefix(host, "www."))
				}
			}
		}
	}
	if m.cfg.OAuthHost != "" {
		hosts = append(hosts, m.cfg.OAuthHost)
	}
	if strings.HasPrefix(platform, "cn") {
		hosts = append(hosts, "https://api.trae.cn", "https://api.trae.com.cn", "https://www.trae.cn")
	} else {
		hosts = append(hosts, "https://api.marscode.com", "https://api.trae.ai", "https://www.trae.ai", "https://www.marscode.com")
	}
	return dedupeStrings(hosts)
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
	info := callbackInfo{
		RefreshToken: firstNonEmpty(q.Get("refreshToken"), q.Get("refresh_token"), q.Get("RefreshToken")),
		AuthCode:     firstNonEmpty(q.Get("authCode"), q.Get("auth_code"), q.Get("AuthCode"), q.Get("authorization_code"), q.Get("code")),
		LoginHost:    firstNonEmpty(q.Get("loginHost"), q.Get("login_host"), q.Get("LoginHost")),
		LoginRegion:  strings.ToLower(firstNonEmpty(q.Get("userRegion"), q.Get("user_region"), q.Get("UserRegion"), q.Get("loginRegion"), q.Get("login_region"), q.Get("LoginRegion"))),
		APIHost:      firstNonEmpty(q.Get("host"), q.Get("apiHost"), q.Get("api_host"), q.Get("APIHost")),
		UserTag:      firstNonEmpty(q.Get("userTag"), q.Get("user_tag"), q.Get("UserTag")),
	}
	if info.AuthCode == "" {
		for _, key := range []string{"authCodeInfo", "auth_code_info", "AuthCodeInfo"} {
			if raw := strings.TrimSpace(q.Get(key)); raw != "" {
				parsed := parseJSONParam(raw)
				info.AuthCode = firstNonEmpty(jsonString(parsed, "AuthCode"), jsonString(parsed, "authCode"), jsonString(parsed, "auth_code"), jsonString(parsed, "code"))
				if expireAt := firstPositive(jsonInt64(parsed, "ExpireAt"), jsonInt64(parsed, "expireAt"), jsonInt64(parsed, "expiresAt")); expireAt > 0 {
					expires := expireAt
					if expires < 1e12 {
						expires *= 1000
					}
					if expires <= time.Now().UnixMilli() {
						return callbackInfo{}, fmt.Errorf("login callback authCodeInfo is expired")
					}
				}
				break
			}
		}
	}

	userInfoMap := parseJSONParam(q.Get("userInfo"))
	info.UID = firstNonEmpty(jsonString(userInfoMap, "UserID"), jsonString(userInfoMap, "userId"), jsonString(userInfoMap, "uid"))
	info.Nickname = firstNonEmpty(jsonString(userInfoMap, "ScreenName"), jsonString(userInfoMap, "screenName"), jsonString(userInfoMap, "nickname"))
	info.EnterpriseID = firstNonEmpty(jsonString(userInfoMap, "TenantID"), jsonString(userInfoMap, "EnterpriseID"), jsonString(userInfoMap, "enterpriseId"))

	jwt := parseJSONParam(firstNonEmpty(q.Get("userJwt"), q.Get("user_jwt")))
	if info.RefreshToken == "" {
		info.RefreshToken = firstNonEmpty(jsonString(jwt, "RefreshToken"), jsonString(jwt, "refreshToken"))
	}
	info.AccessToken = firstNonEmpty(q.Get("accessToken"), q.Get("access_token"), q.Get("token"), jsonString(jwt, "Token"), jsonString(jwt, "AccessToken"), jsonString(jwt, "accessToken"))
	info.ExpiresAt = normalizeExpiry(firstPositive(jsonInt64(jwt, "TokenExpireAt"), jsonInt64(jwt, "expiresAt")))

	if info.RefreshToken == "" && info.AuthCode == "" && info.AccessToken == "" {
		return callbackInfo{}, fmt.Errorf("login callback missing authCodeInfo/AuthCode, refreshToken, and access token")
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
		return strings.TrimSpace(x)
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
	return anyInt64(v)
}

func anyInt64(v any) int64 {
	switch x := v.(type) {
	case json.Number:
		n, _ := x.Int64()
		return n
	case float64:
		return int64(x)
	case float32:
		return int64(x)
	case int:
		return int64(x)
	case int64:
		return x
	case string:
		var n int64
		_, _ = fmt.Sscan(x, &n)
		return n
	default:
		return 0
	}
}

func parseExchangeResult(root map[string]any, now time.Time) exchangeResult {
	accessToken := firstNonEmpty(
		nestedString(root, "Result", "Token"), nestedString(root, "Result", "AccessToken"),
		nestedString(root, "result", "Token"), nestedString(root, "result", "token"), nestedString(root, "result", "accessToken"),
		nestedString(root, "data", "Token"), nestedString(root, "data", "token"), nestedString(root, "data", "accessToken"),
		findStringByKeys(root, "Token", "AccessToken", "accessToken", "access_token"),
	)
	refreshToken := firstNonEmpty(
		nestedString(root, "Result", "RefreshToken"), nestedString(root, "result", "RefreshToken"), nestedString(root, "result", "refreshToken"),
		nestedString(root, "data", "RefreshToken"), nestedString(root, "data", "refreshToken"),
		findStringByKeys(root, "RefreshToken", "refreshToken", "refresh_token"),
	)
	tokenExpireAt := firstPositive(
		nestedInt64(root, "Result", "TokenExpireAt"), nestedInt64(root, "result", "TokenExpireAt"), nestedInt64(root, "result", "tokenExpireAt"),
		nestedInt64(root, "data", "TokenExpireAt"), nestedInt64(root, "data", "tokenExpireAt"),
	)
	tokenDuration := firstPositive(
		nestedInt64(root, "Result", "TokenExpireDuration"), nestedInt64(root, "result", "TokenExpireDuration"), nestedInt64(root, "result", "tokenExpireDuration"),
	)
	refreshExpireAt := firstPositive(
		nestedInt64(root, "Result", "RefreshExpireAt"), nestedInt64(root, "result", "RefreshExpireAt"), nestedInt64(root, "result", "refreshExpireAt"),
		nestedInt64(root, "data", "RefreshExpireAt"), nestedInt64(root, "data", "refreshExpireAt"),
	)
	refreshDuration := firstPositive(
		nestedInt64(root, "Result", "RefreshExpireDuration"), nestedInt64(root, "result", "RefreshExpireDuration"), nestedInt64(root, "result", "refreshExpireDuration"),
	)
	return exchangeResult{
		AccessToken:      accessToken,
		RefreshToken:     refreshToken,
		ExpiresAt:        expiryFrom(tokenExpireAt, tokenDuration, now),
		RefreshExpiresAt: expiryFrom(refreshExpireAt, refreshDuration, now),
	}
}

func applyExchangeResult(auth *TokenData, result exchangeResult) {
	if result.AccessToken != "" {
		auth.AccessToken = result.AccessToken
	}
	if result.RefreshToken != "" {
		auth.RefreshToken = result.RefreshToken
	}
	if result.ExpiresAt > 0 {
		auth.ExpiresAt = result.ExpiresAt
	}
	if result.RefreshExpiresAt > 0 {
		auth.RefreshExpiresAt = result.RefreshExpiresAt
	}
	if result.APIHost != "" {
		auth.APIHost = result.APIHost
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

func randomUUID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	hexID := hex.EncodeToString(buf)
	return hexID[0:8] + "-" + hexID[8:12] + "-" + hexID[12:16] + "-" + hexID[16:20] + "-" + hexID[20:32], nil
}

func generatePKCE() (string, string, error) {
	random := make([]byte, 48)
	if _, err := rand.Read(random); err != nil {
		return "", "", err
	}
	verifier := base64.RawURLEncoding.EncodeToString(random)
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	return verifier, challenge, nil
}

func generateDevicePublicKey() (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generate TRAE device key: %w", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", fmt.Errorf("marshal TRAE device key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

func loginDeviceID(configured string) string {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		return "0"
	}
	for _, r := range configured {
		if r < '0' || r > '9' {
			return "0"
		}
	}
	return configured
}

func inferLoginRegion(loginHost, platform string) string {
	if strings.HasPrefix(platform, "cn") {
		return "cn"
	}
	lower := strings.ToLower(loginHost)
	if strings.Contains(lower, ".cn") {
		return "cn"
	}
	if strings.Contains(lower, ".us") || strings.Contains(lower, "us-") {
		return "us"
	}
	return "sg"
}

func domainForPlatform(platform string) string {
	if strings.HasPrefix(platform, "cn") {
		return "trae.cn"
	}
	return "trae.ai"
}

func apiBaseURLForRegion(region, platform, fallback string) string {
	fallback = strings.TrimRight(strings.TrimSpace(fallback), "/")
	if fallback != "" && !isKnownGeneratedCoreHost(fallback) {
		return fallback
	}
	if strings.HasPrefix(platform, "cn") || strings.EqualFold(region, "cn") {
		return config.DefaultTraeCNBaseURL
	}
	switch strings.ToLower(strings.TrimSpace(region)) {
	case "us", "usttp", "us_ttp", "us-ttp":
		return config.DefaultTraeUSBaseURL
	case "sg", "row", "global", "":
		return config.DefaultTraeBaseURL
	default:
		if fallback != "" && !isLegacyGeneratedCoreHost(fallback) {
			return fallback
		}
		return config.DefaultTraeBaseURL
	}
}

func isKnownGeneratedCoreHost(raw string) bool {
	raw = strings.TrimRight(strings.ToLower(strings.TrimSpace(raw)), "/")
	switch raw {
	case strings.ToLower(config.DefaultTraeBaseURL),
		strings.ToLower(config.DefaultTraeUSBaseURL),
		strings.ToLower(config.DefaultTraeCNBaseURL),
		"https://trae-api-sg.mchost.guru",
		"https://trae-api-us.mchost.guru":
		return true
	default:
		return false
	}
}

func isLegacyGeneratedCoreHost(raw string) bool {
	raw = strings.TrimRight(strings.ToLower(strings.TrimSpace(raw)), "/")
	return raw == "https://trae-api-sg.mchost.guru" || raw == "https://trae-api-us.mchost.guru"
}

func deviceBrandForType(deviceType, fallback string) string {
	switch strings.ToLower(deviceType) {
	case "mac", "darwin":
		return "Apple"
	case "windows":
		return "Microsoft"
	case "linux":
		return "Linux"
	default:
		return fallback
	}
}

func urlOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func isCustomOAuthHost(host, platform string) bool {
	host = strings.TrimRight(strings.TrimSpace(host), "/")
	if host == "" {
		return false
	}
	if strings.HasPrefix(platform, "cn") {
		return host != "https://api.trae.com.cn" && host != "https://api.trae.cn"
	}
	return host != "https://api.trae.ai"
}

func dedupeStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimRight(strings.TrimSpace(value), "/")
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstPositive(values ...int64) int64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func nestedString(root map[string]any, path ...string) string {
	var current any = root
	for _, key := range path {
		obj, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current, ok = obj[key]
		if !ok {
			return ""
		}
	}
	switch value := current.(type) {
	case string:
		return strings.TrimSpace(value)
	case json.Number:
		return value.String()
	default:
		return ""
	}
}

func nestedInt64(root map[string]any, path ...string) int64 {
	var current any = root
	for _, key := range path {
		obj, ok := current.(map[string]any)
		if !ok {
			return 0
		}
		current, ok = obj[key]
		if !ok {
			return 0
		}
	}
	return anyInt64(current)
}

func findStringByKeys(root map[string]any, keys ...string) string {
	if root == nil {
		return ""
	}
	wanted := map[string]struct{}{}
	for _, key := range keys {
		wanted[key] = struct{}{}
	}
	var walk func(any) string
	walk = func(value any) string {
		switch node := value.(type) {
		case map[string]any:
			for key, child := range node {
				if _, ok := wanted[key]; ok {
					switch v := child.(type) {
					case string:
						if strings.TrimSpace(v) != "" {
							return strings.TrimSpace(v)
						}
					case json.Number:
						return v.String()
					}
				}
			}
			for _, child := range node {
				if found := walk(child); found != "" {
					return found
				}
			}
		case []any:
			for _, child := range node {
				if found := walk(child); found != "" {
					return found
				}
			}
		}
		return ""
	}
	return walk(root)
}

func (m *Manager) setLastError(message string) {
	m.mu.Lock()
	m.lastError = message
	m.mu.Unlock()
}
