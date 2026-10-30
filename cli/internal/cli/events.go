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

// The project event stream (ADR 26-10-30-868 §8).
const (
	defaultEventHistory = 100
	maxEventHistory     = 1000
	// defaultEventRetry is how long to wait before reconnecting until the
	// server says otherwise.
	defaultEventRetry = 2 * time.Second
)

// newEventsCommand implements `discobox events`: the project's event stream,
// followed until interrupted and resumed across dropped connections from the
// last event it printed.
func (a *App) newEventsCommand() *cobra.Command {
	var history int
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Stream changes to the project's discoboxes",
		Long: `Stream changes to the project's discoboxes as they happen, until interrupted.

Each line is one event:

  sandbox.created   a discobox was made
  sandbox.updated   its state, its power, or what its agent is doing changed;
                    archiving and restoring are updates whose state says so
  sandbox.deleted   it is gone

and carries the discobox's name, its state, its power (starting, running,
stopping, stopped, suspended) and its agent's state while it runs (idle,
working, done, blocked, error, with what a blocked agent needs). With -o json
each event is printed as the server sent it, one JSON object a line: its id,
its event type, and its data. More kinds of event may be added; they are
printed by type.

The stream starts with the last --history events. A dropped connection is
resumed from the last event printed, so nothing is missed or repeated.
Events are kept for 24 hours: a stream away longer starts again with
--history events, and says so.`,
		Example: `  discobox events
  discobox events --history 0
  discobox events -o json | jq 'select(.data.agent.state == "blocked")'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if history < 0 || history > maxEventHistory {
				return fmt.Errorf("--history must be from 0 to %d", maxEventHistory)
			}
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			return a.streamEvents(cmd.Context(), projectID, history, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().IntVar(&history, "history", defaultEventHistory, "Print the last N events before following live (0 to 1000)")
	return cmd
}

// eventStreamRefused is a response that reconnecting will not change.
type eventStreamRefused struct{ err error }

func (e eventStreamRefused) Error() string { return e.err.Error() }
func (e eventStreamRefused) Unwrap() error { return e.err }

// eventStreamState is what carries over from one connection to the next.
type eventStreamState struct {
	lastID string
	retry  time.Duration
	// connected is set once a stream has opened, after which a failure is a
	// drop to reconnect from rather than a server that is not there.
	connected bool
}

// streamEvents follows the stream, reconnecting from the last event ID until
// ctx ends.
func (a *App) streamEvents(ctx context.Context, projectID string, history int, stdout, stderr io.Writer) error {
	state := &eventStreamState{retry: defaultEventRetry}
	printer := &eventPrinter{json: a.output == "json", out: stdout, errOut: stderr, history: history}
	// Interrupting the command is how it finishes, so a stream cut short by
	// ctx is no error.
	for {
		err := a.readEventStream(ctx, projectID, history, state, printer)
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		var refused eventStreamRefused
		if errors.As(err, &refused) || !state.connected {
			return err
		}
		// A stream the server ended resumes quietly; anything else is said.
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			fmt.Fprintf(stderr, "event stream dropped (%v); resuming\n", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(state.retry):
		}
	}
}

// readEventStream reads one connection's events to the end.
func (a *App) readEventStream(ctx context.Context, projectID string, history int, state *eventStreamState, printer *eventPrinter) error {
	baseURL, httpClient, err := a.httpClient()
	if err != nil {
		return eventStreamRefused{err}
	}
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return eventStreamRefused{err}
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/projects/" + url.PathEscape(projectID) + "/events"
	u.RawQuery = url.Values{"history": {strconv.Itoa(history)}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return eventStreamRefused{err}
	}
	req.Header.Set("Accept", "text/event-stream")
	if state.lastID != "" {
		req.Header.Set("Last-Event-ID", state.lastID)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("open event stream: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		err := fmt.Errorf("open event stream: %s", eventStreamErrorMessage(resp.Status, body))
		if resp.StatusCode < http.StatusInternalServerError {
			return eventStreamRefused{err}
		}
		return err
	}
	state.connected = true
	return readServerSentEvents(resp.Body, state, printer.print)
}

// eventStreamErrorMessage is the detail of the problem the server answered
// with, or its status.
func eventStreamErrorMessage(status string, body []byte) string {
	var problem struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(body, &problem); err == nil && strings.TrimSpace(problem.Detail) != "" {
		return strings.TrimSpace(problem.Detail)
	}
	if message := attachErrorMessage(body); message != "" {
		return message
	}
	return status
}

// serverSentEvent is one dispatched event.
type serverSentEvent struct {
	id    string
	event string
	data  string
}

// readServerSentEvents parses an SSE body as the HTML standard does, keeping
// the last event ID and the reconnect delay in state. A frame with an id but
// no data dispatches nothing and still moves the last event ID, which is how
// the server tells a stream where it starts.
func readServerSentEvents(body io.Reader, state *eventStreamState, dispatch func(serverSentEvent) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var event, data strings.Builder
	hasData := false
	idBuffer := state.lastID
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			state.lastID = idBuffer
			if hasData {
				if err := dispatch(serverSentEvent{id: idBuffer, event: event.String(), data: data.String()}); err != nil {
					return err
				}
			}
			event.Reset()
			data.Reset()
			hasData = false
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event.Reset()
			event.WriteString(value)
		case "data":
			if hasData {
				data.WriteByte('\n')
			}
			data.WriteString(value)
			hasData = true
		case "id":
			if !strings.ContainsRune(value, 0) {
				idBuffer = value
			}
		case "retry":
			if ms, err := strconv.Atoi(value); err == nil && ms >= 0 {
				state.retry = time.Duration(ms) * time.Millisecond
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return io.ErrUnexpectedEOF
}

// sandboxEventData is a sandbox event's data, the SandboxEvent schema.
type sandboxEventData struct {
	SandboxID    string `json:"sandboxId"`
	Name         string `json:"name"`
	DesiredState string `json:"desiredState"`
	State        string `json:"state"`
	RuntimeState string `json:"runtimeState"`
	Agent        *struct {
		State string `json:"state"`
		Kind  string `json:"kind"`
	} `json:"agent"`
	ErrorReason string    `json:"errorReason"`
	At          time.Time `json:"at"`
}

type eventPrinter struct {
	json    bool
	out     io.Writer
	errOut  io.Writer
	history int
}

func (p *eventPrinter) print(event serverSentEvent) error {
	// Every string here can be a discobox's own: a name is whatever its
	// creator sent, and a box may create one. So nothing reaches the terminal
	// unescaped (cli/REVIEW.md), JSON included.
	if p.json {
		line := map[string]any{"event": event.event, "data": json.RawMessage(event.data)}
		if !json.Valid([]byte(event.data)) {
			line["data"] = event.data
		}
		if event.event != "reset" {
			line["id"] = event.id
		}
		return writeTerminalSafeJSONLine(p.out, line)
	}
	if event.event == "reset" {
		var reset struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal([]byte(event.data), &reset)
		why := "the server does not know the last event read"
		if reset.Reason == "expired" {
			why = "events since the last one read are no longer kept"
		}
		fmt.Fprintf(p.errOut, "event stream started over: %s; showing the last %d\n", why, p.history)
		return nil
	}
	eventType := terminalSafe(event.event)
	var sandbox sandboxEventData
	if !strings.HasPrefix(event.event, "sandbox.") || json.Unmarshal([]byte(event.data), &sandbox) != nil {
		_, err := fmt.Fprintf(p.out, "%s  %s\n", time.Now().Format(time.DateTime), eventType)
		return err
	}
	_, err := fmt.Fprintf(p.out, "%s  %-15s  %s\n", sandbox.At.Local().Format(time.DateTime), eventType, sandboxEventSummary(sandbox))
	return err
}

// sandboxEventSummary is a sandbox event's projection on one line, every
// field escaped for the terminal.
func sandboxEventSummary(sandbox sandboxEventData) string {
	name := sandbox.Name
	if name == "" {
		name = sandbox.SandboxID
	}
	parts := []string{terminalSafe(name), "state=" + terminalSafe(sandbox.State)}
	if sandbox.DesiredState != "" && sandbox.DesiredState != "present" {
		parts = append(parts, "desired="+terminalSafe(sandbox.DesiredState))
	}
	power := sandbox.RuntimeState
	if power == "" {
		power = "-"
	}
	parts = append(parts, "power="+terminalSafe(power))
	agent := "-"
	if sandbox.Agent != nil {
		agent = sandbox.Agent.State
		if sandbox.Agent.Kind != "" {
			agent += "(" + sandbox.Agent.Kind + ")"
		}
	}
	parts = append(parts, "agent="+terminalSafe(agent))
	if sandbox.ErrorReason != "" {
		parts = append(parts, "error="+terminalSafe(sandbox.ErrorReason))
	}
	return strings.Join(parts, "  ")
}
