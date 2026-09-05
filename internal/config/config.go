package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/muskke/trae-api-proxy/pkg/utils"
)

const (
	DefaultTraeBaseURL       = "https://trae-api-cn.mchost.guru"
	DefaultOAuthHost         = "https://api.trae.com.cn"
	DefaultOAuthConsoleURL   = "https://www.trae.cn/authorization"
	DefaultOAuthClientID     = "en1oxy7wnw8j9n"
	DefaultOAuthExchangePath = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	DefaultOAuthUserInfoPath = "/cloudide/api/v3/trae/GetUserInfo"
	DefaultOAuthPluginVer    = "2.3.62834"
	DefaultOAuthCallbackPort = "18080"
	DefaultAuthFile          = "./data/trae-auth.json"
	DefaultAppID             = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	DefaultIDEVersion        = "0.1.52"
	DefaultIDEVersionCode    = "20260811"
	DefaultIDEVersionType    = "stable"
	DefaultDeviceBrand       = "83DG"
	DefaultDeviceType        = "windows"
	DefaultOSVersion         = "Windows 11 Pro"
	DefaultUpstreamMode      = "auto"
	DefaultAuthMode          = "auto"
	DefaultModel             = "glm-5.2"
	DefaultRequestTraffic    = "prod"
	DefaultApplicationVer    = "default"
	defaultRequestTimeout    = 120 * time.Second
	defaultHeaderTimeout     = 120 * time.Second
	defaultShutdownTimeout   = 10 * time.Second
	defaultModelCacheTTL     = time.Hour
	defaultRefreshSkew       = 24 * time.Hour
	defaultRefreshInterval   = 15 * time.Minute
	defaultLoginTTL          = 10 * time.Minute
	defaultMaxBodyBytes      = int64(8 << 20)
)

type Config struct {
	AuthToken          string
	AuthMode           string
	AuthFile           string
	AppID              string
	AppVersion         string
	DeviceBrand        string
	DeviceCPU          string
	DeviceID           string
	DeviceType         string
	IDEVersion         string
	IDEVersionCode     string
	IDEVersionType     string
	MachineID          string
	OSVersion          string
	UID                string
	APIBaseURL         string
	Bind               string
	Port               string
	Locale             string
	IdeToken           string
	UpstreamMode       string
	DefaultModel       string
	ModelAliases       map[string]string
	RequestTrafficType string
	RequestTimeout     time.Duration
	HeaderTimeout      time.Duration
	ShutdownTimeout    time.Duration
	ModelCacheTTL      time.Duration
	MaxBodyBytes       int64
	CORSAllowOrigin    string

	OAuthHost          string
	OAuthConsoleURL    string
	OAuthClientID      string
	OAuthExchangePath  string
	OAuthUserInfoPath  string
	OAuthPluginVersion string
	OAuthCallbackBase  string
	OAuthCallbackBind  string
	OAuthRefreshSkew   time.Duration
	OAuthRefreshEvery  time.Duration
	OAuthLoginTTL      time.Duration
}

