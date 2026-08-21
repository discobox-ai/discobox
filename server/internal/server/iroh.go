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
func configureIroh(dataDir string, listenEndpoints []string) error {
	if !hasIrohEndpoint(listenEndpoints) {
		return nil
	}
	key, err := irohd.LoadOrCreateEndpointKey(dataDir)
	if err != nil {
		return fmt.Errorf("iroh endpoint key: %w", err)
	}
	if err := endpoint.ConfigureIroh(endpoint.IrohConfig{
		SecretKey: key,
		Authorize: func(id endpoint.IrohID) bool {
			// Loaded per connection rather than once at startup, so enrolling
			// or revoking an ID takes effect on the next connection without a
			// restart — the same contract sshd's authorized_keys has.
			authorized, err := irohd.LoadAuthorizedIDs(dataDir)
			if err != nil {
				// Fail closed. An unreadable allowlist is not a reason to
				// admit everyone.
				log.Printf("iroh: read authorized IDs: %v", err)
				return false
			}
			if !authorized.Allows(id) {
				log.Printf("iroh: refused endpoint %s: not in authorized_ids", id)
				return false
			}
			return true
		},
	}); err != nil {
		return fmt.Errorf("configure iroh: %w", err)
	}
	id, err := endpoint.LocalIrohID()
	if err != nil {
		return fmt.Errorf("iroh endpoint ID: %w", err)
	}
	// Printed before the listener starts because it is the only way anyone
	// learns the address: unlike the SSH endpoint, an iroh address cannot be
	// fetched over the API, since it *is* how the API is reached (ADR 0052 §6).
	log.Printf("iroh endpoint ID is %s", id)
	return nil
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
