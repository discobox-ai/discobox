package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// A dropped stream is resumed with the last event's ID, so nothing is printed
// twice and nothing is skipped; a discobox named filters what is printed, a
// reset included.
func TestFollowSandboxEventsResumesFromTheLastEvent(t *testing.T) {
	var (
		mu       sync.Mutex
		cursors  []string
		requests int
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		n := requests
		cursors = append(cursors, r.Header.Get("Last-Event-ID"))
		mu.Unlock()
		if r.URL.Path != "/api/projects/proj_1/events" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		event := func(id int, typ, sandbox, state string) {
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: {\"sandboxId\":%q,\"name\":%q,\"state\":%q,\"desiredState\":\"present\",\"runtimeState\":\"running\",\"program\":{\"state\":\"blocked\",\"kind\":\"permission\",\"app\":\"claude-code\"}}\n\n", id, typ, sandbox, sandbox+"-name", state)
		}
		switch n {
		case 1:
			fmt.Fprint(w, "retry: 10\n\n")
			event(1, "sandbox.created", "sbx_a", "pending")
			event(2, "sandbox.created", "sbx_b", "pending")
			// The stream stands past its last event, and says so.
			fmt.Fprint(w, "id: 5\n\n")
			// The connection drops here.
		case 2:
			fmt.Fprint(w, ": ping\n\nevent: reset\ndata: {}\n\n")
			event(3, "sandbox.updated", "sbx_a", "ready")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}
	}))
	defer server.Close()

	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	// Interrupted once the last event is printed.
	cmd.SetOut(writerFunc(func(p []byte) (int, error) {
		n, err := out.Write(p)
		if strings.Contains(string(p), "state=ready") {
			cancel()
		}
		return n, err
	}))
	cmd.SetContext(ctx)
	done := make(chan error, 1)
	go func() { done <- app.followSandboxEvents(cmd, "proj_1", "sbx_a") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("follow: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("follow did not end when its context did")
	}

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(cursors, ",") != ",5" {
		t.Fatalf("cursors sent = %q, want none and then the last ID the stream gave", cursors)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("printed:\n%s\nwant a's two events and the reset", out.String())
	}
	if !strings.Contains(lines[0], "created") || !strings.Contains(lines[0], "sbx_a") || strings.Contains(out.String(), "sbx_b") {
		t.Fatalf("printed:\n%s\nwant sbx_a's events only", out.String())
	}
	if !strings.HasPrefix(lines[1], "reset:") {
		t.Fatalf("second line %q, want the reset", lines[1])
	}
	for _, want := range []string{"updated", "state=ready", "power=running", "agent=blocked(permission) claude-code"} {
		if !strings.Contains(lines[2], want) {
			t.Fatalf("line %q lacks %q", lines[2], want)
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// With -o json each event is a line of JSON holding its id, type and data.
func TestWriteSandboxEventAsJSON(t *testing.T) {
	app := &App{output: "json"}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	event := sandboxEvent{ID: "7", Event: "sandbox.deleted", Data: sandboxEventData{SandboxID: "sbx_a", State: "archived", DesiredState: "deleted"}}
	if err := app.writeSandboxEvent(cmd, event); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("not one JSON value: %q: %v", out.String(), err)
	}
	if got["id"] != "7" || got["event"] != "sandbox.deleted" || got["data"].(map[string]any)["sandboxId"] != "sbx_a" {
		t.Fatalf("json = %v", got)
	}
	if strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("json spans lines: %q", out.String())
	}
}

// A stream the server refuses is the command's error, with the server's
// reason, and is not retried.
func TestFollowSandboxEventsReportsARefusal(t *testing.T) {
	var requests int
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"project not found"}`))
	}))
	defer server.Close()

	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetContext(context.Background())
	err := app.followSandboxEvents(cmd, "proj_1", "")
	if err == nil || !strings.Contains(err.Error(), "project not found") {
		t.Fatalf("error = %v, want the server's reason", err)
	}
	if requests != 1 {
		t.Fatalf("asked %d times, want once", requests)
	}
}

func TestParseServerSentEvents(t *testing.T) {
	stream := "retry: 500\n\n: comment\nid: 4\nevent: sandbox.updated\ndata: {\"a\":\ndata: 1}\n\nevent: reset\ndata: {}\n\nid: 9\n\n"
	var got []string
	var fields []string
	err := parseServerSentEvents(strings.NewReader(stream), func(field, value string) {
		fields = append(fields, field+"="+value)
	}, func(id, event, data string) error {
		got = append(got, id+"|"+event+"|"+data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"4|sandbox.updated|{\"a\":\n1}", "|reset|{}", "9||"}
	if strings.Join(got, ";") != strings.Join(want, ";") {
		t.Fatalf("events = %q, want %q", got, want)
	}
	if strings.Join(fields, ",") != "retry=500" {
		t.Fatalf("fields = %q", fields)
	}
}
