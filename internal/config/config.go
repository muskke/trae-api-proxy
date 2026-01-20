package config

import (
	"os"

	"github.com/muskke/trae-api-proxy/pkg/utils"
)

const (
	DefaultTraeBaseURL = "https://trae-api-sg.mchost.guru"
)

type Config struct {
	AppID          string
	DeviceBrand    string
	DeviceCPU      string
	DeviceID       string
	DeviceType     string
	IDEVersion     string
	IDEVersionCode string
	IDEVersionType string
	MachineID      string
	OSVersion      string
	APIBaseURL     string
	Port           string
	Locale         string
}

func Load() *Config {
	// Try loading .env first, ignore error if not found
	_ = utils.LoadEnvFile(".env")

	return &Config{
		AppID:          os.Getenv("TRAE_APP_ID"),
		DeviceBrand:    os.Getenv("TRAE_DEVICE_BRAND"),
		DeviceCPU:      os.Getenv("TRAE_DEVICE_CPU"),
		DeviceID:       os.Getenv("TRAE_DEVICE_ID"),
		DeviceType:     os.Getenv("TRAE_DEVICE_TYPE"),
		IDEVersion:     os.Getenv("TRAE_IDE_VERSION"),
		IDEVersionCode: os.Getenv("TRAE_IDE_VERSION_CODE"),
		IDEVersionType: os.Getenv("TRAE_IDE_VERSION_TYPE"),
		MachineID:      os.Getenv("TRAE_MACHINE_ID"),
		OSVersion:      os.Getenv("TRAE_OS_VERSION"),
		APIBaseURL:     utils.EnvOrDefault("TRAE_API_BASE_URL", DefaultTraeBaseURL),
		Port:           utils.EnvOrDefault("PORT", "8000"),
		Locale:         utils.EnvOrDefault("TRAE_LOCALE", "zh-cn"),
	}
}
