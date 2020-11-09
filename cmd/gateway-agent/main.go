package main

import (
	"encoding/json"
	"fmt"
	"github.com/nais/device/gateway-agent/config"
	"github.com/nais/device/gateway-agent/prometheus"
	"io/ioutil"
	"net/http"
	"os/exec"
	"path"
	"regexp"
	"time"

	"github.com/nais/device/pkg/logger"

	"github.com/coreos/go-iptables/iptables"
	"github.com/nais/device/pkg/version"
	log "github.com/sirupsen/logrus"
	flag "github.com/spf13/pflag"
)

var (
	cfg = config.DefaultConfig()
)

func init() {
	flag.StringVar(&cfg.Name, "name", cfg.Name, "gateway name")
	flag.StringVar(&cfg.ConfigDir, "config-dir", cfg.ConfigDir, "gateway-agent config directory")
	flag.StringVar(&cfg.TunnelIP, "tunnel-ip", cfg.TunnelIP, "gateway tunnel ip")
	flag.StringVar(&cfg.PrometheusAddr, "prometheus-address", cfg.PrometheusAddr, "prometheus listen address")
	flag.StringVar(&cfg.APIServerURL, "api-server-url", cfg.APIServerURL, "api server URL")
	flag.StringVar(&cfg.APIServerPublicKey, "api-server-public-key", cfg.APIServerPublicKey, "api server public key")
	flag.StringVar(&cfg.APIServerWireGuardEndpoint, "api-server-wireguard-endpoint", cfg.APIServerWireGuardEndpoint, "api server WireGuard endpoint")
	flag.StringVar(&cfg.PrometheusPublicKey, "prometheus-public-key", cfg.PrometheusPublicKey, "prometheus public key")
	flag.StringVar(&cfg.PrometheusTunnelIP, "prometheus-tunnel-ip", cfg.PrometheusTunnelIP, "prometheus tunnel ip")
	flag.BoolVar(&cfg.DevMode, "development-mode", cfg.DevMode, "development mode avoids setting up interface and configuring WireGuard")
	flag.StringVar(&cfg.LogLevel, "log-level", "info", "log level")

	flag.Parse()

	logger.Setup(cfg.LogLevel)
	cfg.WireGuardConfigPath = path.Join(cfg.ConfigDir, "wg0.conf")
	cfg.PrivateKeyPath = path.Join(cfg.ConfigDir, "private.key")
	cfg.APIServerPasswordPath = path.Join(cfg.ConfigDir, "apiserver_password")
	log.Infof("Version: %s, Revision: %s", version.Version, version.Revision)

	prometheus.InitializeMetrics(cfg.Name, version.Version)
	prometheus.Serve(cfg.PrometheusAddr)
}

type GatewayConfig struct {
	Devices []Device `json:"devices"`
	Routes  []string `json:"routes"`
}

type Device struct {
	PSK       string `json:"psk"`
	PublicKey string `json:"publicKey"`
	IP        string `json:"ip"`
}

func main() {
	if err := cfg.InitLocalConfig(); err != nil {
		log.Fatalf("Initializing local configuration: %v", err)
	}

	log.Info("starting gateway-agent")

	if !cfg.DevMode {
		if err := setupInterface(cfg.TunnelIP); err != nil {
			log.Fatalf("setting up interface: %v", err)
		}
		var err error
		cfg.IPTables, err = iptables.New()
		if err != nil {
			log.Fatalf("setting up iptables %v", err)
		}

		err = setupIptables(cfg)
		if err != nil {
			log.Fatalf("Setting up iptables defaults: %v", err)
		}
	} else {
		log.Infof("Skipping interface setup")
	}

	baseConfig := GenerateBaseConfig(cfg)

	if err := actuateWireGuardConfig(baseConfig, cfg.WireGuardConfigPath); err != nil && !cfg.DevMode {
		log.Fatalf("actuating base config: %v", err)
	}

	for range time.NewTicker(10 * time.Second).C {
		log.Infof("getting config")
		gatewayConfig, err := getGatewayConfig(cfg)
		if err != nil {
			log.Error(err)
			prometheus.FailedConfigFetches.Inc()
			continue
		}

		err = updateConnectedDevicesMetrics(cfg)
		if err != nil {
			log.Errorf("Unable to execute command: %v", err)
		}

		prometheus.LastSuccessfulConfigFetch.SetToCurrentTime()

		log.Debugf("%+v\n", gatewayConfig)

		// skip side-effects for local development
		if cfg.DevMode {
			continue
		}

		peerConfig := GenerateWireGuardPeers(gatewayConfig.Devices)
		if err := actuateWireGuardConfig(baseConfig+peerConfig, cfg.WireGuardConfigPath); err != nil {
			log.Errorf("actuating WireGuard config: %v", err)
		}

		err = forwardRoutes(cfg, gatewayConfig.Routes)
		if err != nil {
			log.Errorf("forwarding routes: %v", err)
		}
	}
}

func getGatewayConfig(config config.Config) (*GatewayConfig, error) {
	gatewayConfigURL := fmt.Sprintf("%s/gatewayconfig", config.APIServerURL)
	req, err := http.NewRequest(http.MethodGet, gatewayConfigURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating http request: %w", err)
	}

	req.SetBasicAuth(config.Name, config.APIServerPassword)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("getting peer config from apiserver: %w", err)
	}

	defer resp.Body.Close()

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading bytes, %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching gatewayConfig from apiserver: %v %v %v", resp.StatusCode, resp.Status, string(b))
	}

	var gatewayConfig GatewayConfig
	err = json.Unmarshal(b, &gatewayConfig)
	if err != nil {
		return nil, fmt.Errorf("unmarshal json from apiserver: bytes: %v, error: %w", string(b), err)
	}

	prometheus.RegisteredDevices.Set(float64(len(gatewayConfig.Devices)))

	return &gatewayConfig, nil
}

