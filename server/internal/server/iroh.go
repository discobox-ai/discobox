package server

import (
	"fmt"
	"log"

	"github.com/discobox-ai/discobox/endpoint"
	"github.com/discobox-ai/discobox/server/internal/irohd"
)

// configureIroh installs the server's iroh identity and admission policy when
// any listen endpoint asks for one.
//
// It runs before listenAll because binding the endpoint needs the identity,
// and it is skipped entirely when no endpoint names the scheme: an iroh
// endpoint generates a key and opens a UDP socket, neither of which a server
// that was not asked to serve iroh should do.
func configureIroh(dataDir string, listenEndpoints, relayURLs []string) (*irohd.Admission, error) {
	if !hasIrohEndpoint(listenEndpoints) {
		return nil, nil
	}
	key, err := irohd.LoadOrCreateEndpointKey(dataDir)
	if err != nil {
		return nil, fmt.Errorf("iroh endpoint key: %w", err)
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
		return nil, fmt.Errorf("configure iroh: %w", err)
	}
	id, err := endpoint.LocalIrohID()
	if err != nil {
		return nil, fmt.Errorf("iroh endpoint ID: %w", err)
	}
	// Printed before the listener starts because it is the only way anyone
	// learns the address: unlike the SSH endpoint, this address cannot be
	// fetched over the API, since it *is* how the API is reached (ADR 0052 §6).
	log.Printf("this server's peer ID is %s", id)
	return admission, nil
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
