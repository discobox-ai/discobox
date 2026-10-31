package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	services "github.com/discobox-ai/discobox/server/internal/services"
)

// The project's sandbox events, as server-sent events (ADR 26-10-29-316 §4).
// The route is hand-wired, as the server's other streams are: ogen's handler
// writes a response once, and every event here has to be flushed as it is
// read.
const (
	// sandboxEventPollInterval is how often a stream reads the events after
	// its cursor. It polls the table, as ADR 0081's wait polls its rows, so
	// every server sharing the database serves every change.
	sandboxEventPollInterval = 500 * time.Millisecond
	// sandboxEventPingInterval is how long an idle stream goes before a
	// comment, so that a proxy between does not reap the connection.
	sandboxEventPingInterval = 15 * time.Second
	// sandboxEventRetry is what a client is told to wait before reconnecting.
	sandboxEventRetry = 2 * time.Second
)

func registerSandboxEventRoutes(router chi.Router, service services.SandboxService) {
	router.Method(http.MethodGet, "/api/projects/{projectId}/events", sandboxEventsHandler(service, sandboxEventPollInterval, sandboxEventPingInterval))
}

func sandboxEventsHandler(service services.SandboxService, poll, ping time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if service == nil {
			writeSandboxAgentProxyError(w, http.StatusServiceUnavailable, "sandbox service is not configured")
			return
		}
		lastEventID, err := sandboxEventCursor(r)
		if err != nil {
			writeSandboxAgentProxyError(w, http.StatusBadRequest, err.Error())
			return
		}
		ctx := r.Context()
		stream, err := service.OpenSandboxEvents(ctx, chi.URLParam(r, "projectId"), lastEventID)
		if err != nil {
			writeSandboxAgentProxyError(w, statusCodeForProxyError(err), err.Error())
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeSandboxAgentProxyError(w, http.StatusInternalServerError, "the response cannot be streamed")
			return
		}
		// The stream outlives any write deadline the server set for ordinary
		// responses.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		var frame bytes.Buffer
		send := func() bool {
			if _, err := w.Write(frame.Bytes()); err != nil {
				return false
			}
			frame.Reset()
			flusher.Flush()
			return true
		}
		frame.WriteString("retry: " + strconv.FormatInt(sandboxEventRetry.Milliseconds(), 10) + "\n\n")
		if !send() {
			return
		}
		ticker := time.NewTicker(poll)
		defer ticker.Stop()
		lastWrite := time.Now()
		// sent is the ID the client holds: the one it came with, or the last
		// this stream gave it.
		sent := int64(-1)
		if lastEventID != nil {
			sent = *lastEventID
		}
		for {
			events, err := stream.Next(ctx)
			if err != nil {
				if ctx.Err() == nil {
					// Past the first byte there is no status left to set: the
					// stream ends, and the client resumes from its cursor.
					logSandboxEventStreamError(ctx, err)
				}
				return
			}
			for _, event := range events {
				writeSandboxEvent(&frame, event)
				if event.ID != nil {
					sent = *event.ID
				}
			}
			// The stream can stand past the last event it sent: after a
			// replay, whose events are old, and while the project is quiet.
			// An event with only an ID moves the client's cursor there
			// without dispatching anything, so a reconnect resumes from a
			// cursor retention has not passed. While quiet it is sent only
			// as often as a ping would be, in a ping's place.
			idle := len(events) == 0 && time.Since(lastWrite) >= ping
			if cursor := stream.Cursor(); cursor != sent && (len(events) > 0 || idle) {
				frame.WriteString("id: " + strconv.FormatInt(cursor, 10) + "\n\n")
				sent = cursor
			} else if idle {
				frame.WriteString(": ping\n\n")
			}
			if frame.Len() > 0 {
				if !send() {
					return
				}
				lastWrite = time.Now()
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
}

// sandboxEventCursor is the event the client last had: the Last-Event-ID
// header an EventSource sends when it reconnects, or a lastEventId query
// parameter for a first connection that cannot set a header. The header
// wins.
func sandboxEventCursor(r *http.Request) (*int64, error) {
	value := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if value == "" {
		value = strings.TrimSpace(r.URL.Query().Get("lastEventId"))
	}
	if value == "" {
		return nil, nil
	}
	seq, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seq < 0 {
		return nil, errSandboxEventCursor
	}
	return &seq, nil
}

var errSandboxEventCursor = errors.New("the last event ID is not one this stream sent: it is a whole number")

// writeSandboxEvent writes one event. Its data is one line of JSON, so it
// needs one data field.
func writeSandboxEvent(frame *bytes.Buffer, event services.SandboxStreamEvent) {
	if event.ID != nil {
		frame.WriteString("id: " + strconv.FormatInt(*event.ID, 10) + "\n")
	}
	frame.WriteString("event: " + event.Type + "\n")
	frame.WriteString("data: ")
	frame.Write(event.Data)
	frame.WriteString("\n\n")
}

func logSandboxEventStreamError(ctx context.Context, err error) {
	slog.WarnContext(ctx, "sandbox event stream ended", "error", err)
}
