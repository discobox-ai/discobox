// Command discobox-nri-ca is the sandbox's NRI plugin: it mounts the
// sandbox's MITM CA trust bundles and injects proxy-trust env vars into
// every container a nested Docker daemon creates. See
// docs/adr/0015-nested-docker-builds-trust-the-mitm-proxy-via-nri.md.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/containerd/nri/pkg/stub"
	"github.com/obot-platform/discobox/sandbox-agent/config"
	"github.com/obot-platform/discobox/sandbox-agent/nrica"
)

// reconnectBackoff is how long to wait before retrying plugin registration
// after containerd drops the connection (e.g. a containerd restart).
const reconnectBackoff = 5 * time.Second

func main() {
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	logger := slog.Default()
	plugin, err := nrica.New(logger, config.DefaultPath, nrica.DefaultCABundleDir)
	if err != nil {
		logger.Error("load nri plugin", "error", err)
		os.Exit(1)
	}

	for ctx.Err() == nil {
		nriStub, err := stub.New(plugin, stub.WithPluginName("discobox-ca"))
		if err != nil {
			logger.Error("create nri stub", "error", err)
			os.Exit(1)
		}
		if err := nriStub.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Warn("nri plugin disconnected, retrying", "error", err)
			select {
			case <-ctx.Done():
			case <-time.After(reconnectBackoff):
			}
		}
	}
}
