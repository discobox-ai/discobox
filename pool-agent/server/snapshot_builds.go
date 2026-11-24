package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"

	"github.com/coder/websocket"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/go-chi/chi/v5"

	"github.com/discobox-ai/discobox/pkg/execstream/frame"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// A snapshot build is reached through the guest the pool agent holds for it,
// by its key, on every runtime (ADR 26-11-23-004 §4): its feature logs, read
// by the pool agent's own fixed tail, and a command or shell in its box,
// which keeps its result from being committed (§5). The server forwards both
// from its admin routes; neither is in the generated API, since each is an
// unbounded stream.

// snapshotBuildShell is what an exec with no command runs: the box's login
// shell, bash where the base has it.
var snapshotBuildShell = []string{"/bin/sh", "-c", `if command -v bash >/dev/null 2>&1; then exec bash -l; fi; exec sh -l`}

func registerSnapshotBuildRoutes(router chi.Router, service *sandboxService) {
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/snapshot-builds/{key}/logs", service.snapshotBuildLogsHandler())
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/snapshot-builds/{key}/exec", service.snapshotBuildExecHandler())
}

// snapshotBuildLogsHandler streams a build's feature logs as text: `feature`
// names one, `tail` how many lines of each, and `follow` keeps reading until
// the build ends.
func (s *sandboxService) snapshotBuildLogsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.authorizeTree(r, ScopeSnapshotBuildRead); err != nil {
			writeProblem(w, statusCodeForTreeError(err), err.Error())
			return
		}
		query := r.URL.Query()
		req := sandboxruntime.SnapshotLogsRequest{Feature: query.Get("feature"), Follow: query.Get("follow") == "true"}
		if tail := query.Get("tail"); tail != "" {
			lines, err := strconv.Atoi(tail)
			if err != nil || lines < 0 {
				writeProblem(w, http.StatusBadRequest, "tail must be a count of lines")
				return
			}
			req.Lines = lines
		}
		out := &startedWriter{w: w}
		err := s.runtime.SnapshotBuildLogs(r.Context(), chi.URLParam(r, "key"), req, out)
		if err != nil && !out.started {
			writeProblem(w, statusCodeForSnapshotBuildError(err), err.Error())
		}
	})
}

// startedWriter writes a streamed body, flushing each write, and reports
// whether it has begun: an error before it has is still the response's.
type startedWriter struct {
	w       http.ResponseWriter
	started bool
}

func (s *startedWriter) Write(p []byte) (int, error) {
	if !s.started {
		s.w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		s.w.WriteHeader(http.StatusOK)
		s.started = true
	}
	n, err := s.w.Write(p)
	if flusher, ok := s.w.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}

// snapshotBuildExecHandler runs a command, or a shell, in a build's box over
// a WebSocket carrying execstream frames, as `admin pool console` does:
// `command` (repeated) is its argv, none a login shell on a terminal, and
// `tty` asks for a terminal of `rows` by `cols`. The session ends when the command exits, the caller leaves, or the
// build ends.
func (s *sandboxService) snapshotBuildExecHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.authorizeTree(r, ScopeSnapshotBuildExec); err != nil {
			writeProblem(w, statusCodeForTreeError(err), err.Error())
			return
		}
		query := r.URL.Query()
		req := machine.ExecRequest{Argv: query["command"], TTY: query.Get("tty") == "true", Rows: dimension(query.Get("rows")), Cols: dimension(query.Get("cols"))}
		command := len(req.Argv) > 0
		if !command {
			// A shell is a terminal, as a console's is.
			req.Argv, req.TTY = snapshotBuildShell, true
		}
		socket, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer socket.Close(websocket.StatusNormalClosure, "done")
		conn := websocket.NetConn(r.Context(), socket, websocket.MessageBinary)
		defer conn.Close()
		process, ended, err := s.runtime.SnapshotBuildExec(r.Context(), chi.URLParam(r, "key"), req)
		if err != nil {
			_ = frame.Write(conn, frame.Error, []byte(err.Error()))
			return
		}
		defer process.Close()
		pumpSnapshotBuildExec(ended, process, conn, req.TTY)
	})
}

// pumpSnapshotBuildExec moves frames between conn and process until the
// process exits, the caller leaves, or ended does, then reports how the
// session ended.
func pumpSnapshotBuildExec(ended context.Context, process machine.Process, conn net.Conn, tty bool) {
	stop := context.AfterFunc(ended, func() { _ = process.Close() })
	defer stop()
	var writeMu sync.Mutex
	writeFrame := func(kind byte, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return frame.Write(conn, kind, payload)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			next, err := frame.Read(conn)
			if err != nil {
				// The caller is gone: its command goes with it.
				_ = process.Close()
				return
			}
			switch next.Type {
			case frame.Input:
				if _, err := process.Write(next.Payload); err != nil {
					return
				}
			case frame.Resize:
				if size, err := frame.DecodeResize(next.Payload); err == nil {
					_ = process.Resize(size.Rows, size.Cols)
				}
			case frame.CloseInput:
				// A terminal has no end of input: its session ends when its
				// process does.
				if !tty {
					_ = process.CloseStdin()
				}
			}
		}
	}()
	code, err := process.Wait(&frameWriter{kind: frame.Stdout, write: writeFrame}, &frameWriter{kind: frame.Stderr, write: writeFrame})
	var payload []byte
	switch {
	case ended.Err() != nil:
		payload, _ = frame.EncodeExit("failed", nil, "the snapshot build ended, and its box with it")
	case err != nil:
		payload, _ = frame.EncodeExit("failed", nil, err.Error())
	default:
		exitCode := int64(code)
		payload, _ = frame.EncodeExit("exited", &exitCode, "")
	}
	if payload != nil {
		_ = writeFrame(frame.Exit, payload)
	}
	_ = conn.Close()
	<-done
}

// frameWriter writes each write as one frame of kind.
type frameWriter struct {
	kind  byte
	write func(byte, []byte) error
}

func (f *frameWriter) Write(p []byte) (int, error) {
	if err := f.write(f.kind, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// dimension is a terminal size from a query value, or zero for none.
func dimension(value string) uint16 {
	size, err := strconv.Atoi(value)
	if err != nil || size <= 0 || size > 0xffff {
		return 0
	}
	return uint16(size)
}

// statusCodeForSnapshotBuildError is how a refused read or exec of a build
// is answered.
func statusCodeForSnapshotBuildError(err error) int {
	switch {
	case errors.Is(err, sandboxruntime.ErrSnapshotBuildNotFound), errors.Is(err, sandboxruntime.ErrSnapshotFeatureUnknown):
		return http.StatusNotFound
	case errors.Is(err, sandboxruntime.ErrSnapshotBuildQueued), errors.Is(err, sandboxruntime.ErrSnapshotBuildNoFeatures), errors.Is(err, sandboxruntime.ErrSnapshotBuildProvisioned):
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}
