package config

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/muskke/trae-api-proxy/pkg/utils"
)

const (
	DefaultOAuthPlatform             = "global"
	DefaultTraeBaseURL               = "https://coresg-normal.trae.ai"
	DefaultTraeUSBaseURL             = "https://coreva-normal.trae.ai"
	DefaultTraeCNBaseURL             = "https://trae-api-cn.mchost.guru"
	DefaultOAuthHost                 = "https://api.trae.ai"
	DefaultOAuthConsoleURL           = "https://www.trae.ai/authorization"
	DefaultOAuthClientID             = "ono9krqynydwx5"
	DefaultSoloOAuthClientID         = "en1oxy7wnw8j9n"
	DefaultOAuthExchangePath         = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	DefaultOAuthAuthCodeExchangePath = "/trae/api/v3/oauth/ExchangeToken"
	DefaultOAuthUserInfoPath         = "/cloudide/api/v3/trae/GetUserInfo"
	DefaultOAuthPluginVer            = "local"
	DefaultOAuthCallbackPort         = "18080"
	DefaultAuthFile                  = "./data/trae-auth.json"
	DefaultModelStatusFile           = "./data/model-status.json"
	DefaultReasoningMode             = "preserve"
	DefaultAppID                     = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	DefaultIDEVersion                = "3.5.66"
	DefaultIDEVersionCode            = "20260811"
	DefaultIDEVersionType            = "stable"
	DefaultDeviceBrand               = "PC"
	DefaultDeviceType                = "windows"
	DefaultOSVersion                 = "Windows"
	DefaultUpstreamMode              = "auto"
	DefaultAuthMode                  = "auto"
	DefaultModel                     = "glm-5.2"
	DefaultRequestTraffic            = "prod"
	DefaultApplicationVer            = "default"
	defaultRequestTimeout            = 120 * time.Second
	defaultHeaderTimeout             = 120 * time.Second
	defaultShutdownTimeout           = 10 * time.Second
	defaultModelCacheTTL             = time.Hour
	defaultModelFailureTTL           = 30 * time.Minute
	defaultModelSuccessTTL           = 6 * time.Hour
	defaultRefreshSkew               = 24 * time.Hour
	defaultRefreshInterval           = 15 * time.Minute
	defaultLoginTTL                  = 10 * time.Minute
	defaultMaxBodyBytes              = int64(8 << 20)
)

var (
	DefaultGlobalLoginGuidanceURLs = []string{
		"https://api.marscode.com/cloudide/api/v3/trae/GetLoginGuidance",
		"https://api.trae.ai/cloudide/api/v3/trae/GetLoginGuidance",
		"https://www.trae.ai/cloudide/api/v3/trae/GetLoginGuidance",
	}
	DefaultCNLoginGuidanceURLs = []string{
		"https://api.trae.cn/cloudide/api/v3/trae/GetLoginGuidance",
		"https://api.trae.com.cn/cloudide/api/v3/trae/GetLoginGuidance",
		"https://www.trae.cn/cloudide/api/v3/trae/GetLoginGuidance",
	}
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
	ModelFailureTTL    time.Duration
	ModelSuccessTTL    time.Duration
	ModelStatusFile    string
	ReasoningMode      string
	MaxBodyBytes       int64
	CORSAllowOrigin    string

	OAuthPlatform             string
	OAuthHost                 string
	OAuthConsoleURL           string
	OAuthClientID             string
	OAuthGuidanceURLs         []string
	OAuthExchangePath         string
	OAuthAuthCodeExchangePath string
	OAuthUserInfoPath         string
	OAuthPluginVersion        string
	OAuthCallbackBase         string
	OAuthCallbackBind         string
	OAuthRefreshSkew          time.Duration
	OAuthRefreshEvery         time.Duration
	OAuthLoginTTL             time.Duration
}