func setupInterface(tunnelIP string) error {
	if err := exec.Command("ip", "link", "del", "wg0").Run(); err != nil {
		log.Infof("pre-deleting WireGuard interface (ok if this fails): %v", err)
	}

	run := func(commands [][]string) error {
		for _, s := range commands {
			cmd := exec.Command(s[0], s[1:]...)

			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("running %v: %w: %v", cmd, err, string(out))
			} else {
				log.Debugf("%v: %v\n", cmd, string(out))
			}
		}
		return nil
	}

	commands := [][]string{
		{"ip", "link", "add", "dev", "wg0", "type", "wireguard"},
		{"ip", "link", "set", "wg0", "mtu", "1360"},
		{"ip", "address", "add", "dev", "wg0", tunnelIP + "/21"},
		{"ip", "link", "set", "wg0", "up"},
	}

	return run(commands)
}

func GenerateBaseConfig(cfg config.Config) string {
	template := `[Interface]
PrivateKey = %s
ListenPort = 51820

[Peer] # apiserver
PublicKey = %s
AllowedIPs = %s/32
Endpoint = %s

[Peer] # prometheus
PublicKey = %s
AllowedIPs = %s/32

`

	return fmt.Sprintf(template, cfg.PrivateKey, cfg.APIServerPublicKey, cfg.APIServerTunnelIP, cfg.APIServerWireGuardEndpoint, cfg.PrometheusPublicKey, cfg.PrometheusTunnelIP)
}

func GenerateWireGuardPeers(devices []Device) string {
	peerTemplate := `[Peer]
PublicKey = %s
AllowedIPs = %s
`
	var peers string

	for _, device := range devices {
		peers += fmt.Sprintf(peerTemplate, device.PublicKey, device.IP)
	}

	return peers
}

func updateConnectedDevicesMetrics(cfg config.Config) error {
	if cfg.DevMode {
		prometheus.ConnectedDevices.Set(1337)
		return nil
	}

	output, err := exec.Command("wg", "show", "wg0", "endpoints").Output()
	if err != nil {
		return err
	}
	re := regexp.MustCompile(`\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}:\d{1,5}`)
	matches := re.FindAll(output, -1)

	numConnectedDevices := float64(len(matches))
	prometheus.ConnectedDevices.Set(numConnectedDevices)
	return nil
}

// actuateWireGuardConfig runs syncconfig with the provided WireGuard config
func actuateWireGuardConfig(wireGuardConfig, wireGuardConfigPath string) error {
	if err := ioutil.WriteFile(wireGuardConfigPath, []byte(wireGuardConfig), 0600); err != nil {
		return fmt.Errorf("writing WireGuard config to disk: %w", err)
	}

	cmd := exec.Command("wg", "syncconf", "wg0", wireGuardConfigPath)

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running syncconf: %w", err)
	}

	log.Debugf("Actuated WireGuard config: %v", wireGuardConfigPath)

	return nil
}

func setupIptables(cfg config.Config) error {
	err := cfg.IPTables.ChangePolicy("filter", "FORWARD", "DROP")
	if err != nil {
		return fmt.Errorf("setting FORWARD policy to DROP: %w", err)
	}

	// Allow ESTABLISHED,RELATED from wg0 to default interface
	err = cfg.IPTables.AppendUnique("filter", "FORWARD", "-i", "wg0", "-o", cfg.DefaultInterface, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT")
	if err != nil {
		return fmt.Errorf("adding default FORWARD outbound-rule: %w", err)
	}

	// Allow ESTABLISHED,RELATED from default interface to wg0
	err = cfg.IPTables.AppendUnique("filter", "FORWARD", "-i", cfg.DefaultInterface, "-o", "wg0", "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT")
	if err != nil {
		return fmt.Errorf("adding default FORWARD inbound-rule: %w", err)
	}

	// Create and set up LOG_ACCEPT CHAIN
	err = cfg.IPTables.NewChain("filter", "LOG_ACCEPT")
	if err != nil {
		log.Infof("Creating LOG_ACCEPT chain (probably already exist), error: %v", err)
	}
	err = cfg.IPTables.AppendUnique("filter", "LOG_ACCEPT", "-j", "LOG", "--log-prefix", "naisdevice-fwd: ", "--log-level", "6")
	if err != nil {
		return fmt.Errorf("adding default LOG_ACCEPT log-rule: %w", err)
	}
	err = cfg.IPTables.AppendUnique("filter", "LOG_ACCEPT", "-j", "ACCEPT")
	if err != nil {
		return fmt.Errorf("adding default LOG_ACCEPT accept-rule: %w", err)
	}

	return nil
}

func forwardRoutes(cfg config.Config, routes []string) error {
	var err error

	for _, ip := range routes {
		err = cfg.IPTables.AppendUnique("nat", "POSTROUTING", "-o", cfg.DefaultInterface, "-p", "tcp", "-d", ip, "-j", "SNAT", "--to-source", cfg.DefaultInterfaceIP)
		if err != nil {
			return fmt.Errorf("setting up snat: %w", err)
		}

		err = cfg.IPTables.AppendUnique(
			"filter",
			"FORWARD",
			"--in-interface", "wg0",
			"--out-interface", cfg.DefaultInterface,
			"--protocol", "tcp",
			"--syn",
			"--destination", ip,
			"--match", "conntrack",
			"--ctstate", "NEW",
			"--jump", "LOG_ACCEPT",
		)
		if err != nil {
			return fmt.Errorf("setting up iptables log rule: %w", err)
		}
	}

	return nil
}
