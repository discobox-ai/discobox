package server

import (
	"context"
	"io"
	"net/http"
	"sync"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"github.com/discobox-ai/discobox/pkg/execstream/frame"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// registerPoolSnapshotBuildRoutes exposes a pool's snapshot builds beside its
// console and logs (ADR 26-11-23-004 §4, ADR 26-11-18-571 §1): a build's
// feature logs, and a command or shell in its box. Unlike those two, both
// are forwarded to the pool agent, which alone holds the build's box on
// every runtime. The server keeps nothing of a build: the key is the pool's,
// as its report lists it. They are authorized as the console is.
func registerPoolSnapshotBuildRoutes(router chi.Router, service services.PoolService) {
	router.Method(http.MethodGet, "/projects/{projectId}/pools/{poolId}/snapshot-builds/{key}/logs", poolSnapshotBuildLogsHandler(service))
	router.Method(http.MethodGet, "/projects/{projectId}/pools/{poolId}/snapshot-builds/{key}/exec", poolSnapshotBuildExecHandler(service))
}

// poolSnapshotBuildLogsHandler streams a build's feature logs as text:
// `feature` names one, `tail` how many lines of each, and `follow` keeps
// reading until the build ends.
func poolSnapshotBuildLogsHandler(service services.PoolService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if service == nil {
			writeSandboxAgentProxyError(w, http.StatusServiceUnavailable, "pool service is not configured")
			return
		}
		query := r.URL.Query()
		opts := sandbox.SnapshotBuildLogOptions{
			Feature: query.Get("feature"),
			Tail:    poolLogTail(query.Get("tail")),
			Follow:  query.Get("follow") == "true",
		}
		stream, err := service.OpenSnapshotBuildLogs(r.Context(), chi.URLParam(r, "projectId"), chi.URLParam(r, "poolId"), chi.URLParam(r, "key"), opts)
		if err != nil {
			writeSandboxAgentProxyError(w, statusCodeForProxyError(err), err.Error())
			return
		}
		// A followed read blocks until the next line, which may never come:
		// the client leaving closes the stream rather than the next write.
		var closeOnce sync.Once
		closeStream := func() { closeOnce.Do(func() { _ = stream.Close() }) }
		defer closeStream()
		stop := context.AfterFunc(r.Context(), closeStream)
		defer stop()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(&flushingWriter{writer: w, controller: http.NewResponseController(w)}, stream)
	})
}

// poolSnapshotBuildExecHandler bridges the caller's WebSocket to the pool
// agent's exec in a build's box, frame bytes passed through unread: `command`
// (repeated) is its argv, none a shell, and `tty` asks for a terminal of
// `rows` by `cols`, as the console's route takes them.
func poolSnapshotBuildExecHandler(service services.PoolService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if service == nil {
			writeSandboxAgentProxyError(w, http.StatusServiceUnavailable, "pool service is not configured")
			return
		}
		query := r.URL.Query()
		opts := sandbox.ConsoleOptions{
			Command: query["command"],
			TTY:     query.Get("tty") == "true",
			Rows:    consoleDimension(query.Get("rows")),
			Cols:    consoleDimension(query.Get("cols")),
		}
		// Accept validates the handshake and Origin before anything runs in
		// the build. What the pool refuses travels in the framed stream.
		socket, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer socket.Close(websocket.StatusNormalClosure, "done")
		socket.SetReadLimit(-1)
		client := websocket.NetConn(r.Context(), socket, websocket.MessageBinary)
		defer client.Close()
		build, err := service.DialSnapshotBuildExec(r.Context(), chi.URLParam(r, "projectId"), chi.URLParam(r, "poolId"), chi.URLParam(r, "key"), opts)
		if err != nil {
			_ = frame.Write(client, frame.Error, []byte(err.Error()))
			return
		}
		defer build.Close()
		done := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(build, client); _ = build.Close(); done <- struct{}{} }()
		go func() { _, _ = io.Copy(client, build); _ = client.Close(); done <- struct{}{} }()
		<-done
		_ = client.Close()
		_ = build.Close()
		<-done
	})
}
