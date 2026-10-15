package filter

import "github.com/discobox-ai/discobox/proxy/egress"

// Config is the destination policy: the allow list of each restricted client,
// keyed by client ID. A client absent from it is unrestricted.
type Config struct {
	Clients map[string][]string
}

// Filter decides destinations per client (ADR 26-10-01-032 §6).
type Filter struct {
	clients map[string]egress.Matcher
}

// New creates a filter from config.
func New(cfg Config) *Filter {
	f := &Filter{clients: make(map[string]egress.Matcher, len(cfg.Clients))}
	for clientID, allow := range cfg.Clients {
		f.clients[clientID] = egress.NewMatcher(allow)
	}
	return f
}

// AllowHostForClient reports whether clientID may reach host. A restricted
// client with no entries reaches nothing.
func (f *Filter) AllowHostForClient(host, clientID string) bool {
	if f == nil {
		return true
	}
	matcher, restricted := f.clients[clientID]
	if !restricted {
		return true
	}
	return matcher.Allows(host)
}
