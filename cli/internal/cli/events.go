package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// sandboxEventReset is the stream's control event: it could not go on from
// the cursor, and the events after it are the current state
// (ADR 26-10-29-316 §4).
const sandboxEventReset = "reset"

// defaultEventsRetry is how long a dropped stream waits before reconnecting
// until the server has said (`retry:`).
const defaultEventsRetry = 2 * time.Second

// newEventsCommand implements `discobox events`: the project's sandbox state
// changes as they happen, over the server's event stream (ADR 26-10-29-316).
func (a *App) newEventsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "events [DISCOBOX]",
		Short: "Print discoboxes' state changes as they happen",
		Long: `Print a line each time a discobox in the project is created, archived or
deleted, powers up or down, or the program in it starts or stops working,
finishes, or is blocked on a person.

It begins with each discobox's state now, then follows. A dropped connection is
resumed where it left off: no event is printed twice and none is skipped. When
the server no longer has the events since then, it says so ("reset") and
prints each discobox's state again.

With DISCOBOX, only that discobox's events are printed. DISCOBOX is the NAME
"discobox ls" shows, its ID, or a short prefix of that ID.

With -o json each event is one line of JSON: its id, its event type and its
data, the discobox's state as the server documents it (SandboxEventData).`,
		Example: `  discobox events
  discobox events mybox
  discobox events -o json | jq 'select(.data.program.state == "blocked")'`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			sandboxID := ""
			if len(args) == 1 {
				apiClient, err := a.apiClient()
				if err != nil {
					return err
				}
				if sandboxID, err = a.resolveListedSandbox(cmd.Context(), apiClient, projectID, args[0]); err != nil {
					return err
				}
			}
			return a.followSandboxEvents(cmd, projectID, sandboxID)
		},
	}
	return cmd
}

// sandboxEvent is one event as the stream sent it. Its data is decoded
// through a local struct, as the server documents it (SandboxEventData in
// server.yaml): the schema has no generated type, since no operation's body
// is it.
type sandboxEvent struct {
	ID    string           `json:"id,omitempty"`
	Event string           `json:"event"`
	Data  sandboxEventData `json:"data"`
}

type sandboxEventData struct {
	ProjectID    string               `json:"projectId,omitempty"`
	SandboxID    string               `json:"sandboxId,omitempty"`
	Name         string               `json:"name,omitempty"`
	At           *time.Time           `json:"at,omitempty"`
	DesiredState string               `json:"desiredState,omitempty"`
	State        string               `json:"state,omitempty"`
	RuntimeState string               `json:"runtimeState,omitempty"`
	Program      *sandboxEventProgram `json:"program,omitempty"`
}

type sandboxEventProgram struct {
	State string `json:"state"`
	Kind  string `json:"kind,omitempty"`
	App   string `json:"app,omitempty"`
}

// followSandboxEvents prints events until the command is interrupted,
// reconnecting with the last event's ID whenever the stream drops. The first
// connection's failure is the command's; a later one is waited out, since the
// cursor makes a reconnect lose nothing.
func (a *App) followSandboxEvents(cmd *cobra.Command, projectID, sandboxID string) error {
	ctx := cmd.Context()
	cursor := ""
	retry := defaultEventsRetry
	connected := false
	for {
		opened, err := a.readSandboxEvents(ctx, projectID, cursor, func(field, value string) {
			if field == "retry" {
				if ms, err := strconv.Atoi(value); err == nil && ms > 0 {
					retry = time.Duration(ms) * time.Millisecond
				}
			}
		}, func(event sandboxEvent) error {
			if event.ID != "" {
				cursor = event.ID
			}
			// An event with only an ID moves the cursor and says nothing.
			if event.Event == "" {
				return nil
			}
			if sandboxID != "" && event.Event != sandboxEventReset && event.Data.SandboxID != sandboxID {
				return nil
			}
			if err := a.writeSandboxEvent(cmd, event); err != nil {
				return eventsOutputError{err}
			}
			return nil
		})
		connected = connected || opened
		select {
		case <-ctx.Done():
			// Interrupted, which is how a follow ends.
			return nil
		default:
		}
		var refused *sandboxEventsRefused
		var output eventsOutputError
		if !connected || errors.As(err, &refused) || errors.As(err, &output) {
			if err == nil {
				err = errors.New("the server ended the event stream")
			}
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(retry):
		}
	}
}

