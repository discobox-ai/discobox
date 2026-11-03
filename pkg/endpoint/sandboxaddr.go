package endpoint

import (
	"fmt"
	"net/url"
	"strings"
)

// SandboxAddress is an address that names one discobox on a server:
// discobox://<server>/<discobox> (ADR 0116 §1), or the same with the transport
// named — discobox+http://<host>:<port>/<discobox>. It is what somebody pastes
// to somebody else, and what a command that takes a discobox accepts in place
// of an ID.
//
// discobox:///<discobox>, with no server at all, names the discobox on the
// reader's server (ADR 26-11-03-113 §§1-2): whichever server the one reading
// it would name a bare ID on. Its Server is empty, and the reader puts its own
// server there before anything is dialed.
type SandboxAddress struct {
	// Server is the address with the discobox left off: what --server takes,
	// and what a client registers. Empty for discobox:///<discobox>, which
	// names no server (Local).
	Server string
	// Sandbox is the discobox as it was written: its ID, or a prefix of one.
	Sandbox string
}

// EnvSandboxAddress holds a discobox's own address,
// discobox://<server-peer-id>/<sandbox-id>: what a process inside it hands
// somebody so they can reach this discobox. A server sets it on the sandboxes
// it creates while it listens on iroh; a sandbox created before, or on a
// server with no iroh listener, has none.
const EnvSandboxAddress = "DISCOBOX_ADDRESS"

// Local reports whether the address names no server, which makes it the
// reader's: discobox:///<discobox>.
func (a SandboxAddress) Local() bool { return a.Server == "" }

// String is the address written out, the form [ParseSandboxAddress] reads.
func (a SandboxAddress) String() string {
	if a.Local() {
		return SchemeDiscobox + ":///" + a.Sandbox
	}
	u, err := url.Parse(a.Server)
	if err != nil {
		return strings.TrimSuffix(a.Server, "/") + "/" + a.Sandbox
	}
	u.Path = "/" + a.Sandbox
	return u.String()
}

// ParseSandboxAddress reads raw as a discobox's address.
//
// Anything not written as a discobox:// address — an ID, a prefix, a name —
// reports false with no error, so a caller that takes those too carries on
// with them. One written as an address that names no discobox, or names its
// server wrongly, is an error: it was plainly meant as one.
//
// discobox:///<discobox> is read with an empty Server (Local). It is the bare
// scheme only — a named transport needs a host to apply to — and carries
// nothing after the discobox: the query an address may carry is its server's
// (?addr=), and it names none.
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
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return SandboxAddress{}, true, fmt.Errorf("%q carries a fragment; a discobox's address is %s://<server>/<discobox>", raw, SchemeDiscobox)
	}
	sandbox := strings.Trim(u.Path, "/")
	if u.Host == "" {
		switch {
		case scheme != SchemeDiscobox || !strings.HasPrefix(raw[len(scheme)+len("://"):], "/"):
			return SandboxAddress{}, true, fmt.Errorf("%q names no server; a discobox's address is %s://<server>/<discobox>, or %s:///<discobox> on your own server", raw, SchemeDiscobox, SchemeDiscobox)
		case sandbox == "":
			return SandboxAddress{}, true, fmt.Errorf("%q names no discobox; a discobox on your own server is %s:///<discobox>", raw, SchemeDiscobox)
		case strings.Contains(sandbox, "/"):
			return SandboxAddress{}, true, fmt.Errorf("%q names more than one discobox; a discobox on your own server is %s:///<discobox>", raw, SchemeDiscobox)
		case u.RawQuery != "" || u.ForceQuery:
			return SandboxAddress{}, true, fmt.Errorf("%q carries a query, and names no server for it to be about; a discobox on your own server is %s:///<discobox>", raw, SchemeDiscobox)
		}
		return SandboxAddress{Sandbox: sandbox}, true, nil
	}
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
