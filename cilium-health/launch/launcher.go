// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package launch

import (
	"errors"
	"fmt"
	"net/http"
	"os"

	healthApi "github.com/cilium/cilium/api/v1/health/server"
	"github.com/cilium/cilium/api/v1/models"
	"github.com/cilium/cilium/pkg/api"
	ciliumPkg "github.com/cilium/cilium/pkg/client"
	"github.com/cilium/cilium/pkg/health/client"
	"github.com/cilium/cilium/pkg/health/defaults"
	"github.com/cilium/cilium/pkg/health/server"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/logging"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/time"
)

// CiliumHealth launches and polls the cilium-health daemon
type CiliumHealth struct {
	mutex  lock.RWMutex
	server *server.Server
	client *client.Client
	status *models.Status
}

var log = logging.DefaultLogger.WithField(logfields.LogSubsys, "cilium-health-launcher")

const (
	serverProbeDeadline  = 10 * time.Second
	connectRetryInterval = 1 * time.Second
	statusProbeInterval  = 5 * time.Second
)

// Launch starts the cilium-health server and returns a handle to obtain its status
func Launch(spec *healthApi.Spec, initialized <-chan struct{}) (*CiliumHealth, error) {
	var (
		err error
		ch  = &CiliumHealth{}
	)

	config := server.Config{
		CiliumURI:     ciliumPkg.DefaultSockPath(),
		Debug:         option.Config.Opts.IsEnabled(option.Debug),
		ICMPReqsCount: option.Config.HealthCheckICMPFailureThreshold,
		ProbeDeadline: serverProbeDeadline,
		HTTPPathPort:  option.Config.ClusterHealthPort,
		HealthAPISpec: spec,
	}

	ch.server, err = server.NewServer(config)
	if err != nil {
		return nil, fmt.Errorf("failed to instantiate cilium-health server: %w", err)
	}

	ch.client, err = client.NewDefaultClient()
	if err != nil {
		return nil, fmt.Errorf("failed to instantiate cilium-health client: %w", err)
	}

	go ch.runServer(initialized)

	return ch, nil
}

func (ch *CiliumHealth) runServer(initialized <-chan struct{}) {
	// Wait until the agent has initialized sufficiently
	<-initialized

	// Wait until Cilium API is available
	for {
		cli, err := ciliumPkg.NewDefaultClient()
		if err == nil {
			// Making sure that we can talk with the daemon.
			if _, err = cli.Daemon.GetHealthz(nil); err == nil {
				break
			}
		}
		log.WithError(err).Debugf("Cannot establish connection to local cilium instance")
		time.Sleep(connectRetryInterval)
	}

	// Launch cilium-health API server
	os.Remove(defaults.SockPath)
	go func() {
		defer ch.server.Shutdown()
		if err := ch.server.Serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.WithError(err).Error("Failed to serve cilium-health API")
		}
	}()

	// When the unix socket is made available, set its permissions.
	scopedLog := log.WithField(logfields.Path, defaults.SockPath)
	for {
		_, err := os.Stat(defaults.SockPath)
		if err == nil {
			break
		}
		scopedLog.WithError(err).Debugf("Cannot find socket")
		time.Sleep(1 * time.Second)
	}
	if err := api.SetDefaultPermissions(logging.DefaultSlogLogger, defaults.SockPath); err != nil {
		scopedLog.WithError(err).Fatal("Cannot set default permissions on socket")
	}

	// Periodically fetch status from cilium-health server
	for {
		status := &models.Status{
			State: models.StatusStateOk,
		}

		_, err := ch.client.Restapi.GetHealthz(nil)
		if err != nil {
			status.Msg = err.Error()
			status.State = models.StatusStateWarning
		}

		ch.setStatus(status)
		time.Sleep(statusProbeInterval)
	}
}

// GetStatus returns the status of the cilium-health daemon.
func (ch *CiliumHealth) GetStatus() *models.Status {
	ch.mutex.RLock()
	status := ch.status
	ch.mutex.RUnlock()
	return status
}

// setStatus updates the status of the cilium-health daemon.
func (ch *CiliumHealth) setStatus(status *models.Status) {
	ch.mutex.Lock()
	ch.status = status
	ch.mutex.Unlock()
}
