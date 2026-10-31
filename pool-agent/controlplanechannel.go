package poolagent

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/discobox-ai/discobox/pool-agent/cpchannel"
)

// firstChannelWait bounds how long a starting agent waits for the server to
// open its control-plane channel. The server dials as soon as the pool's box
// is made and redials with backoff, so a channel that has not arrived by
// then is a control plane that cannot reach this pool, which fails the boot
// as an unreachable control plane does, and systemd tries it again.
const firstChannelWait = 3 * time.Minute

// openControlPlaneChannel is the pool end of the control-plane channel of a
// pool whose bootstrap says the server opens one (ADR 26-10-30-875 §§1–2),
// serving ControlPlaneChannelSocket to the agent and the proxy unit, which
// reach the control plane there. A pool that dials its control plane has
// none, and gets nil.
func openControlPlaneChannel(ctx context.Context, logger *slog.Logger, bootstrap Bootstrap, identity *Identity) (*cpchannel.Channel, error) {
	if !bootstrap.ControlPlaneChannel {
		return nil, nil
	}
	controlPlane, err := decodeControlPlaneKey(bootstrap.ControlPlaneKey)
	if err != nil {
		return nil, err
	}
	channel, err := cpchannel.New(logger, identity.PrivateKey, controlPlane)
	if err != nil {
		return nil, err
	}
	listener, err := listenProxySocket(ControlPlaneChannelSocket)
	if err != nil {
		return nil, fmt.Errorf("listen for the control plane on %s: %w", ControlPlaneChannelSocket, err)
	}
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	go func() {
		if err := channel.Serve(ctx, listener); err != nil {
			logger.Error("control-plane socket ended", "error", err)
		}
	}()
	return channel, nil
}

// awaitControlPlaneChannel waits for the server's first channel, which the
// agent's first report needs.
func awaitControlPlaneChannel(ctx context.Context, logger *slog.Logger, channel *cpchannel.Channel) error {
	logger.Info("waiting for the control plane to open its channel")
	wait, cancel := context.WithTimeout(ctx, firstChannelWait)
	defer cancel()
	if err := channel.Wait(wait); err != nil {
		return fmt.Errorf("the control plane did not open a channel to this pool within %s: %w", firstChannelWait, err)
	}
	return nil
}

// channelHandler is channel as the agent's front serves it, nil for none: a
// nil *cpchannel.Channel in an http.Handler would not be.
func channelHandler(channel *cpchannel.Channel) http.Handler {
	if channel == nil {
		return nil
	}
	return channel
}

// decodeControlPlaneKey is the control plane's issuer key, as the bootstrap
// carries it and the agent's server authenticator reads it.
func decodeControlPlaneKey(text string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return nil, fmt.Errorf("decode the control plane's public key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("the control plane's public key is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}
