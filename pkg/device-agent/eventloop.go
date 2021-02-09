package device_agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gen2brain/beeep"
	"github.com/nais/device/device-agent/apiserver"
	"github.com/nais/device/device-agent/auth"
	"github.com/nais/device/device-agent/runtimeconfig"
	"github.com/nais/device/pkg/pb"
	"github.com/nais/device/pkg/version"
	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	lastConfigurationFile string
)

const (
	versionCheckInterval      = 1 * time.Hour
	syncConfigInterval        = 5 * time.Minute
	initialGatewayRefreshWait = 2 * time.Second
	initialConnectWait        = initialGatewayRefreshWait
	healthCheckInterval       = 20 * time.Second
	versionCheckTimeout       = 3 * time.Second
	agentHelperCallTimeout    = 3 * time.Second
)

func (das *DeviceAgentServer) ConfigureHelper(rc *runtimeconfig.RuntimeConfig, gateways []*pb.Gateway) error {
	return das.HelperConfigStream.Send(&pb.Configuration{
		PrivateKey: string(rc.PrivateKey),
		DeviceIP:   rc.BootstrapConfig.DeviceIP,
		Gateways:   gateways,
	})
}

func (das *DeviceAgentServer) EventLoop(rc *runtimeconfig.RuntimeConfig) {
	var err error

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

	syncConfigTicker := time.NewTicker(syncConfigInterval)
	healthCheckTicker := time.NewTicker(healthCheckInterval)
	versionCheckTicker := time.NewTicker(5 * time.Second)

	status := &pb.AgentStatus{}
	das.stateChange <- status.ConnectionState

	for {
		das.UpdateAgentStatus(status)

		select {
		case sig := <-signals:
			log.Infof("Received signal %s, exiting...", sig)
			return

		case <-versionCheckTicker.C:
			ctx, cancel := context.WithTimeout(context.Background(), versionCheckTimeout)
			status.NewVersionAvailable, err = newVersionAvailable(ctx)
			cancel()

			if err != nil {
				log.Errorf("check for new version: %s", err)
				continue
			}

			if status.NewVersionAvailable {
				notify("New version of device agent available: https://doc.nais.io/device/install#installation")
				versionCheckTicker.Stop()
			} else {
				versionCheckTicker.Reset(versionCheckInterval)
			}

		case <-syncConfigTicker.C:
			if status.ConnectionState == pb.AgentState_Connected {
				das.stateChange <- pb.AgentState_SyncConfig
			}

		case <-healthCheckTicker.C:
			if status.ConnectionState == pb.AgentState_Connected {
				das.stateChange <- pb.AgentState_HealthCheck
			}

		case status.ConnectionState = <-das.stateChange:
			log.Infof("state changed to %s", status.ConnectionState)

			switch status.ConnectionState {
			case pb.AgentState_Bootstrapping:
				if rc.BootstrapConfig != nil {
					log.Infof("Already bootstrapped")
				} else {
					ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
					rc.BootstrapConfig, err = runtimeconfig.EnsureBootstrapping(rc, ctx)
					cancel()
					if err != nil {
						notify(fmt.Sprintf("Error during bootstrap: %v", err))
						das.stateChange <- pb.AgentState_Disconnecting
						continue
					}
				}

				das.HelperConfigStream, err = das.DeviceHelper.Configure(context.Background())
				if err != nil {
					notify(err.Error())
					das.stateChange <- pb.AgentState_Disconnecting
					continue
				}

				err = das.ConfigureHelper(rc, []*pb.Gateway{
					rc.BootstrapConfig.Gateway(),
				})

				if err != nil {
					notify(err.Error())
					das.stateChange <- pb.AgentState_Disconnecting
					continue
				}

				das.stateChange <- pb.AgentState_Authenticating

			case pb.AgentState_Authenticating:
				if rc.SessionInfo != nil && !rc.SessionInfo.Expired() {
					log.Infof("Already have a valid session")
				} else {
					log.Infof("No valid session, authenticating")
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
					rc.SessionInfo, err = auth.EnsureAuth(rc.SessionInfo, ctx, rc.Config.APIServer, rc.Config.Platform, rc.Serial)
					cancel()
					if err != nil {
						notify(fmt.Sprintf("Error during authentication: %v", err))

						log.Errorf("Authenticating with apiserver: %v", err)
						das.stateChange <- pb.AgentState_Disconnecting
						continue
					}
				}
				status.ConnectedSince = timestamppb.Now()
				das.stateChange <- pb.AgentState_SyncConfig
				continue

			case pb.AgentState_Connected:
				// noop

			case pb.AgentState_Disconnected:
				status.Gateways = make([]*pb.Gateway, 0)

			case pb.AgentState_Quitting:
				log.Info("closing device-helper configuration stream")
				_, err = das.HelperConfigStream.CloseAndRecv()
				status.Gateways = make([]*pb.Gateway, 0)
				return

			case pb.AgentState_Disconnecting:
				log.Info("closing device-helper configuration stream")
				_, err = das.HelperConfigStream.CloseAndRecv()
				if err != nil {
					notify(err.Error())
				}
				das.stateChange <- pb.AgentState_Disconnected

			case pb.AgentState_HealthCheck:
				for _, gw := range rc.GetGateways() {
					err := ping(gw.IP)
					if err == nil {
						gw.Healthy = true
						log.Debugf("Successfully pinged gateway %v with ip: %v", gw.Name, gw.IP)
					} else {
						gw.Healthy = false
						log.Infof("unable to ping host %s: %v", gw.IP, err)
					}
				}
				das.stateChange <- pb.AgentState_Connected
				status.Gateways = ApiGatewaysToProtobufGateways(rc.Gateways)
				das.UpdateAgentStatus(status)
				// trigger configuration save here if health checks are supposed to alter routes

			case pb.AgentState_SyncConfig:
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				gateways, err := apiserver.GetDeviceConfig(rc.SessionInfo.Key, rc.Config.APIServer, ctx)
				cancel()

				switch {
				case errors.Is(err, &apiserver.UnauthorizedError{}):
					log.Errorf("Unauthorized access from apiserver: %v", err)
					log.Errorf("Assuming invalid session; disconnecting.")
					rc.SessionInfo = nil
					das.stateChange <- pb.AgentState_Disconnecting
					continue

				case errors.Is(err, &apiserver.UnhealthyError{}):
					// TODO produce unhealthy status message to "even watcher" stream

					log.Errorf("Device is not healthy: %v", err)
					// TODO consider moving all notify calls to systray code
					notify("No access as your device is unhealthy. Run '/msg @Kolide status' on Slack and fix the errors")
					das.stateChange <- pb.AgentState_Unhealthy
					continue

				case err != nil:
					log.Errorf("Unable to get gateway config: %v", err)
					das.stateChange <- pb.AgentState_HealthCheck
					continue
				}

				rc.UpdateGateways(gateways)
				status.Gateways = ApiGatewaysToProtobufGateways(rc.Gateways)

				err = das.ConfigureHelper(rc, status.GetGateways())
				if err != nil {
					log.Error(err.Error())
					notify(err.Error())
					das.stateChange <- pb.AgentState_Disconnecting
				} else {
					das.stateChange <- pb.AgentState_HealthCheck
				}

			case pb.AgentState_Unhealthy:
			}
		}
	}
}

