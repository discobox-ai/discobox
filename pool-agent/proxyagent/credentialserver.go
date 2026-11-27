package proxyagent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/agentcreds"
	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/proxy"
)

const (
	// CredentialsListenAddress is where the pool serves the agent credentials
	// protocol to its sandboxes. It binds all interfaces for the same reason the
	// proxy does: sandboxes reach the pool over the per-pool internal network,
	// resolving ServerName through Docker's embedded DNS.
	//
	// It must not collide with any other pool-container listener: this endpoint
	// shares a process with the proxy, so a bind failure here takes the pool's
	// egress path down with it. 17081 belongs to the BuildKit mediator
	// (buildkitagent.MediatorListen), which is a separate systemd unit in the
	// same network namespace. TestPoolListenAddressesAreDistinct guards it.
	CredentialsListenAddress = "0.0.0.0:17083"

	// CredentialsURL is the address a sandbox's relay dials. It presents the
	// same server certificate as the proxy and verifies the same per-sandbox
	// client certificates, so the sandbox needs no new material and no new
	// trust: its identity here is exactly its identity for egress.
	CredentialsURL = "https://" + ServerName + ":17083"
)

// newControlPlaneCredentials is the broker's client of the control plane for
// one pool, authenticated by the scoped token the pool agent writes.
func newControlPlaneCredentials(root layout.Root, projectID, poolID string) *controlPlaneCredentials {
	return &controlPlaneCredentials{
		contextPath: root.ProxyResolveContextFile(projectID, poolID),
		client:      controlPlaneHTTPClient(),
	}
}

// serveCredentialsOn runs the sandbox-facing agent credentials endpoint on
// tcp until ctx is done, or tcp is closed, which a drain does: then it answers
// what it accepted, each a bounded round trip, and returns. Every request is
// served for the sandbox named by the verified client certificate, so a
// sandbox cannot ask about another sandbox's credentials by saying so in a
// body (ADR 0031 §2). Its listener is given, so a test can drive the real
// mTLS stack on an ephemeral port.
func serveCredentialsOn(ctx context.Context, logger *slog.Logger, tcp net.Listener, bundle *proxy.CertificateBundle, controlPlane *controlPlaneCredentials, secretsPath string, trusts *hostTrusts, proxyServer *proxy.Server) error {
	handler := &credentialsHandler{
		logger:       logger,
		controlPlane: controlPlane,
		secretsPath:  secretsPath,
		trusts:       trusts,
		proxy:        proxyServer,
	}

	listener := tls.NewListener(tcp, sandboxTLSConfig(bundle))
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// A request here is one control-plane round trip, never a stream, so
		// bounded deadlines are safe and a stuck sandbox cannot pin a connection.
		ReadTimeout:  30 * time.Second,
		WriteTimeout: credentialBrokerTimeout + 30*time.Second,
		IdleTimeout:  120 * time.Second,
		BaseContext:  func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		_ = listener.Close()
		_ = server.Close()
	}()

	logger.Info("pool credentials endpoint serving", "addr", tcp.Addr())
	err := server.Serve(listener)
	if errors.Is(err, net.ErrClosed) && ctx.Err() == nil {
		return server.Shutdown(ctx)
	}
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// credentialsHandler binds each request to the sandbox its client certificate
// names, then hands it to the protocol handler. Building the service per
// request rather than once is what makes the identity structural: there is no
// shared handler a caller could reach that is not already scoped to it.
type credentialsHandler struct {
	logger       *slog.Logger
	controlPlane *controlPlaneCredentials
	secretsPath  string
	trusts       *hostTrusts
	proxy        *proxy.Server
}

func (h *credentialsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sandboxID := sandboxIDFromRequest(r)
	if sandboxID == "" {
		http.Error(w, "client certificate does not identify a sandbox", http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), credentialBrokerTimeout)
	defer cancel()
	if r.URL.Path == sandboxconfig.RawSecretsRoute {
		h.serveRawSecrets(w, r.WithContext(ctx), sandboxID)
		return
	}
	agentcreds.NewHandler(&credentialBroker{
		sandboxID:   sandboxID,
		controlPlan: h.controlPlane,
		secretsPath: h.secretsPath,
		trusts:      h.trusts,
		proxy:       h.proxy,
		logger:      h.logger,
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

// serveRawSecrets answers the sandbox's raw secrets (ADR 26-10-27-840): each
// raw binding SecretsFile names for it, in the variable and delivery it is
// delivered in, with the value this process holds to watch its requests for.
// One the control plane no longer hands out is left out, which is how the
// sandbox learns it is gone. It is the one route here that carries a value,
// and it is not part of the agent credentials protocol: the sandbox agent
// reads it over its own link, and its credentials relay does not serve it.
func (h *credentialsHandler) serveRawSecrets(w http.ResponseWriter, r *http.Request, sandboxID string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	doc, err := readSecretsDoc(h.secretsPath)
	if err != nil {
		http.Error(w, "raw secrets unavailable", http.StatusServiceUnavailable)
		return
	}
	held := doc.Raw[sandboxID]
	out := sandboxconfig.RawSecretsResponse{Secrets: []sandboxconfig.RawSecret{}}
	if len(held) > 0 && h.proxy != nil {
		for _, value := range h.proxy.RawSecrets(r.Context(), sandboxID) {
			entry, ok := held[value.Sentinel]
			if !ok || !sandboxconfig.ValidRawDelivery(entry.Delivery) {
				continue
			}
			secret := sandboxconfig.RawSecret{Name: entry.EnvName, Delivery: entry.Delivery, Value: value.Value}
			if !value.ExpiresAt.IsZero() {
				expires := value.ExpiresAt.UTC()
				secret.ExpiresAt = &expires
			}
			out.Secrets = append(out.Secrets, secret)
		}
	}
	slices.SortFunc(out.Secrets, func(a, b sandboxconfig.RawSecret) int { return strings.Compare(a.Name, b.Name) })
	data, err := json.Marshal(out)
	if err != nil {
		http.Error(w, "raw secrets unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}
