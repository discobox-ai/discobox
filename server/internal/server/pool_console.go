package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"github.com/discobox-ai/discobox/pkg/execstream/frame"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// consoleExitWait bounds how long the handler waits for the console container's
// exit status once the stream ends. A detach is the ordinary ending and the
// container is still running then, so this must be short: it is only how long
// the handler spends distinguishing "the shell exited" from "the operator left".
const consoleExitWait = 2 * time.Second

// registerPoolConsoleRoutes exposes the pool host's administrative console.
//
// Unlike the sandbox attach routes, this one is not a reverse proxy to the pool
// agent: the console is opened against the pool host's own runtime through the
// provider driver, so it still answers on a host whose agent never registered —
// which is the situation an operator opens a console for. The control plane
// therefore terminates the websocket itself and pumps execstream frames between
// the client and the console's TTY.
func registerPoolConsoleRoutes(router chi.Router, service services.PoolService) {
	router.Method(http.MethodGet, "/api/projects/{projectId}/pools/{poolId}/console", poolConsoleHandler(service))
}

func poolConsoleHandler(service services.PoolService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if service == nil {
			writeSandboxAgentProxyError(w, http.StatusServiceUnavailable, "pool service is not configured")
			return
		}
		opts := sandbox.ConsoleOptions{
			Command: r.URL.Query()["command"],
			TTY:     r.URL.Query().Get("tty") == "true",
			Rows:    consoleDimension(r.URL.Query().Get("rows")),
			Cols:    consoleDimension(r.URL.Query().Get("cols")),
		}
		// Accept validates both the handshake and Origin before anything can
		// execute on the pool host. Provider failures travel in the framed stream.
		socket, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer socket.Close(websocket.StatusNormalClosure, "done")
		conn := websocket.NetConn(r.Context(), socket, websocket.MessageBinary)
		defer conn.Close()
		console, err := service.OpenPoolConsole(r.Context(), chi.URLParam(r, "projectId"), chi.URLParam(r, "poolId"), opts)
		if err != nil {
			_ = frame.Write(conn, frame.Error, []byte(err.Error()))
			return
		}
		defer console.Close()

		pumpPoolConsole(r.Context(), console, conn, len(opts.Command) > 0)
	})
}

// pumpPoolConsole moves bytes between the console TTY and the framed websocket
// until one of them ends, then reports how the session ended.
func pumpPoolConsole(ctx context.Context, console sandbox.Stream, conn net.Conn, command bool) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			next, err := frame.Read(conn)
			if err != nil {
				// The client is gone. Closing the console detaches; the
				// container and whatever runs in it stay up.
				_ = console.Close()
				return
			}
			switch next.Type {
			case frame.Input:
				if _, err := console.Write(next.Payload); err != nil {
					return
				}
			case frame.Resize:
				size, err := frame.DecodeResize(next.Payload)
				if err != nil {
					continue
				}
				_ = console.Resize(ctx, int(size.Rows), int(size.Cols))
			case frame.CloseInput:
				if command {
					_ = console.CloseWrite()
				}
			}
		}
	}()

	// stdout and stderr can arrive concurrently, but frames must stay atomic.
	var writeMu sync.Mutex
	writeFrame := func(kind byte, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return frame.Write(conn, kind, payload)
	}
	var output sync.WaitGroup
	outputErrors := make(chan error, 2)
	copyOutput := func(reader io.Reader, kind byte) {
		defer output.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				if err := writeFrame(kind, buf[:n]); err != nil {
					_ = console.Close()
					return
				}
			}
			if err != nil {
				if command && !errors.Is(err, io.EOF) {
					outputErrors <- err
					_ = console.Close()
				}
				return
			}
		}
	}
	output.Add(1)
	go copyOutput(console, frame.Stdout)
	if stderr := console.Stderr(); stderr != nil {
		output.Add(1)
		go copyOutput(stderr, frame.Stderr)
	}
	output.Wait()
	select {
	case err := <-outputErrors:
		_ = frame.Write(conn, frame.Error, []byte("read pool console output: "+err.Error()))
	default:
		writePoolConsoleExit(ctx, console, conn, command)
	}
	_ = conn.Close()
	<-done
}

// writePoolConsoleExit reports the console shell's exit code when the container
// stopped, and reports a plain detach otherwise. The container outliving the
// stream is the normal case, so the wait for a status is deliberately brief.
func writePoolConsoleExit(ctx context.Context, console sandbox.Stream, conn net.Conn, command bool) {
	waitCtx := ctx
	if !command {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, consoleExitWait)
		defer cancel()
	}
	code, err := console.Wait(waitCtx)
	if err != nil {
		status, message := "detached", ""
		if command {
			status, message = "failed", err.Error()
		}
		payload, encodeErr := frame.EncodeExit(status, nil, message)
		if encodeErr == nil {
			_ = frame.Write(conn, frame.Exit, payload)
		}
		return
	}
	exitCode := int64(code)
	payload, err := frame.EncodeExit("exited", &exitCode, "")
	if err != nil {
		return
	}
	_ = frame.Write(conn, frame.Exit, payload)
}

func consoleDimension(value string) int {
	size, err := strconv.Atoi(value)
	if err != nil || size <= 0 || size > 0xffff {
		return 0
	}
	return size
}