func Load() *Config {
	_ = utils.LoadEnvFile(".env")

	platform := canonicalOAuthPlatform(utils.EnvOrDefault("TRAE_OAUTH_PLATFORM", DefaultOAuthPlatform))
	defaults := oauthPlatformDefaults(platform)
	product := detectInstalledProduct(platform)
	clientID := firstConfigured("TRAE_OAUTH_CLIENT_ID", product.ClientID, defaults.ClientID)
	pluginVersion := firstConfigured("TRAE_OAUTH_PLUGIN_VERSION", product.PluginVersion, DefaultOAuthPluginVer)
	ideVersion := firstConfigured("TRAE_IDE_VERSION", product.AppVersion, DefaultIDEVersion)
	port := utils.EnvOrDefault("PORT", "8000")
	callbackPort := utils.EnvOrDefault("TRAE_OAUTH_CALLBACK_PORT", DefaultOAuthCallbackPort)
	callbackBase := strings.TrimRight(os.Getenv("TRAE_OAUTH_CALLBACK_BASE_URL"), "/")
	if callbackBase == "" {
		callbackBase = "http://127.0.0.1:" + callbackPort
	}

	guidanceURLs := parseURLList(os.Getenv("TRAE_OAUTH_GUIDANCE_URLS"))
	if len(guidanceURLs) == 0 {
		guidanceURLs = append([]string(nil), defaults.GuidanceURLs...)
	}

	return &Config{
		AuthToken:          os.Getenv("AUTH_TOKEN"),
		AuthMode:           strings.ToLower(utils.EnvOrDefault("TRAE_AUTH_MODE", DefaultAuthMode)),
		AuthFile:           utils.EnvOrDefault("TRAE_AUTH_FILE", DefaultAuthFile),
		AppID:              utils.EnvOrDefault("TRAE_APP_ID", DefaultAppID),
		AppVersion:         utils.EnvOrDefault("TRAE_APP_VERSION", DefaultApplicationVer),
		DeviceBrand:        utils.EnvOrDefault("TRAE_DEVICE_BRAND", defaultDeviceBrand()),
		DeviceCPU:          os.Getenv("TRAE_DEVICE_CPU"),
		DeviceID:           os.Getenv("TRAE_DEVICE_ID"),
		DeviceType:         utils.EnvOrDefault("TRAE_DEVICE_TYPE", defaultDeviceType()),
		IdeToken:           os.Getenv("TRAE_IDE_TOKEN"),
		IDEVersion:         ideVersion,
		IDEVersionCode:     utils.EnvOrDefault("TRAE_IDE_VERSION_CODE", DefaultIDEVersionCode),
		IDEVersionType:     utils.EnvOrDefault("TRAE_IDE_VERSION_TYPE", DefaultIDEVersionType),
		MachineID:          os.Getenv("TRAE_MACHINE_ID"),
		OSVersion:          utils.EnvOrDefault("TRAE_OS_VERSION", defaultOSVersion()),
		UID:                os.Getenv("TRAE_UID"),
		APIBaseURL:         strings.TrimRight(utils.EnvOrDefault("TRAE_API_BASE_URL", defaults.APIBaseURL), "/"),
		Bind:               utils.EnvOrDefault("BIND", utils.EnvOrDefault("HOST", "0.0.0.0")),
		Port:               port,
		Locale:             utils.EnvOrDefault("TRAE_LOCALE", defaults.Locale),
		UpstreamMode:       strings.ToLower(utils.EnvOrDefault("TRAE_UPSTREAM_MODE", DefaultUpstreamMode)),
		DefaultModel:       utils.EnvOrDefault("TRAE_DEFAULT_MODEL", DefaultModel),
		ModelAliases:       parseAliases(os.Getenv("TRAE_MODEL_ALIASES")),
		RequestTrafficType: utils.EnvOrDefault("TRAE_REQUEST_TRAFFIC_TYPE", DefaultRequestTraffic),
		RequestTimeout:     envDuration("TRAE_REQUEST_TIMEOUT", defaultRequestTimeout),
		HeaderTimeout:      envDuration("TRAE_RESPONSE_HEADER_TIMEOUT", defaultHeaderTimeout),
		ShutdownTimeout:    envDuration("SHUTDOWN_TIMEOUT", defaultShutdownTimeout),
		ModelCacheTTL:      envDuration("TRAE_MODEL_CACHE_TTL", defaultModelCacheTTL),
		ModelFailureTTL:    envDuration("TRAE_MODEL_FAILURE_TTL", defaultModelFailureTTL),
		ModelSuccessTTL:    envDuration("TRAE_MODEL_SUCCESS_TTL", defaultModelSuccessTTL),
		ModelStatusFile:    utils.EnvOrDefault("TRAE_MODEL_STATUS_FILE", DefaultModelStatusFile),
		ReasoningMode:      strings.ToLower(utils.EnvOrDefault("TRAE_REASONING_MODE", DefaultReasoningMode)),
		MaxBodyBytes:       envInt64("MAX_BODY_BYTES", defaultMaxBodyBytes),
		CORSAllowOrigin:    os.Getenv("CORS_ALLOW_ORIGIN"),

		OAuthPlatform:             platform,
		OAuthHost:                 strings.TrimRight(utils.EnvOrDefault("TRAE_OAUTH_HOST", defaults.OAuthHost), "/"),
		OAuthConsoleURL:           utils.EnvOrDefault("TRAE_OAUTH_CONSOLE_URL", defaults.ConsoleURL),
		OAuthClientID:             clientID,
		OAuthGuidanceURLs:         guidanceURLs,
		OAuthExchangePath:         normalizePath(utils.EnvOrDefault("TRAE_OAUTH_EXCHANGE_PATH", DefaultOAuthExchangePath)),
		OAuthAuthCodeExchangePath: normalizePath(utils.EnvOrDefault("TRAE_OAUTH_AUTH_CODE_EXCHANGE_PATH", DefaultOAuthAuthCodeExchangePath)),
		OAuthUserInfoPath:         normalizePath(utils.EnvOrDefault("TRAE_OAUTH_USERINFO_PATH", DefaultOAuthUserInfoPath)),
		OAuthPluginVersion:        pluginVersion,
		OAuthCallbackBase:         callbackBase,
		OAuthCallbackBind:         strings.TrimSpace(os.Getenv("TRAE_OAUTH_CALLBACK_BIND")),
		OAuthRefreshSkew:          envDuration("TRAE_OAUTH_REFRESH_SKEW", defaultRefreshSkew),
		OAuthRefreshEvery:         envDuration("TRAE_OAUTH_REFRESH_INTERVAL", defaultRefreshInterval),
		OAuthLoginTTL:             envDuration("TRAE_OAUTH_LOGIN_TTL", defaultLoginTTL),
	}
}

