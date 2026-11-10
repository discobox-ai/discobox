package bridge

import (
	"context"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

// follow says where a link pressed in a pane goes, by the core's rules: a
// discobox link to the box it names (console.ParseBoxLink, BoxLinkServer,
// AddressedBox), read from the last listing, and any other link through the
// forward the pane's box's header holds (console.ForwardedURL). Finding the
// link on the screen, and opening what this answers, are the app's.
func (s *Server) follow(ctx context.Context, f Follow) Result {
	if !console.IsBoxLink(f.URI) {
		return Result{Status: StatusDone, URL: console.ForwardedURL(f.URI, func() map[console.PortKey]int { return s.forwarded(f.Box) })}
	}
	address, err := console.ParseBoxLink(f.URI)
	if err != nil {
		return failed(err.Error())
	}
	session, last := s.lastListing()
	primary := ""
	if len(session.Servers) > 0 {
		primary = session.Servers[0]
	}
	// The pane's server is its box's key's, listed or not: a box of a server
	// the last listing could not read is missing from it (Key).
	reader := console.LinkReaderServer(keyServer(f.Box), f.Box != "", primary)
	server, err := console.BoxLinkServer(ctx, s.ds, address, reader, f.URI)
	if err != nil {
		return failed(err.Error())
	}
	box, err := console.AddressedBox(last.Sandboxes, last.Unreachable, server, address.Sandbox, f.URI)
	if err != nil {
		return failed(err.Error())
	}
	return Result{Status: StatusEnter, Box: Key(box.Server, box.ID)}
}

// holdForward records the bindings of a forward a box's header holds, by the
// box's key, until the returned func lets it go: what a link pressed in that
// box's panes is pointed through.
func (s *Server) holdForward(key string, bindings func() []console.Binding) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	held := &bindings
	s.forwards[key] = append(s.forwards[key], held)
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for i, h := range s.forwards[key] {
			if h == held {
				s.forwards[key] = append(s.forwards[key][:i:i], s.forwards[key][i+1:]...)
				break
			}
		}
		if len(s.forwards[key]) == 0 {
			delete(s.forwards, key)
		}
	}
}

// forwarded is what the forward a box's header holds has bound, by port: nil
// while nothing follows its header, or its forward has bound nothing.
func (s *Server) forwarded(key string) map[console.PortKey]int {
	s.mu.Lock()
	held := s.forwards[key]
	s.mu.Unlock()
	if len(held) == 0 {
		return nil
	}
	return console.Forwarded((*held[len(held)-1])())
}
