package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/discobox-ai/discobox/pkg/ahphost"
	"github.com/discobox-ai/discobox/sandbox-agent/ahplocal"
	"github.com/discobox-ai/discobox/sandbox-agent/localexec"
)

// runAHP serves this discobox to VS Code's Agents window (ADR 26-10-22-853):
// VS Code runs it over SSH as the sandbox user, through
// chat.sshRemoteAgentHostCommand, reads the WebSocket URL it prints, and
// tunnels to it. It serves until that SSH channel closes — which ends its
// stdin, since VS Code runs it with no terminal to hang up — or it is
// signaled.
func runAHP(ctx context.Context, args []string, stdin *os.File, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("discobox-sandbox-agent ahp", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socket := flags.String("socket", localexec.DefaultSocketPath, "the sandbox agent's local exec socket")
	verbose := flags.Bool("v", false, "log what the host cannot tell its client")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	// The port is the kernel's choice: VS Code reads it from the URL, and
	// every connection gets its own host.
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		logger.Error("listen", "error", err)
		return 1
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		logger.Error("make a connection token", "error", err)
		return 1
	}
	cfg := ahphost.Config{
		Boxes:     ahplocal.NewBoxes(localexec.NewClient(*socket)),
		Workspace: ahplocal.Workspace{},
		Token:     hex.EncodeToString(token),
	}
	if *verbose {
		cfg.Logf = func(format string, args ...any) { logger.Info(fmt.Sprintf(format, args...)) }
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go endWithStdin(stdin, cancel)
	host := ahphost.New(cfg)
	go func() { _ = host.Run(ctx) }()
	server := &http.Server{Handler: host, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	// This line is what VS Code waits for.
	_, _ = fmt.Fprintf(stdout, "ws://%s?tkn=%s\n", ln.Addr(), cfg.Token)
	if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("serve", "error", err)
		return 1
	}
	return 0
}

// endWithStdin calls end once stdin closes, when stdin is a pipe or a socket:
// the SSH channel it was started on. One that is a terminal or /dev/null —
// run by hand — never closes that way.
func endWithStdin(stdin *os.File, end func()) {
	info, err := stdin.Stat()
	if err != nil || info.Mode()&(os.ModeNamedPipe|os.ModeSocket) == 0 {
		return
	}
	_, _ = io.Copy(io.Discard, stdin)
	end()
}
