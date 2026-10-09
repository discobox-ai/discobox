package bridge

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// maxRequestLine bounds how much of a connection's start the forwarder reads
// before deciding where it goes; a longer request line is simply forwarded.
const maxRequestLine = 8 << 10

// localTarget reports whether the connection client starts with an HTTP proxy
// request for an IP address in one of the host's own subnets, and if so its
// host:port and whether it is a CONNECT. It only peeks: whatever it reads is
// still in client for the path the connection takes.
//
// A connection whose first byte cannot start an HTTP method — SOCKS, TLS — is
// decided on that byte alone, since a SOCKS client waits for the proxy's
// answer to its greeting before it sends anything more.
func (f *Forwarder) localTarget(client *bufio.Reader) (target string, connect, ok bool) {
	if f.localSubnets == nil {
		return "", false, false
	}
	first, err := client.Peek(1)
	if err != nil || first[0] < 'A' || first[0] > 'Z' {
		return "", false, false
	}
	line, ok := peekLine(client)
	if !ok {
		return "", false, false
	}
	method, rest, _ := strings.Cut(line, " ")
	requestTarget, _, _ := strings.Cut(rest, " ")
	connect = method == http.MethodConnect
	hostport := requestTarget
	if !connect {
		// An absolute-form request; an https one arrives as a CONNECT.
		u, err := url.Parse(requestTarget)
		if err != nil || u.Scheme != "http" || u.Host == "" {
			return "", false, false
		}
		hostport = u.Host
		if u.Port() == "" {
			hostport = net.JoinHostPort(u.Hostname(), "80")
		}
	}
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return "", false, false
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		// A name, even one that resolves to a local address, goes to the pool
		// proxy, as NO_PROXY's subnet entries never cover names either.
		return "", false, false
	}
	addr = addr.Unmap()
	for _, cidr := range f.localSubnets() {
		if prefix, err := netip.ParsePrefix(cidr); err == nil && prefix.Contains(addr) {
			return hostport, connect, true
		}
	}
	return "", false, false
}

// peekLine returns the first line client holds, without consuming it.
func peekLine(client *bufio.Reader) (string, bool) {
	for n := 1; n <= maxRequestLine; n++ {
		b, err := client.Peek(n)
		if err != nil {
			return "", false
		}
		if b[n-1] == '\n' {
			return strings.TrimRight(string(b), "\r\n"), true
		}
	}
	return "", false
}

// direct serves the proxy request at the start of client by connecting to
// target itself. A CONNECT is answered and then tunneled. An absolute-form
// request is passed on byte for byte except for what a proxy must change: the
// request line takes origin form, the proxy's own and hop-by-hop headers are
// dropped, and it asks for "Connection: close", so the client does not reuse
// the connection for a request this one was not decided for. An upgrade
// request keeps "Connection: upgrade" and becomes a tunnel.
func (f *Forwarder) direct(ctx context.Context, local net.Conn, client *bufio.Reader, target string, connect bool) {
	head, err := readHead(client, connect)
	if err != nil {
		_ = local.Close()
		return
	}
	var dialer net.Dialer
	remote, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		slog.Warn("sandbox proxy bridge could not reach a local address", "target", target, "error", err)
		_, _ = io.WriteString(local, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		_ = local.Close()
		return
	}
	f.trackConn(remote)
	defer f.untrackConn(remote)

	if connect {
		if _, err := io.WriteString(local, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
			return
		}
	}
	// The response is copied while the head is written, and the body is
	// spliced rather than parsed, so a client waiting on "100 Continue"
	// before sending its body is not stalled.
	splice(local, remote, func() error {
		if _, err := io.WriteString(remote, head); err != nil {
			return err
		}
		_, err := io.Copy(remote, client)
		return err
	})
}

// framing are the headers the origin needs to find the request and its body;
// a Connection header naming one does not drop it.
var framing = map[string]bool{"": true, "host": true, "content-length": true, "transfer-encoding": true}

// readHead consumes the request line and headers of the proxy request at the
// start of client and returns what to send the origin in their place: nothing
// for a CONNECT, and for an absolute-form request its head rewritten as
// direct describes.
func readHead(client *bufio.Reader, connect bool) (string, error) {
	var lines []string
	size := 0
	for {
		line, err := client.ReadString('\n')
		if err != nil {
			return "", err
		}
		if size += len(line); size > http.DefaultMaxHeaderBytes {
			return "", errors.New("request head too large")
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		lines = append(lines, line)
	}
	if connect {
		return "", nil
	}
	method, rest, _ := strings.Cut(lines[0], " ")
	requestTarget, proto, _ := strings.Cut(rest, " ")
	// Origin form is the target with its scheme and authority cut off, not
	// rebuilt from a parsed URL, which would re-escape a path the client sent
	// raw and break anything that signs or matches it.
	authority, path := requestTarget[len("http://"):], "/"
	if end := strings.IndexAny(authority, "/?"); end >= 0 {
		authority, path = authority[:end], authority[end:]
		if path[0] == '?' {
			path = "/" + path
		}
	}
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		authority = authority[at+1:]
	}

	type field struct{ name, value string }
	fields := make([]field, 0, len(lines)-1)
	hop := map[string]bool{
		"connection": true, "proxy-connection": true, "keep-alive": true,
		"proxy-authorization": true, "upgrade": true,
	}
	upgrade, hasHost := false, false
	for _, line := range lines[1:] {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return "", errors.New("malformed header line")
		}
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		lower := strings.ToLower(name)
		if lower == "connection" || lower == "proxy-connection" {
			for _, token := range strings.Split(value, ",") {
				token = strings.ToLower(strings.TrimSpace(token))
				if token == "upgrade" {
					upgrade = true
				} else if !framing[token] {
					hop[token] = true
				}
			}
		}
		hasHost = hasHost || lower == "host"
		fields = append(fields, field{name, value})
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s\r\n", method, path, proto)
	if !hasHost {
		fmt.Fprintf(&b, "Host: %s\r\n", authority)
	}
	for _, f := range fields {
		lower := strings.ToLower(f.name)
		if hop[lower] && (!upgrade || lower != "upgrade") {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\r\n", f.name, f.value)
	}
	if upgrade {
		b.WriteString("Connection: upgrade\r\n\r\n")
	} else {
		b.WriteString("Connection: close\r\n\r\n")
	}
	return b.String(), nil
}
