// +build linux darwin

package wireguard

import (
	"fmt"

	"github.com/nais/device/device-agent/config"
)

func GenerateBaseConfig(bootstrapConfig *config.BootstrapConfig, privateKey []byte) string {
	template := `[Interface]
PrivateKey = %s

[Peer]
PublicKey = %s
AllowedIPs = %s/32
Endpoint = %s
`
	return fmt.Sprintf(template, KeyToBase64(privateKey), bootstrapConfig.PublicKey, bootstrapConfig.APIServerIP, bootstrapConfig.Endpoint)
}
