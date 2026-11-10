package console

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/endpoint"
)

// Where a link in a pane goes is the core's (ADR 26-11-07-876 §1): finding a
// link on a screen is the renderer's terminal emulation, and what a renderer
// does with the answer — open a browser, focus a box, say why not — is its
// own, but which port a URL printed in a box reaches from here, and which box
// a discobox: link names, are answered here for both consoles.

// ForwardedURL is where a URL printed inside a box actually reaches from
// here, through a forward whose bindings forwarded returns.
//
// A server in a sandbox prints the address it bound — `http://localhost:8080`,
// `http://0.0.0.0:8080` — and both halves of that are wrong on this side of the
// forward. The port is only 8080 here if 8080 was free when the forward took
// it, and the bind address a server prints is not one a browser can open. So a
// link on that text points at the local end instead: the port the forward
// actually bound, on localhost.
//
// It is the name and not the address deliberately. Under WSL2 the Windows side
// reaches a Linux listener by the name — `localhost` is what the port proxy is
// published under — and a literal 127.0.0.1 handed to a browser over there is
// that machine's own loopback, which is nothing. The header's ports
// (portLink) spell the same URL out.
//
// Anything else is returned as it came: a URL to somewhere that is not this
// sandbox is already right, and saying so is what keeps a renderer from
// linking it. Only the ports the forward has bound are moved — a link to a
// local port nothing is listening on is worse than no link, which is the rule
// the header's ports follow too. forwarded is asked only once a URL could be
// one of the box's: it is called for every URL on a screen, every frame, and
// most of them are somewhere else entirely.
func ForwardedURL(raw string, forwarded func() map[PortKey]int) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	remote, ok := sandboxPort(parsed)
	if !ok {
		return raw
	}
	// A URL's port is a TCP one: http and https are the only schemes this
	// moves.
	local, ok := forwarded()[PortKey{Number: remote}]
	if !ok {
		return raw
	}
	parsed.Host = net.JoinHostPort("localhost", strconv.Itoa(local))
	return parsed.String()
}

// OpenedURL is the web address a pane's link may open on this machine,
// after pointing it through the box's forward. Unlike an OSC 8 passed to an
// outer terminal, opening it here hands it to this machine's URL handler:
// files and other applications' schemes must never reach that handler.
// Box links are answered inside the console, before calling this.
func OpenedURL(raw string, forwarded func() map[PortKey]int) (string, error) {
	target := ForwardedURL(raw, forwarded)
	parsed, err := url.Parse(target)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.Opaque != "" {
		return "", fmt.Errorf("not opened: %q is not a web address", raw)
	}
	return target, nil
}

// sandboxPort is the port a URL names inside the sandbox, and whether the URL
// is one the forward could be standing in for at all.
//
// Loopback and the wildcard both qualify: they are what a server prints, and
// they name the sandbox's own network namespace either way. A port left out is
// the scheme's — `http://localhost/` is port 80 there, and 80 is as forwardable
// as anything else.
func sandboxPort(parsed *url.URL) (int, bool) {
	var fallback int
	switch parsed.Scheme {
	case "http":
		fallback = 80
	case "https":
		fallback = 443
	default:
		return 0, false
	}
	host := parsed.Hostname()
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || (!ip.IsLoopback() && !ip.IsUnspecified()) {
			return 0, false
		}
	}
	port := parsed.Port()
	if port == "" {
		return fallback, true
	}
	number, err := strconv.Atoi(port)
	if err != nil || number <= 0 {
		return 0, false
	}
	return number, true
}

// BoxLinkSchemes are the schemes a discobox's address is written in. A link
// to one in a pane is the console's to answer (ADR 26-11-03-113 §3): pressing
// it opens that box. Nothing outside the console can open one, so a renderer
// claims these links from its terminal rather than passing them on.
var BoxLinkSchemes = []string{endpoint.SchemeDiscobox, endpoint.SchemeDiscoboxHTTP, endpoint.SchemeDiscoboxHTTPS}

