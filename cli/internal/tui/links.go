package tui

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// forwardedURL is where a URL printed inside a pane actually reaches from here.
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
// that machine's own loopback, which is nothing. See also portEntry, which
// spells the same URL out in the header.
//
// Anything else is returned as it came: a URL to somewhere that is not this
// sandbox is already right, and saying so is what keeps [termpane.Model] from
// linking it. Only the ports the forward has bound are moved — a link to a
// local port nothing is listening on is worse than no link, which is the rule
// the header's arrows follow too.
func (m *Model) forwardedURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	// Ahead of the bindings, which are a fresh map each time: this is called
	// for every URL on the screen, every frame, and most of them are somewhere
	// else entirely.
	remote, ok := sandboxPort(parsed)
	if !ok {
		return raw
	}
	// A URL's port is a TCP one: http and https are the only schemes this
	// moves.
	local, ok := m.forwardedPorts()[portKey{number: remote}]
	if !ok {
		return raw
	}
	parsed.Host = net.JoinHostPort("localhost", itoa(local))
	return parsed.String()
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

// launcherGrace is how long a press waits to hear back from the URL handler.
// Long enough for a launcher that has nowhere to send the URL to say so — they
// fail within milliseconds — and short enough that a handler which stays up is
// taken as having opened it before anyone wonders whether the click landed.
const launcherGrace = 2 * time.Second

// openURL hands a URL to whatever this machine opens URLs with: a plain click
// on a header link. The window draws those links for the local end of its
// forward (`http://localhost:<port>`), which is the only end anything here can
// reach.
//
// "This machine" is the one the window runs on. A Ctrl-click hands the same
// OSC 8 link to the terminal instead, which opens it wherever the terminal
// runs; the two are one machine except over SSH, where the forward's local end
// is on this side and the plain click is the one that reaches it.
//
// Each platform has one way in, and WSL has Windows': the listener is on the
// Linux side, but the browser that opens it is not, and `localhost` crosses
// that boundary by name — which is the whole reason the links are written with
// the name rather than the address. See forwardedURL.
//
// The context is the window's with its cancellation taken off
// (context.WithoutCancel, at the call site): quitting the window must not take
// a browser with it wherever the launcher is the process still holding one,
// and a page opened from here should outlive the window that opened it.
func openURL(ctx context.Context, target string) error {
	return launch(openCommand(ctx, target), launcherGrace)
}

// launch starts a URL handler and waits up to grace for its answer, because
// its exit status is the only word there is on whether anything opened.
// xdg-open on a box with no desktop session — an SSH login, a container —
// starts fine and then exits 3 with nowhere to send the URL, and a press that
// reported "opening" over that would be a click that did nothing while saying
// it had. A non-zero exit inside the window is the error.
//
// A handler still running past it is taken as having opened the URL: xdg-open's
// generic path execs the browser itself, so the process that stays is the
// browser. It is reaped whenever it goes, rather than left a zombie for as long
// as the window runs. Its output goes nowhere — the window's own screen is no
// place for a launcher to print — which is also what keeps Wait from waiting on
// a pipe the browser inherited.
func launch(command *exec.Cmd, grace time.Duration) error {
	if err := command.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(command.Path), err)
		}
		return nil
	case <-timer.C:
		return nil
	}
}

// openCommand is the platform's URL handler, with the URL to hand it.
//
// Windows has no `open`, and `start` is a shell builtin whose first quoted
// argument is a window title rather than the thing to open; rundll32 is the
// documented way to ask the shell to handle a URL with no shell in between.
// PowerShell is what a WSL distribution has to reach the Windows side with,
// the same bridge the clipboard crosses — with the value quoted for
// PowerShell, whose escape for a single quote inside a single-quoted string is
// two of them.
func openCommand(ctx context.Context, target string) *exec.Cmd {
	switch {
	case runtime.GOOS == "darwin":
		return exec.CommandContext(ctx, "open", target)
	case runtime.GOOS == "windows":
		return exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", target)
	case isWSL():
		quoted := "'" + strings.ReplaceAll(target, "'", "''") + "'"
		//nolint:gosec // there is no shell between here and powershell's argv, and the value is single-quoted for powershell itself with its own escape for a quote inside one
		return exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-Command", "Start-Process "+quoted)
	default:
		return exec.CommandContext(ctx, "xdg-open", target)
	}
}
