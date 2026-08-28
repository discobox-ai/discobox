package proxyagent

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/discobox-ai/discobox/agentcreds"
	"github.com/discobox-ai/discobox/layout"
	"github.com/discobox-ai/discobox/proxy"
)

const (
	// CredentialsListenAddress is where the pool serves the agent credentials
	// protocol to its sandboxes. It binds all interfaces for the same reason the
	// proxy does: sandboxes reach the pool over the per-pool internal network,
	// resolving ServerName through Docker's embedded DNS.
	CredentialsListenAddress = "0.0.0.0:17081"

	// CredentialsURL is the address a sandbox's relay dials. It presents the
	// same server certificate as the proxy and verifies the same per-sandbox
	// client certificates, so the sandbox needs no new material and no new
	// trust: its identity here is exactly its identity for egress.
	CredentialsURL = "https://" + ServerName + ":17081"

	// activationSweepInterval bounds how long an expired activation lingers in
	// the proxy's match set. Expiry is enforced at resolve time regardless; this
	// only keeps the set from growing with dead entries on an idle sandbox.
	activationSweepInterval = time.Minute
)

// serveCredentials runs the sandbox-facing agent credentials endpoint until ctx
// is done. Every request is served for the sandbox named by the verified client
// certificate, so a sandbox cannot ask about another sandbox's credentials by
// saying so in a body (ADR 0031 §2).
func serveCredentials(ctx context.Context, logger *slog.Logger, bundle *proxy.CertificateBundle, projectID, poolID string, live *activations) error {
	var listenConfig net.ListenConfig
	tcp, err := listenConfig.Listen(ctx, "tcp", CredentialsListenAddress)
	if err != nil {
		return err
	}
	return serveCredentialsOn(ctx, logger, tcp, bundle, projectID, poolID, live)
}

// serveCredentialsOn serves on an already-bound listener. The seam exists so a
// test can drive the real mTLS stack on an ephemeral port — the certificate
// handling and identity derivation are the parts worth testing, and they are
// exactly what a fake listener would skip.
func serveCredentialsOn(ctx context.Context, logger *slog.Logger, tcp net.Listener, bundle *proxy.CertificateBundle, projectID, poolID string, live *activations) error {
	broker := &controlPlaneCredentials{
		contextPath: layout.ProxyResolveContextFile(projectID, poolID),
		client:      controlPlaneHTTPClient(),
	}
	handler := &credentialsHandler{controlPlane: broker, activations: live}

	listener := tls.NewListener(tcp, &tls.Config{
		Certificates: []tls.Certificate{bundle.ServerCert},
		ClientCAs:    bundle.ClientCAPool,
		// The client certificate is the identity, so an unverified client is not
		// an anonymous caller to be authenticated some other way — it is no
		// caller at all.
		ClientAuth: tls.RequireAndVerifyClientCert,
		MinVersion: tls.VersionTLS12,
	})
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// A request here is one control-plane round trip, never a stream, so
		// bounded deadlines are safe and a stuck sandbox cannot pin a connection.
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
		BaseContext:  func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		_ = listener.Close()
		_ = server.Close()
	}()
	go sweepActivations(ctx, live)

	logger.Info("pool credentials endpoint serving", "addr", tcp.Addr())
	err := server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func sweepActivations(ctx context.Context, live *activations) {
	ticker := time.NewTicker(activationSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			live.sweep()
		}
	}
}

// credentialsHandler binds each request to the sandbox its client certificate
// names, then hands it to the protocol handler. Building the service per
// request rather than once is what makes the identity structural: there is no
// shared handler a caller could reach that is not already scoped to it.
type credentialsHandler struct {
	controlPlane *controlPlaneCredentials
	activations  *activations
}

func (h *credentialsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sandboxID := sandboxIDFromRequest(r)
	if sandboxID == "" {
		http.Error(w, "client certificate does not identify a sandbox", http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), credentialBrokerTimeout)
	defer cancel()
	agentcreds.NewHandler(&credentialBroker{
		sandboxID:   sandboxID,
		controlPlan: h.controlPlane,
		activations: h.activations,
	}).ServeHTTP(w, r.WithContext(ctx))
}

// sandboxIDFromRequest reads the sandbox ID out of the verified client
// certificate, matching how the proxy derives a client ID from the same
// certificate: the common name, which EnsureClientCertificate sets to the
// sandbox ID.
func sandboxIDFromRequest(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	return r.TLS.PeerCertificates[0].Subject.CommonName
}
