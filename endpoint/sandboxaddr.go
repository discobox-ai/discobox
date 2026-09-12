package endpoint

import (
	"fmt"
	"net/url"
	"strings"
)

// SandboxAddress is an address that names one discobox on a server:
// discobox://<server>/<discobox> (ADR 0114 §1), or the same with the transport
// named — discobox+http://<host>:<port>/<discobox>. It is what somebody pastes
// to somebody else, and what a command that takes a discobox accepts in place
// of an ID.
type SandboxAddress struct {
	// Server is the address with the discobox left off: what --server takes,
	// and what a client registers.
	Server string
	// Sandbox is the discobox as it was written: its ID, or a prefix of one.
	Sandbox string
}

// ParseSandboxAddress reads raw as a discobox's address.
//
// Anything not written as a discobox:// address — an ID, a prefix, a name —
// reports false with no error, so a caller that takes those too carries on
// with them. One written as an address that names no discobox, or names its
// server wrongly, is an error: it was plainly meant as one.
func ParseSandboxAddress(raw string) (SandboxAddress, bool, error) {
	raw = strings.TrimSpace(raw)
	scheme := ""
	for _, candidate := range []string{SchemeDiscobox, SchemeDiscoboxHTTP, SchemeDiscoboxHTTPS} {
		if strings.HasPrefix(strings.ToLower(raw), candidate+"://") {
			scheme = candidate
		}
	}
	if scheme == "" {
		return SandboxAddress{}, false, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return SandboxAddress{}, true, err
	}
	if u.Host == "" {
		return SandboxAddress{}, true, fmt.Errorf("%q names no server; a discobox's address is %s://<server>/<discobox>", raw, SchemeDiscobox)
	}
	sandbox := strings.Trim(u.Path, "/")
	if sandbox == "" {
		return SandboxAddress{}, true, fmt.Errorf("%q names a server but no discobox; a discobox's address is %s://<server>/<discobox>", raw, SchemeDiscobox)
	}
	if strings.Contains(sandbox, "/") {
		return SandboxAddress{}, true, fmt.Errorf("%q names more than one discobox; a discobox's address is %s://<server>/<discobox>", raw, SchemeDiscobox)
	}
	server := (&url.URL{Scheme: scheme, Host: u.Host, RawQuery: u.RawQuery}).String()
	if _, err := Parse(server); err != nil {
		return SandboxAddress{}, true, err
	}
	return SandboxAddress{Server: server, Sandbox: sandbox}, true, nil
}