// eventsOutputError is a failure to print an event — a closed pipe — which
// no reconnect mends.
type eventsOutputError struct{ err error }

func (e eventsOutputError) Error() string { return e.err.Error() }
func (e eventsOutputError) Unwrap() error { return e.err }

// sandboxEventsRefused is a stream the server answered with an error: asking
// again will not change the answer.
type sandboxEventsRefused struct{ message string }

func (e *sandboxEventsRefused) Error() string { return "read discobox events: " + e.message }

// readSandboxEvents reads one connection's events, from after cursor, until
// it ends. It reports whether the server opened the stream.
func (a *App) readSandboxEvents(ctx context.Context, projectID, cursor string, onField func(field, value string), onEvent func(sandboxEvent) error) (bool, error) {
	baseURL, httpClient, err := a.httpClient()
	if err != nil {
		return false, err
	}
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return false, err
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/projects/" + url.PathEscape(projectID) + "/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if cursor != "" {
		req.Header.Set("Last-Event-ID", cursor)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("read discobox events: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		message := attachErrorMessage(body)
		if message == "" {
			message = resp.Status
		}
		if resp.StatusCode >= http.StatusInternalServerError {
			return false, errors.New("read discobox events: " + message)
		}
		return false, &sandboxEventsRefused{message: message}
	}
	return true, parseServerSentEvents(resp.Body, onField, func(id, event, data string) error {
		decoded := sandboxEvent{ID: id, Event: event}
		if data != "" {
			if err := json.Unmarshal([]byte(data), &decoded.Data); err != nil {
				return fmt.Errorf("read discobox events: event %s: %w", id, err)
			}
		}
		return onEvent(decoded)
	})
}

// parseServerSentEvents reads an event stream (the HTML server-sent events
// format) and hands each event to onEvent as it ends. The ID passed is the
// event's own, empty when it set none: the caller keeps the cursor. A block
// with an ID and nothing else is handed on with no type, as the cursor move
// it is: an EventSource takes its ID and dispatches nothing.
func parseServerSentEvents(r io.Reader, onField func(field, value string), onEvent func(id, event, data string) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var id, event string
	var data []string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			switch {
			case len(data) > 0 || event != "":
				if event == "" {
					event = "message"
				}
				if err := onEvent(id, event, strings.Join(data, "\n")); err != nil {
					return err
				}
			case id != "":
				if err := onEvent(id, "", ""); err != nil {
					return err
				}
			}
			id, event, data = "", "", nil
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			id = value
		case "event":
			event = value
		case "data":
			data = append(data, value)
		default:
			onField(field, value)
		}
	}
	return scanner.Err()
}

// writeSandboxEvent prints one event: a line of JSON with -o json, otherwise
// when, what happened, which discobox, and its state.
func (a *App) writeSandboxEvent(cmd *cobra.Command, event sandboxEvent) error {
	if a.output == "json" {
		return writeTerminalSafeJSONLine(cmd, event)
	}
	out := cmd.OutOrStdout()
	if event.Event == sandboxEventReset {
		_, err := fmt.Fprintln(out, "reset: events were missed; each discobox's state follows")
		return err
	}
	data := event.Data
	at := ""
	if data.At != nil {
		at = data.At.Local().Format(time.DateTime)
	}
	what := strings.TrimPrefix(event.Event, "sandbox.")
	fields := []string{at, fmt.Sprintf("%-7s", what), terminalSafe(data.Name), terminalSafe(data.SandboxID)}
	fields = append(fields, "state="+terminalSafe(data.State))
	if data.DesiredState != "" && data.DesiredState != "present" {
		fields = append(fields, "desired="+terminalSafe(data.DesiredState))
	}
	power := data.RuntimeState
	if power == "" {
		power = "-"
	}
	fields = append(fields, "power="+terminalSafe(power))
	if program := data.Program; program != nil {
		agent := program.State
		if program.Kind != "" {
			agent += "(" + program.Kind + ")"
		}
		if program.App != "" {
			agent += " " + program.App
		}
		fields = append(fields, "agent="+terminalSafe(agent))
	}
	_, err := fmt.Fprintln(out, strings.Join(fields, "  "))
	return err
}