func Load() *Config {
	// A local .env is convenient for this single-binary proxy. Environment
	// variables still win, which keeps container and service deployments clean.
	_ = utils.LoadEnvFile(".env")

	port := utils.EnvOrDefault("PORT", "8000")
	callbackPort := utils.EnvOrDefault("TRAE_OAUTH_CALLBACK_PORT", DefaultOAuthCallbackPort)
	callbackBase := strings.TrimRight(os.Getenv("TRAE_OAUTH_CALLBACK_BASE_URL"), "/")
	if callbackBase == "" {
		callbackBase = "http://127.0.0.1:" + callbackPort
	}

	return &Config{
		AuthToken:          os.Getenv("AUTH_TOKEN"),
		AuthMode:           strings.ToLower(utils.EnvOrDefault("TRAE_AUTH_MODE", DefaultAuthMode)),
		AuthFile:           utils.EnvOrDefault("TRAE_AUTH_FILE", DefaultAuthFile),
		AppID:              utils.EnvOrDefault("TRAE_APP_ID", DefaultAppID),
		AppVersion:         utils.EnvOrDefault("TRAE_APP_VERSION", DefaultApplicationVer),
		DeviceBrand:        utils.EnvOrDefault("TRAE_DEVICE_BRAND", DefaultDeviceBrand),
		DeviceCPU:          os.Getenv("TRAE_DEVICE_CPU"),
		DeviceID:           os.Getenv("TRAE_DEVICE_ID"),
		DeviceType:         utils.EnvOrDefault("TRAE_DEVICE_TYPE", DefaultDeviceType),
		IdeToken:           os.Getenv("TRAE_IDE_TOKEN"),
		IDEVersion:         utils.EnvOrDefault("TRAE_IDE_VERSION", DefaultIDEVersion),
		IDEVersionCode:     utils.EnvOrDefault("TRAE_IDE_VERSION_CODE", DefaultIDEVersionCode),
		IDEVersionType:     utils.EnvOrDefault("TRAE_IDE_VERSION_TYPE", DefaultIDEVersionType),
		MachineID:          os.Getenv("TRAE_MACHINE_ID"),
		OSVersion:          utils.EnvOrDefault("TRAE_OS_VERSION", DefaultOSVersion),
		UID:                os.Getenv("TRAE_UID"),
		APIBaseURL:         strings.TrimRight(utils.EnvOrDefault("TRAE_API_BASE_URL", DefaultTraeBaseURL), "/"),
		Bind:               utils.EnvOrDefault("BIND", utils.EnvOrDefault("HOST", "0.0.0.0")),
		Port:               port,
		Locale:             utils.EnvOrDefault("TRAE_LOCALE", "zh-cn"),
		UpstreamMode:       strings.ToLower(utils.EnvOrDefault("TRAE_UPSTREAM_MODE", DefaultUpstreamMode)),
		DefaultModel:       utils.EnvOrDefault("TRAE_DEFAULT_MODEL", DefaultModel),
		ModelAliases:       parseAliases(os.Getenv("TRAE_MODEL_ALIASES")),
		RequestTrafficType: utils.EnvOrDefault("TRAE_REQUEST_TRAFFIC_TYPE", DefaultRequestTraffic),
		RequestTimeout:     envDuration("TRAE_REQUEST_TIMEOUT", defaultRequestTimeout),
		HeaderTimeout:      envDuration("TRAE_RESPONSE_HEADER_TIMEOUT", defaultHeaderTimeout),
		ShutdownTimeout:    envDuration("SHUTDOWN_TIMEOUT", defaultShutdownTimeout),
		ModelCacheTTL:      envDuration("TRAE_MODEL_CACHE_TTL", defaultModelCacheTTL),
		MaxBodyBytes:       envInt64("MAX_BODY_BYTES", defaultMaxBodyBytes),
		CORSAllowOrigin:    os.Getenv("CORS_ALLOW_ORIGIN"),

		OAuthHost:          strings.TrimRight(utils.EnvOrDefault("TRAE_OAUTH_HOST", DefaultOAuthHost), "/"),
		OAuthConsoleURL:    utils.EnvOrDefault("TRAE_OAUTH_CONSOLE_URL", DefaultOAuthConsoleURL),
		OAuthClientID:      utils.EnvOrDefault("TRAE_OAUTH_CLIENT_ID", DefaultOAuthClientID),
		OAuthExchangePath:  normalizePath(utils.EnvOrDefault("TRAE_OAUTH_EXCHANGE_PATH", DefaultOAuthExchangePath)),
		OAuthUserInfoPath:  normalizePath(utils.EnvOrDefault("TRAE_OAUTH_USERINFO_PATH", DefaultOAuthUserInfoPath)),
		OAuthPluginVersion: utils.EnvOrDefault("TRAE_OAUTH_PLUGIN_VERSION", DefaultOAuthPluginVer),
		OAuthCallbackBase:  callbackBase,
		OAuthCallbackBind:  strings.TrimSpace(os.Getenv("TRAE_OAUTH_CALLBACK_BIND")),
		OAuthRefreshSkew:   envDuration("TRAE_OAUTH_REFRESH_SKEW", defaultRefreshSkew),
		OAuthRefreshEvery:  envDuration("TRAE_OAUTH_REFRESH_INTERVAL", defaultRefreshInterval),
		OAuthLoginTTL:      envDuration("TRAE_OAUTH_LOGIN_TTL", defaultLoginTTL),
	}
}

func (c *Config) ListenAddr() string {
	return net.JoinHostPort(c.Bind, c.Port)
}

func (c *Config) OAuthCallbackListenAddr() (string, bool) {
	u, err := url.Parse(c.OAuthCallbackBase)
	if err != nil || u.Scheme != "http" || !isLoopbackHost(u.Hostname()) {
		return "", false
	}
	port := u.Port()
	if port == "" {
		port = "80"
	}
	bindHost := strings.TrimSpace(c.OAuthCallbackBind)
	if bindHost == "" {
		bindHost = u.Hostname()
	}
	return net.JoinHostPort(bindHost, port), true
}

