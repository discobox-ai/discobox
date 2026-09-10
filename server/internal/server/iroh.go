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

// configureIroh installs the server's iroh identity and admission policy when
// any listen endpoint asks for one.
//
// It runs before listenAll because binding the endpoint needs the identity,
// and it is skipped entirely when no endpoint names the scheme: an iroh
// endpoint generates a key and opens a UDP socket, neither of which a server
// that was not asked to serve iroh should do.
// It returns the identity it installed along with the gate, because that ID is
// this server's address and `GET /peer` serves it (ADR 0098). A server with no
// iroh endpoint returns the zero ID, which is the honest answer: it has no peer
// identity rather than an unused one.
func configureIroh(ctx context.Context, dataDir string, listenEndpoints, relayURLs []string, logLevel string) (*irohd.Admission, endpoint.IrohID, *irohd.ListenerWatch, error) {
	if !hasIrohEndpoint(listenEndpoints) {
		return nil, endpoint.IrohID{}, nil, nil
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
	key, err := irohd.LoadOrCreateEndpointKey(dataDir)
	if err != nil {
		return nil, endpoint.IrohID{}, nil, fmt.Errorf("iroh endpoint key: %w", err)
	}
	// Built here and handed its store once NewApp returns: this runs before
	// the database exists, so the managed layer cannot be captured (ADR 0095
	// §4). Both layers are consulted per connection rather than cached, so
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
		// not name ours (ADR 0096 §6).
		RelayURLs: relayURLs,
	}); err != nil {
		return nil, endpoint.IrohID{}, nil, fmt.Errorf("configure iroh: %w", err)
	}
	id, err := endpoint.LocalIrohID()
	if err != nil {
		return nil, endpoint.IrohID{}, nil, fmt.Errorf("iroh endpoint ID: %w", err)
	}
	// Printed before the listener starts, for the one caller GET /peer cannot
	// serve: a client whose only transport is the endpoint it is trying to find
	// (ADR 0052 §6). Every other caller — anything already reaching this server
	// over a socket, a pipe or HTTP — asks for it instead (ADR 0098).
	log.Printf("this server's peer ID is %s", id)
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
