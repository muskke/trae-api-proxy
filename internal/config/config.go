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
	DefaultTraeBaseURL     = "https://trae-api-cn.mchost.guru"
	DefaultAppID           = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	DefaultIDEVersion      = "0.1.52"
	DefaultIDEVersionCode  = "20260811"
	DefaultIDEVersionType  = "stable"
	DefaultDeviceBrand     = "83DG"
	DefaultDeviceType      = "windows"
	DefaultOSVersion       = "Windows 11 Pro"
	DefaultUpstreamMode    = "auto"
	DefaultModel           = "glm-5.2"
	DefaultRequestTraffic  = "prod"
	DefaultApplicationVer  = "default"
	defaultRequestTimeout  = 120 * time.Second
	defaultHeaderTimeout   = 120 * time.Second
	defaultShutdownTimeout = 10 * time.Second
	defaultModelCacheTTL   = time.Hour
	defaultMaxBodyBytes    = int64(8 << 20)
)

type Config struct {
	AuthToken          string
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
}

func Load() *Config {
	// A local .env is convenient for this single-binary proxy. Environment
	// variables still win, which keeps container and service deployments clean.
	_ = utils.LoadEnvFile(".env")

	return &Config{
		AuthToken:          os.Getenv("AUTH_TOKEN"),
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
		Port:               utils.EnvOrDefault("PORT", "8000"),
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
	}
}

func (c *Config) ListenAddr() string {
	return net.JoinHostPort(c.Bind, c.Port)
}

func (c *Config) Validate() error {
	switch c.UpstreamMode {
	case "auto", "solo", "legacy":
	default:
		return fmt.Errorf("TRAE_UPSTREAM_MODE must be auto, solo, or legacy")
	}

	u, err := url.Parse(c.APIBaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("TRAE_API_BASE_URL must be an absolute http(s) URL")
	}
	port, err := strconv.Atoi(c.Port)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("PORT must be between 1 and 65535")
	}
	if c.AuthToken != "" && c.IdeToken == "" {
		return fmt.Errorf("TRAE_IDE_TOKEN is required when AUTH_TOKEN is configured")
	}
	if c.MaxBodyBytes <= 0 {
		return fmt.Errorf("MAX_BODY_BYTES must be greater than zero")
	}
	if c.RequestTimeout <= 0 || c.HeaderTimeout <= 0 || c.ShutdownTimeout <= 0 || c.ModelCacheTTL <= 0 {
		return fmt.Errorf("timeout values and TRAE_MODEL_CACHE_TTL must be greater than zero")
	}
	return nil
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