func (c *Config) PrimaryServesOAuthCallback() bool {
	u, err := url.Parse(c.OAuthCallbackBase)
	if err != nil {
		return false
	}
	callbackPort := u.Port()
	if callbackPort == "" {
		if u.Scheme == "http" {
			callbackPort = "80"
		} else {
			callbackPort = "443"
		}
	}
	if callbackPort != c.Port {
		return false
	}
	if strings.EqualFold(c.Bind, u.Hostname()) {
		return true
	}
	if c.Bind == "0.0.0.0" || c.Bind == "::" || isLoopbackHost(c.Bind) {
		return isLoopbackHost(u.Hostname())
	}
	return false
}

func (c *Config) Validate() error {
	switch c.UpstreamMode {
	case "auto", "solo", "legacy":
	default:
		return fmt.Errorf("TRAE_UPSTREAM_MODE must be auto, solo, or legacy")
	}
	switch c.AuthMode {
	case "auto", "oauth", "token", "passthrough":
	default:
		return fmt.Errorf("TRAE_AUTH_MODE must be auto, oauth, token, or passthrough")
	}

	if err := validateAbsoluteHTTPURL("TRAE_API_BASE_URL", c.APIBaseURL); err != nil {
		return err
	}
	if err := validateAbsoluteHTTPURL("TRAE_OAUTH_HOST", c.OAuthHost); err != nil {
		return err
	}
	if err := validateAbsoluteHTTPURL("TRAE_OAUTH_CONSOLE_URL", c.OAuthConsoleURL); err != nil {
		return err
	}
	if err := validateCallbackURL(c.OAuthCallbackBase); err != nil {
		return err
	}
	if bind := strings.TrimSpace(c.OAuthCallbackBind); bind != "" && bind != "0.0.0.0" && bind != "::" && !isLoopbackHost(bind) {
		return fmt.Errorf("TRAE_OAUTH_CALLBACK_BIND must be loopback, 0.0.0.0, or ::")
	}

	port, err := strconv.Atoi(c.Port)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("PORT must be between 1 and 65535")
	}
	if c.AuthMode == "token" && strings.TrimSpace(c.IdeToken) == "" {
		return fmt.Errorf("TRAE_IDE_TOKEN is required when TRAE_AUTH_MODE=token")
	}
	if (c.AuthMode == "auto" || c.AuthMode == "oauth") && strings.TrimSpace(c.AuthFile) == "" {
		return fmt.Errorf("TRAE_AUTH_FILE must not be empty when OAuth auth is enabled")
	}
	if c.MaxBodyBytes <= 0 {
		return fmt.Errorf("MAX_BODY_BYTES must be greater than zero")
	}
	if c.RequestTimeout <= 0 || c.HeaderTimeout <= 0 || c.ShutdownTimeout <= 0 || c.ModelCacheTTL <= 0 || c.OAuthRefreshSkew <= 0 || c.OAuthRefreshEvery <= 0 || c.OAuthLoginTTL <= 0 {
		return fmt.Errorf("timeout values, refresh intervals, and TRAE_MODEL_CACHE_TTL must be greater than zero")
	}
	return nil
}

func validateAbsoluteHTTPURL(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s must be an absolute http(s) URL", name)
	}
	return nil
}

func validateCallbackURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("TRAE_OAUTH_CALLBACK_BASE_URL must be an absolute http(s) URL")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("TRAE_OAUTH_CALLBACK_BASE_URL must not contain a query or fragment")
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return fmt.Errorf("TRAE_OAUTH_CALLBACK_BASE_URL must use https unless it targets localhost")
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func normalizePath(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "/"
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	return raw
}

func parseAliases(raw string) map[string]string {
	aliases := map[string]string{}
	for _, item := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' }) {
		parts := strings.SplitN(item, "=", 2)
		if len(parts) != 2 {
			continue
		}
		alias := strings.TrimSpace(parts[0])
		target := strings.TrimSpace(parts[1])
		if alias == "" || target == "" {
			continue
		}
		aliases[strings.ToLower(alias)] = target
	}
	return aliases
}

func envDuration(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := time.ParseDuration(raw)
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

func envInt64(key string, fallback int64) int64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}