func ApiGatewaysToProtobufGateways(apigws apiserver.Gateways) []*pb.Gateway {
	var pbgws []*pb.Gateway

	for _, apigw := range apigws {
		pbgws = append(pbgws, &pb.Gateway{
			Name:                     apigw.Name,
			Healthy:                  apigw.Healthy,
			PublicKey:                apigw.PublicKey,
			Endpoint:                 apigw.Endpoint,
			Ip:                       apigw.IP,
			Routes:                   apigw.Routes,
			RequiresPrivilegedAccess: apigw.RequiresPrivilegedAccess,
			AccessGroupIDs:           nil,
		})
	}

	return pbgws
}

func newVersionAvailable(ctx context.Context) (bool, error) {
	type response struct {
		Tag string `json:"tag_name"`
	}

	log.Info("Checking release version on github")

	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/repos/nais/device/releases/latest", nil)
	if err != nil {
		return false, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("retrieve current release version: %s", err)
	}

	defer resp.Body.Close()
	res := &response{}
	decoder := json.NewDecoder(resp.Body)
	err = decoder.Decode(res)
	if err != nil {
		return false, fmt.Errorf("unmarshal response: %s", err)
	}

	if version.Version != res.Tag {
		return true, nil
	}

	return false, nil
}

func ping(addr string) error {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%s", addr, "3000"), 2*time.Second)
	if err != nil {
		return err
	}
	c.Close()

	return nil
}

func notify(format string, args ...interface{}) {
	message := fmt.Sprintf(format, args...)
	err := beeep.Notify("NAIS device", message, "../Resources/nais-logo-red.png")
	log.Infof("sending message to notification centre: %s", message)
	if err != nil {
		log.Errorf("failed sending message due to error: %s", err)
	}
}
