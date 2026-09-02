package server

import (
	"io"
	"net/http"
	"strconv"
	"sync"

	"github.com/go-chi/chi/v5"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// poolLogSourceHeader names what the backend actually opened — a guest serial
// console, a Docker daemon journal — so the operator reading the body knows
// which record they got. There is no uniform pool host log for the body alone
// to imply one.
const poolLogSourceHeader = "X-Discobox-Pool-Log-Source"

// maxPoolLogTail bounds the requested line count so a typo cannot ask a driver
// to scan back through a console log without limit.
const maxPoolLogTail = 1_000_000

// registerPoolLogsRoutes exposes the pool host's backend log.
//
// It is hand-wired next to the console for the same reason (see
// pool_console.go): nothing is forwarded to a pool agent, because the log is
// read through the provider driver and has to answer on a host whose agent
// never started. It is a plain streaming GET rather than a websocket — the
// bytes only travel one way — and the response is flushed as it arrives so a
// followed log reaches the operator live.
func registerPoolLogsRoutes(router chi.Router, service services.PoolService) {
	router.Method(http.MethodGet, "/api/projects/{projectId}/pools/{poolId}/logs", poolLogsHandler(service))
}

func poolLogsHandler(service services.PoolService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if service == nil {
			writeSandboxAgentProxyError(w, http.StatusServiceUnavailable, "pool service is not configured")
			return
		}
		opts := sandbox.PoolLogOptions{
			Tail:   poolLogTail(r.URL.Query().Get("tail")),
			Follow: r.URL.Query().Get("follow") == "true",
		}
		stream, err := service.OpenPoolLogs(r.Context(), chi.URLParam(r, "projectId"), chi.URLParam(r, "poolId"), opts)
		if err != nil {
			// Reported before any of the body is written: once bytes are
			// flowing, "this backend keeps no log" can no longer be a status.
			writeSandboxAgentProxyError(w, statusCodeForProxyError(err), err.Error())
			return
		}
		// The stream is what holds the driver's resources — a journalctl on the
		// pool host, an SSH connection to a droplet — and a read of an idle
		// followed log blocks until the next line, which may never come. So the
		// client going away closes it here rather than waiting for the copy
		// below to notice on its next write.
		var closeOnce sync.Once
		closeStream := func() { closeOnce.Do(func() { _ = stream.Close() }) }
		defer closeStream()
		copying := make(chan struct{})
		defer close(copying)
		go func() {
			select {
			case <-r.Context().Done():
				closeStream()
			case <-copying:
			}
		}()

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set(poolLogSourceHeader, stream.Source)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		// A read error ends the response rather than appending anything to it:
		// the body is the host's log, and nothing this handler has to say
		// belongs inside it.
		_, _ = io.Copy(&flushingWriter{writer: w, controller: http.NewResponseController(w)}, stream)
	})
}

// flushingWriter pushes every chunk to the client as it is written. Without it
// a followed log sits in the response buffer until enough of it accumulates,
// which is the opposite of what following is for.
type flushingWriter struct {
	writer     io.Writer
	controller *http.ResponseController
}

func (f *flushingWriter) Write(p []byte) (int, error) {
	n, err := f.writer.Write(p)
	if err != nil {
		return n, err
	}
	// A response that cannot be flushed still gets written; buffering is a
	// latency problem, not a correctness one.
	_ = f.controller.Flush()
	return n, nil
}

func poolLogTail(value string) int {
	tail, err := strconv.Atoi(value)
	if err != nil || tail <= 0 {
		return 0
	}
	if tail > maxPoolLogTail {
		return maxPoolLogTail
	}
	return tail
}