type oauthDefaults struct {
	OAuthHost    string
	ConsoleURL   string
	ClientID     string
	APIBaseURL   string
	Locale       string
	GuidanceURLs []string
}

func oauthPlatformDefaults(platform string) oauthDefaults {
	isCN := strings.HasPrefix(platform, "cn")
	isSolo := strings.HasSuffix(platform, "solo")
	clientID := DefaultOAuthClientID
	if isSolo {
		clientID = DefaultSoloOAuthClientID
	}
	if isCN {
		return oauthDefaults{
			OAuthHost:    "https://api.trae.com.cn",
			ConsoleURL:   "https://www.trae.cn/authorization",
			ClientID:     clientID,
			APIBaseURL:   DefaultTraeCNBaseURL,
			Locale:       "zh-cn",
			GuidanceURLs: DefaultCNLoginGuidanceURLs,
		}
	}
	return oauthDefaults{
		OAuthHost:    DefaultOAuthHost,
		ConsoleURL:   DefaultOAuthConsoleURL,
		ClientID:     clientID,
		APIBaseURL:   DefaultTraeBaseURL,
		Locale:       "en",
		GuidanceURLs: DefaultGlobalLoginGuidanceURLs,
	}
}

func canonicalOAuthPlatform(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "global", "trae", "global-trae", "trae-global":
		return "global"
	case "global-solo", "solo", "trae-solo", "solo-global":
		return "global-solo"
	case "cn", "cn-trae", "trae-cn":
		return "cn"
	case "cn-solo", "solo-cn", "trae-solo-cn":
		return "cn-solo"
	default:
		return strings.ToLower(strings.TrimSpace(raw))
	}
}

