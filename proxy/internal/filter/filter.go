package filter

import "github.com/discobox-ai/discobox/proxy/egress"

// Config is the destination policy: the allow list of each restricted client,
// keyed by client ID, and each client's passthrough hosts. A client absent
// from Clients is unrestricted; one absent from Passthrough has none.
type Config struct {
	Clients     map[string][]string
	Passthrough map[string][]string
}

// Filter decides destinations per client (ADR 26-10-01-032 §6), and which of
// them a CONNECT is carried to unread (ADR 26-10-31-819).
type Filter struct {
	clients     map[string]egress.Matcher
	passthrough map[string]egress.Matcher
}

// New creates a filter from config.
func New(cfg Config) *Filter {
	f := &Filter{
		clients:     make(map[string]egress.Matcher, len(cfg.Clients)),
		passthrough: make(map[string]egress.Matcher, len(cfg.Passthrough)),
	}
	for clientID, allow := range cfg.Clients {
		f.clients[clientID] = egress.NewMatcher(allow)
	}
	for clientID, hosts := range cfg.Passthrough {
		if len(hosts) > 0 {
			f.passthrough[clientID] = egress.NewMatcher(hosts)
		}
	}
	return f
}

// PassthroughForClient reports the passthrough entry that matches host for
// clientID, if any. It says nothing about whether the host is allowed;
// AllowHostForClient does.
func (f *Filter) PassthroughForClient(host, clientID string) (string, bool) {
	if f == nil {
		return "", false
	}
	matcher, ok := f.passthrough[clientID]
	if !ok {
		return "", false
	}
	return matcher.Match(host)
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
