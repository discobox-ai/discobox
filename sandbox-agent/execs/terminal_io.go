package execs

import (
	"context"
	"net/http"
	"os"
	"strconv"

	"github.com/discobox-ai/discobox/sandbox-agent/shimproxy"
	"github.com/discobox-ai/discobox/sandbox-agent/shimruntime"
)

// A terminal read, typed into, and waited on through its shim, without an
// attach (ADR 0137). Each asks the shim that holds the terminal: it alone has
// the emulator, the PTY, and the output as it happens.

// Screen reads an exec's terminal screen as text.
func (m *Manager) Screen(ctx context.Context, id string, scrollback int) (shimruntime.ScreenText, error) {
	socket, err := m.liveSocket(id)
	if err != nil {
		return shimruntime.ScreenText{}, err
	}
	return shimproxy.CallJSON[shimruntime.ScreenText](ctx, socket, http.MethodGet, "/screen?scrollback="+strconv.Itoa(max(scrollback, 0)), nil)
}

// Input writes input parts to an exec's terminal.
func (m *Manager) Input(ctx context.Context, id string, parts []shimruntime.InputPart) error {
	socket, err := m.liveSocket(id)
	if err != nil {
		return err
	}
	_, err = shimproxy.CallJSON[struct{}](ctx, socket, http.MethodPost, "/input", ShimInput{Input: parts})
	return err
}

// AwaitTerminal waits in an exec's shim for its terminal to go quiet or its
// process to exit, bounded by the wait's timeout.
func (m *Manager) AwaitTerminal(ctx context.Context, id string, wait ShimWait) (ShimWaitResult, error) {
	socket, err := m.liveSocket(id)
	if err != nil {
		return ShimWaitResult{}, err
	}
	return shimproxy.CallJSON[ShimWaitResult](ctx, socket, http.MethodPost, "/wait", wait)
}

// liveSocket is the shim socket of an exec that still has one. An exec that has
// ended has no terminal left to read or type into.
func (m *Manager) liveSocket(id string) (string, error) {
	exec, ok := m.Get(id)
	if !ok {
		return "", ErrNotFound
	}
	if _, err := os.Stat(exec.SocketPath); err != nil {
		return "", ErrSessionGone
	}
	return exec.SocketPath, nil
}