func (c *Config) OAuthIsCN() bool   { return strings.HasPrefix(c.OAuthPlatform, "cn") }
func (c *Config) OAuthIsSolo() bool { return strings.HasSuffix(c.OAuthPlatform, "solo") }

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
	switch c.OAuthPlatform {
	case "global", "global-solo", "cn", "cn-solo":
	default:
		return fmt.Errorf("TRAE_OAUTH_PLATFORM must be global, global-solo, cn, or cn-solo")
	}
	switch c.ReasoningMode {
	case "preserve", "merge", "drop":
	default:
		return fmt.Errorf("TRAE_REASONING_MODE must be preserve, merge, or drop")
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
	if len(c.OAuthGuidanceURLs) == 0 {
		return fmt.Errorf("TRAE_OAUTH_GUIDANCE_URLS must contain at least one endpoint")
	}
	for _, endpoint := range c.OAuthGuidanceURLs {
		if err := validateAbsoluteHTTPURL("TRAE_OAUTH_GUIDANCE_URLS", endpoint); err != nil {
			return err
		}
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
	if strings.TrimSpace(c.OAuthClientID) == "" {
		return fmt.Errorf("TRAE_OAUTH_CLIENT_ID must not be empty")
	}
	if c.MaxBodyBytes <= 0 {
		return fmt.Errorf("MAX_BODY_BYTES must be greater than zero")
	}
	if strings.TrimSpace(c.ModelStatusFile) == "" {
		return fmt.Errorf("TRAE_MODEL_STATUS_FILE must not be empty")
	}
	if c.RequestTimeout <= 0 || c.HeaderTimeout <= 0 || c.ShutdownTimeout <= 0 || c.ModelCacheTTL <= 0 || c.ModelFailureTTL <= 0 || c.ModelSuccessTTL <= 0 || c.OAuthRefreshSkew <= 0 || c.OAuthRefreshEvery <= 0 || c.OAuthLoginTTL <= 0 {
		return fmt.Errorf("timeout values, refresh intervals, and model cache TTLs must be greater than zero")
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

func parseURLList(raw string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, item := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' || r == '\n' }) {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func defaultDeviceType() string {
	switch runtime.GOOS {
	case "darwin":
		return "mac"
	case "windows":
		return "windows"
	case "linux":
		return "linux"
	default:
		return runtime.GOOS
	}
}

func defaultDeviceBrand() string {
	switch runtime.GOOS {
	case "darwin":
		return "Apple"
	case "windows":
		return "Microsoft"
	case "linux":
		return "Linux"
	default:
		return DefaultDeviceBrand
	}
}

func defaultOSVersion() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	default:
		return runtime.GOOS
	}
}

type installedProduct struct {
	PluginVersion string
	AppVersion    string
	ClientID      string
}

func firstConfigured(envKey string, values ...string) string {
	if raw := strings.TrimSpace(os.Getenv(envKey)); raw != "" {
		return raw
	}
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func detectInstalledProduct(platform string) installedProduct {
	for _, candidate := range installedProductCandidates(platform) {
		raw, err := os.ReadFile(candidate)
		if err != nil {
			continue
		}
		var root map[string]any
		if json.Unmarshal(raw, &root) != nil {
			continue
		}
		product := parseInstalledProduct(root, platform)
		if product.PluginVersion != "" || product.AppVersion != "" || product.ClientID != "" {
			return product
		}
	}
	return installedProduct{}
}

func installedProductCandidates(platform string) []string {
	var candidates []string
	if configured := strings.TrimSpace(os.Getenv("TRAE_APP_PATH")); configured != "" {
		candidates = append(candidates, productJSONCandidates(configured)...)
	}
	if runtime.GOOS == "windows" {
		for _, root := range []string{os.Getenv("LOCALAPPDATA"), os.Getenv("ProgramFiles")} {
			if root == "" {
				continue
			}
			for _, name := range []string{"Trae", "TRAE", "Trae CN", "TRAE SOLO"} {
				base := filepath.Join(root, "Programs", name)
				if root == os.Getenv("ProgramFiles") {
					base = filepath.Join(root, name)
				}
				candidates = append(candidates, productJSONCandidates(base)...)
			}
		}
	} else if runtime.GOOS == "darwin" {
		for _, name := range []string{"Trae.app", "TRAE.app", "TRAE SOLO.app"} {
			candidates = append(candidates, filepath.Join("/Applications", name, "Contents", "Resources", "app", "product.json"))
		}
	} else {
		for _, base := range []string{"/opt/Trae", "/opt/trae", "/usr/share/trae", "/usr/lib/trae"} {
			candidates = append(candidates, productJSONCandidates(base)...)
		}
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		candidate = filepath.Clean(candidate)
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		out = append(out, candidate)
	}
	_ = platform
	return out
}

func productJSONCandidates(base string) []string {
	return []string{
		filepath.Join(base, "resources", "app", "product.json"),
		filepath.Join(base, "Resources", "app", "product.json"),
		filepath.Join(base, "Contents", "Resources", "app", "product.json"),
		filepath.Join(base, "product.json"),
	}
}

func parseInstalledProduct(root map[string]any, platform string) installedProduct {
	product := installedProduct{
		PluginVersion: mapString(root, "tronBuildVersion", "buildVersion", "productVersion", "version"),
		AppVersion:    mapString(root, "appVersion", "productVersion", "version"),
	}
	quality := strings.ToLower(mapString(root, "quality"))
	if quality == "" {
		quality = DefaultIDEVersionType
	}
	group := "TRAE"
	if strings.HasSuffix(platform, "solo") {
		group = "SOLO"
	}
	if icube, ok := root["iCubeApp"].(map[string]any); ok {
		if authConfig, ok := icube["authConfig"].(map[string]any); ok {
			if groupConfig, ok := authConfig[group].(map[string]any); ok {
				product.ClientID = firstNonEmptyMap(groupConfig, quality, DefaultIDEVersionType)
			}
		}
	}
	return product
}

func mapString(root map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := root[key]; ok {
			if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
	}
	return ""
}

func firstNonEmptyMap(root map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := root[key].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	for _, value := range root {
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
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
