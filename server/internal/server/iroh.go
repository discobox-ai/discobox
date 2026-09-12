package server

import (
	"context"
	"fmt"
	"log"
	"net/netip"

	"github.com/discobox-ai/discobox/endpoint"
	"github.com/discobox-ai/discobox/server/internal/irohd"
	"github.com/discobox-ai/discobox/server/internal/services"
)

// configureIroh loads this server's identity and, when any listen endpoint
// asks for one, installs its iroh endpoint and admission policy.
//
// The identity is loaded on every start, whatever the server listens on
// (ADR 0115): an ed25519 key in the data directory, written by Go like the SSH
// host key, whose public half is the peer ID `GET /peer` serves (ADR 0098).
// Loading it binds nothing. What is skipped without an iroh endpoint is the
// endpoint — the native library, the UDP socket, the relay — none of which a
// server that was not asked to serve iroh should open.
//
// It runs before listenAll because binding the endpoint needs the identity.
func configureIroh(ctx context.Context, dataDir string, listenEndpoints, relayURLs []string, logLevel string) (*irohd.Admission, endpoint.IrohID, *irohd.ListenerWatch, error) {
	key, err := irohd.LoadOrCreateEndpointKey(dataDir)
	if err != nil {
		return nil, endpoint.IrohID{}, nil, fmt.Errorf("server identity key: %w", err)
	}
	id, err := irohd.EndpointID(key)
	if err != nil {
		return nil, endpoint.IrohID{}, nil, fmt.Errorf("server peer ID: %w", err)
	}
	// Printed on every start, and before any listener: for the one caller GET
	// /peer cannot serve — a client whose only transport is the endpoint it is
	// trying to find (ADR 0052 §6) — and for an operator comparing it against
	// what a client recorded. Every other caller asks for it (ADR 0098).
	log.Printf("this server's peer ID is %s", id)
	if !hasIrohEndpoint(listenEndpoints) {
		return nil, id, nil, nil
	}
	level, err := endpoint.ParseIrohLogLevel(logLevel)
	if err != nil {
		return nil, endpoint.IrohID{}, nil, fmt.Errorf("iroh.logLevel: %w", err)
	}
	// Ahead of everything else, so a failure to load the library or bind the
	// socket is itself logged at the level that was asked for. log.Writer() is
	// this server's own log destination, which for an autolaunched server is
	// the file `discobox admin server logs` prints.
	if err := endpoint.SetIrohLogging(level, log.Writer()); err != nil {
		return nil, endpoint.IrohID{}, nil, fmt.Errorf("configure iroh logging: %w", err)
	}
	// Built here and handed its store once NewApp returns: this runs before
	// the database exists, so the managed layer cannot be captured (ADR 0095
	// §4, enrolled iroh IDs). Both layers are consulted per connection rather than cached, so
	// enrolling or revoking takes effect on the next connection without a
	// restart — the contract sshd's authorized_keys has.
	admission := irohd.NewAdmission(dataDir)
	// Once, here, where somebody who has just upgraded is reading.
	irohd.LogAuthorizedIDProblems(dataDir)
	if err := endpoint.ConfigureIroh(endpoint.IrohConfig{
		SecretKey: key,
		Authorize: admission.Authorize,
		// Empty keeps n0's public relays, which are free but rate-limited and
		// carry no uptime guarantee. A deployment on its own relays has to
		// configure its clients too: the address carries a peer ID and does
		// not name ours (ADR 0096 §6, configuration file).
		RelayURLs: relayURLs,
		// The ports of the last start, so the clients that remembered them
		// can still dial this server directly after a restart.
		PreferredBindAddrs: irohd.LoadSockets(dataDir),
		Bound: func(sockets []netip.AddrPort) {
			if err := irohd.RememberSockets(dataDir, sockets); err != nil {
				log.Printf("iroh: could not remember this server's sockets for the next start: %v", err)
			}
		},
	}); err != nil {
		return nil, endpoint.IrohID{}, nil, fmt.Errorf("configure iroh: %w", err)
	}
	watch := irohd.NewListenerWatch()
	watch.Start(ctx)
	return admission, id, watch, nil
}

func hasIrohEndpoint(listenEndpoints []string) bool {
	for _, raw := range listenEndpoints {
		parsed, err := endpoint.Parse(raw)
		if err != nil {
			continue
		}
		if parsed.Scheme == "iroh" {
			return true
		}
	}
	return false
}

// irohFallbackURL renders a listener's address with this host's direct socket
// addresses attached, and reports an error for every other scheme so the
// caller can simply not print one.
//
// It is printed beside the address rather than instead of it. The address is
// what an operator reads, pastes and enrolls; this is what a peer dials when
// discovery cannot resolve that address — a deployment with no route to a
// discovery service, or two peers on one host that should not wait for one
// (ADR 0097 §3).
func irohFallbackURL(display string) (string, error) {
	parsed, err := endpoint.Parse(display)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "iroh" {
		return "", fmt.Errorf("endpoint %q is not reached over iroh", display)
	}
	return endpoint.LocalIrohFallbackURL()
}

// irohListenerService is how GET /peer answers what this server's listener is
// doing right now, as opposed to who it is.
//
// It is a func rather than a value because that is the difference that matters:
// the peer ID is loaded once and cannot change, while whether the listener has
// a relay changes underneath a running server and is exactly the thing nobody
// could see. A server with no iroh endpoint returns nil, and the API reports
// nothing rather than a listener that is down.
func irohListenerService(watch *irohd.ListenerWatch) services.IrohListenerService {
	if watch == nil {
		return nil
	}
	return func() (services.IrohListener, bool) {
		state, since, read := watch.State()
		if !read {
			return services.IrohListener{}, false
		}
		return services.IrohListener{
			Online:      state.Online,
			HomeRelay:   state.HomeRelay,
			Since:       since,
			Sockets:     joinAddrPorts(state.Sockets),
			DirectAddrs: state.DirectAddrs,
		}, true
	}
}

func joinAddrPorts(addrs []netip.AddrPort) []string {
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		out = append(out, addr.String())
	}
	return out
}

// serverPeer is what GET /peer answers with. A server that never configured an
// iroh endpoint reports no ID rather than a zero one: the peer ID form of the
// zero key is a real-looking address that reaches nothing.
func serverPeer(id endpoint.IrohID) services.ServerPeer {
	if id.IsZero() {
		return services.ServerPeer{}
	}
	return services.ServerPeer{ID: id.String()}
}