// IsBoxLink reports whether a link is one the console answers: its scheme is
// one of BoxLinkSchemes, in any case.
func IsBoxLink(uri string) bool {
	scheme, _, ok := strings.Cut(uri, ":")
	return ok && slices.ContainsFunc(BoxLinkSchemes, func(s string) bool { return strings.EqualFold(s, scheme) })
}

// AddressedServerTimeout bounds asking which server a box link names. Most
// of the answer is a comparison; what can be slow is a listed server asked
// for its peer ID, and a press should say something rather than hang.
const AddressedServerTimeout = 5 * time.Second

// A box link's failures are said on a status line, which a renderer draws
// outside any pane's emulator, while the link and everything parsed out of it
// is text the program in the pane wrote: so every such value goes in quoted,
// %q, whose escapes cover C0 and C1 controls and invisible format characters
// (cli/REVIEW.md). A percent-encoded ESC in a link decodes to a real one.

// ParseBoxLink is the discobox address a box link names, or why it names
// none.
func ParseBoxLink(uri string) (endpoint.SandboxAddress, error) {
	address, ok, err := endpoint.ParseSandboxAddress(uri)
	switch {
	case err != nil:
		// ParseSandboxAddress quotes what it was given in its errors.
		return endpoint.SandboxAddress{}, err
	case !ok:
		return endpoint.SandboxAddress{}, fmt.Errorf("%q is not a discobox's address", uri)
	}
	return address, nil
}

// LinkReaderServer is the reader's server for a box link in a pane (ADR
// 26-11-03-113 §2): the server of the box the pane belongs to (paneServer,
// with inBox), which is where the program that wrote it runs, and the primary
// for a pane that is no box's. The pane's server is the one its box is known
// by, listed or not: a box whose server did not answer the last listing is
// missing from it, and its links are still on its server. A window with one
// server names none, on its rows or in its session.
func LinkReaderServer(paneServer string, inBox bool, primary string) string {
	if inBox {
		return paneServer
	}
	return primary
}

// BoxLinkServer is which listed server a box link's address names, by the
// name its rows carry: the reader's for one that names none. A server the
// window does not list is never dialed and never registered — the text was
// written by whatever runs in the box, and a click is no reason to reach a
// host it chose; a typed attach is (ADR 0116 §6) — so it is refused, with how
// to register it.
func BoxLinkServer(ctx context.Context, ds DataSource, address endpoint.SandboxAddress, reader, uri string) (string, error) {
	if address.Local() {
		return reader, nil
	}
	ctx, cancel := context.WithTimeout(ctx, AddressedServerTimeout)
	defer cancel()
	name, known, unsure := ds.AddressedServer(ctx, address.Server)
	switch {
	case known:
		return name, nil
	case unsure != nil:
		// It may be one of the servers that did not say who they are.
		return "", fmt.Errorf("%q: no server this console lists says it is the one named, and %w", uri, unsure)
	}
	return "", fmt.Errorf("%q is on a server this console doesn't know — `discobox attach %q` registers it", uri, uri)
}

// AddressedBox is the box a link's ref names on server, by ID or by a prefix
// only one of its boxes has, among the boxes listed; unreachable are the
// servers the listing could not read, of which server may be one.
func AddressedBox(boxes []Sandbox, unreachable []string, server, ref, uri string) (Sandbox, error) {
	var found []Sandbox
	for _, box := range boxes {
		if box.Server != server {
			continue
		}
		if box.ID == ref {
			return box, nil
		}
		if strings.HasPrefix(box.ID, ref) {
			found = append(found, box)
		}
	}
	on := "this server"
	if server != "" {
		on = server
	}
	switch {
	case len(found) == 1:
		return found[0], nil
	case len(found) > 1:
		return Sandbox{}, fmt.Errorf("%q: %q is more than one discobox on %s", uri, ref, on)
	case slices.Contains(unreachable, server):
		return Sandbox{}, fmt.Errorf("%q: %s isn't answering", uri, on)
	}
	return Sandbox{}, fmt.Errorf("%q: no discobox %q on %s", uri, ref, on)
}
