package config

import (
	"path/filepath"

	"github.com/nais/device/pkg/config"
	log "github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/endpoints"
)

type Config struct {
	APIServer                string
	Interface                string
	ConfigDir                string
	BootstrapToken           string
	WireGuardBinary          string
	WireGuardGoBinary        string
	PrivateKeyPath           string
	WireGuardConfigPath      string
	BootstrapConfigPath      string
	SerialPath               string
	LogLevel                 string
	LogFilePath              string
	OAuth2Config             oauth2.Config
	Platform                 string
	BootstrapAPI             string
	AutoConnect              bool
	GrpcAddress              string
	DeviceAgentHelperAddress string
}

func (c *Config) SetDefaults() {
	c.Platform = Platform
	c.SetPlatformDefaults()
	c.PrivateKeyPath = filepath.Join(c.ConfigDir, "private.key")
	c.WireGuardConfigPath = filepath.Join(c.ConfigDir, c.Interface+".conf")
	c.BootstrapConfigPath = filepath.Join(c.ConfigDir, "bootstrapconfig.json")
	c.SerialPath = filepath.Join(c.ConfigDir, "product_serial")
}

func DefaultConfig() Config {
	userConfigDir, err := config.UserConfigDir()
	if err != nil {
		log.Fatal("Getting user config dir: %w", err)
	}

	return Config{
		APIServer:                "http://10.255.240.1",
		BootstrapAPI:             "https://bootstrap.device.nais.io",
		ConfigDir:                userConfigDir,
		LogLevel:                 "info",
		GrpcAddress:              filepath.Join(userConfigDir, "agent.sock"),
		DeviceAgentHelperAddress: filepath.Join(userConfigDir, "helper.sock"),
		OAuth2Config: oauth2.Config{
			ClientID:    "8086d321-c6d3-4398-87da-0d54e3d93967",
			Scopes:      []string{"openid", "6e45010d-2637-4a40-b91d-d4cbb451fb57/.default", "offline_access"},
			Endpoint:    endpoints.AzureAD("62366534-1ec3-4962-8869-9b5535279d0b"),
			RedirectURL: "http://localhost:51800",
		},
	}
}
