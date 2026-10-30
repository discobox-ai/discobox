package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	services "github.com/discobox-ai/discobox/server/internal/services"
)

// The project event stream's parameters (ADR 26-10-30-868 §1).
const (
	defaultProjectEventHistory = 100
	maxProjectEventHistory     = 1000
	// projectEventRetry is the reconnect delay the stream tells its client.
	projectEventRetry = 2 * time.Second
)

// projectEventKeepalive is how long the stream stays silent before it writes
// a comment, so a proxy between it and its client keeps the connection. A
// variable so a test can shorten it.
var projectEventKeepalive = 15 * time.Second

// registerProjectEventRoutes serves a project's event stream.
//
// It is hand-wired, like the pool's logs, because the generated server copies
// a text/event-stream body in one piece and flushes nothing: an event has to
// reach its client when it is written. The route is in the OpenAPI document
// for its contract, with a stub the router never reaches.
func registerProjectEventRoutes(router chi.Router, service services.EventService) {
	router.Method(http.MethodGet, "/projects/{projectId}/events", projectEventsHandler(service))
}

func projectEventsHandler(service services.EventService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if service == nil {
			writeProblem(w, http.StatusServiceUnavailable, "the event service is not configured")
			return
		}
		history := defaultProjectEventHistory
		if raw := r.URL.Query().Get("history"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 0 || parsed > maxProjectEventHistory {
				writeProblem(w, http.StatusBadRequest, fmt.Sprintf("history must be a whole number from 0 to %d", maxProjectEventHistory))
				return
			}
			history = parsed
		}
		stream, err := service.OpenProjectEvents(r.Context(), chi.URLParam(r, "projectId"), services.ProjectEventStart{
			LastEventID: r.Header.Get("Last-Event-ID"),
			History:     history,
		})
		if err != nil {
			writeProblem(w, statusCodeForProxyError(err), err.Error())
			return
		}
		defer stream.Close()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		// An nginx in front would otherwise hold the events back to fill its
		// buffer.
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		out := &sseWriter{w: w, flush: http.NewResponseController(w)}
		// The ID alone sets a client's last event ID (as EventSource does for
		// a frame with no data), so one that drops before any event resumes
		// from here rather than replaying the history again.
		out.printf("retry: %d\nid: %d\n\n", projectEventRetry.Milliseconds(), stream.Position())
		if reason := stream.Reset(); reason != "" {
			data, err := json.Marshal(map[string]string{"reason": reason})
			if err != nil {
				return
			}
			out.printf("event: reset\ndata: %s\n\n", data)
		}
		for out.err == nil {
			if !out.send() {
				return
			}
			next, cancel := context.WithTimeout(r.Context(), projectEventKeepalive)
			event, err := stream.Next(next)
			cancel()
			switch {
			case err == nil:
				out.printf("id: %d\nevent: %s\ndata: %s\n\n", event.ID, event.Type, event.Data)
			case errors.Is(err, context.DeadlineExceeded) && r.Context().Err() == nil:
				// With an ID when the stream has nothing waiting, so a quiet
				// project's client resumes from where the log now is, not
				// from where it connected, which retention eventually removes.
				if through, ok := stream.Through(); ok {
					out.printf(": keepalive\nid: %d\n\n", through)
				} else {
					out.printf(": keepalive\n\n")
				}
			case errors.Is(err, services.ErrProjectEventStreamBehind):
				// The response ends; the client resumes from the last event
				// it read and loses nothing.
				slog.InfoContext(r.Context(), "an event stream fell behind and was ended", "projectId", chi.URLParam(r, "projectId"))
				return
			default:
				if r.Context().Err() == nil {
					slog.WarnContext(r.Context(), "an event stream ended on a read error", "projectId", chi.URLParam(r, "projectId"), "error", err)
				}
				return
			}
		}
	})
}

// sseWriter writes SSE frames and keeps the first write error, after which
// it writes nothing.
type sseWriter struct {
	w     http.ResponseWriter
	flush *http.ResponseController
	err   error
	// unsent is whether a frame has been written since the last flush.
	unsent bool
}

func (s *sseWriter) printf(format string, args ...any) {
	if s.err != nil {
		return
	}
	_, s.err = fmt.Fprintf(s.w, format, args...)
	s.unsent = true
}

// send flushes what has been written, reporting whether the stream is still
// writable.
func (s *sseWriter) send() bool {
	if s.err == nil && s.unsent {
		s.err = s.flush.Flush()
		s.unsent = false
	}
	return s.err == nil
}

// writeProblem answers with the ErrorModel the route's OpenAPI document
// names, as the generated routes do.
func writeProblem(w http.ResponseWriter, status int, detail string) {
	body, err := json.Marshal(map[string]any{"status": status, "title": http.StatusText(status), "detail": detail})
	if err != nil {
		http.Error(w, detail, status)
		return
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
