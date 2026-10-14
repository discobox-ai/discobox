package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The reconnect decision turns on whether the command itself ended or the
// runtime disappeared underneath it. A recorded exit is the command's own
// result; a lost unit is an ungraceful disappearance the attach can recover
// from, but only through the virtual primary id.
func TestSandboxExecAttachDoneDistinguishesGracefulExitFromLostRuntime(t *testing.T) {
	const sandboxID = "sandbox-1"
	tests := []struct {
		name     string
		execID   string
		exec     string
		wantDone bool
		wantErr  string
	}{
		{
			name:     "graceful exit ends the session",
			execID:   primaryExecID,
			exec:     `{"id":"ex_1","status":"exited","exitCode":0,"command":["bash"],"workdir":"/workspace","tty":true,"createdAt":"2026-01-01T00:00:00Z"}`,
			wantDone: true,
		},
		{
			name:     "nonzero exit ends the session with its code",
			execID:   primaryExecID,
			exec:     `{"id":"ex_1","status":"exited","exitCode":3,"command":["bash"],"workdir":"/workspace","tty":true,"createdAt":"2026-01-01T00:00:00Z"}`,
			wantDone: true,
			wantErr:  "process exited with code 3",
		},
		{
			name:     "lost primary reconnects so the attach relaunches it",
			execID:   primaryExecID,
			exec:     `{"id":"ex_1","status":"lost","error":"exec unit status is unavailable","command":["bash"],"workdir":"/workspace","tty":true,"createdAt":"2026-01-01T00:00:00Z"}`,
			wantDone: false,
		},
		{
			name:     "lost concrete exec ends the session, nothing can revive it",
			execID:   "ex_1",
			exec:     `{"id":"ex_1","status":"lost","error":"exec unit status is unavailable","command":["bash"],"workdir":"/workspace","tty":true,"createdAt":"2026-01-01T00:00:00Z"}`,
			wantDone: true,
			wantErr:  "exec unit status is unavailable",
		},
		{
			name:     "failed launch ends the session",
			execID:   primaryExecID,
			exec:     `{"id":"ex_1","status":"failed","error":"start shim: no such file","command":["bash"],"workdir":"/workspace","tty":true,"createdAt":"2026-01-01T00:00:00Z"}`,
			wantDone: true,
			wantErr:  "start shim: no such file",
		},
		{
			name:     "running terminal keeps reconnecting",
			execID:   primaryExecID,
			exec:     `{"id":"ex_1","status":"running","command":["bash"],"workdir":"/workspace","tty":true,"createdAt":"2026-01-01T00:00:00Z"}`,
			wantDone: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				_, _ = w.Write([]byte(tc.exec))
			}))
			t.Cleanup(server.Close)

			app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
			done, err := app.sandboxExecAttachDone(t.Context(), "project-1", sandboxID, tc.execID)
			if done != tc.wantDone {
				t.Fatalf("done = %v, want %v (err = %v)", done, tc.wantDone, err)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// A terminal cannot outlive its sandbox: when the exec read fails and the
// sandbox is stopping or stopped, the attach ends instead of reconnecting
// forever. The client never restarts the sandbox — autostart is a future
// `discobox attach` concern.
func TestSandboxExecAttachDoneEndsWhenSandboxStops(t *testing.T) {
	const sandboxID = "sbx_9qk5n25t2hh2rv00"
	for _, phase := range []string{"stopping", "stopped"} {
		t.Run(phase, func(t *testing.T) {
			var started bool
			server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				switch {
				case strings.HasSuffix(r.URL.Path, "/execs/primary"):
					// The proxy refuses exec reads unless the sandbox is running.
					w.WriteHeader(http.StatusConflict)
					_, _ = w.Write([]byte(`{"error":"sandbox not found"}`))
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
					started = true
					w.WriteHeader(http.StatusAccepted)
					_, _ = w.Write([]byte(runTestSandboxJSON(sandboxID, "running")))
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, sandboxID):
					_, _ = w.Write([]byte(runTestSandboxJSON(sandboxID, phase)))
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(server.Close)

			app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
			done, err := app.sandboxExecAttachDone(t.Context(), "project-1", sandboxID, primaryExecID)
			if !done {
				t.Fatalf("done = false, want the attach to end; err = %v", err)
			}
			if err == nil || !strings.Contains(err.Error(), phase) {
				t.Fatalf("err = %v, want it to name the %q phase", err, phase)
			}
			if started {
				t.Fatal("attach started the sandbox; autostart must not happen")
			}
		})
	}
}

// A discobox being repaired reads failed, with its container gone, until the
// repair lands. That is a rebuild the attach reconnects across, not a sandbox
// that took the terminal with it (ADR 26-10-12-711 §8).
func TestSandboxExecAttachDoneKeepsReconnectingThroughARepair(t *testing.T) {
	const sandboxID = "sbx_9qk5n25t2hh2rv00"
	repairing := strings.Replace(runTestSandboxJSON(sandboxID, "failed"),
		`"displayState":"error"`, `"displayState":"repairing","runtimeState":"stopped"`, 1)
	if !strings.Contains(repairing, `"displayState":"repairing"`) {
		t.Fatalf("fixture = %s, want a sandbox being repaired", repairing)
	}
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch {
		case strings.HasSuffix(r.URL.Path, "/execs/primary"):
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"it has no container on its pool"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, sandboxID):
			_, _ = w.Write([]byte(repairing))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
	done, err := app.sandboxExecAttachDone(t.Context(), "project-1", sandboxID, primaryExecID)
	if done {
		t.Fatalf("done = true, want the attach to keep reconnecting; err = %v", err)
	}
}

// A read failure that does not resolve to a stopped sandbox is transient: keep
// reconnecting rather than ending the session on a control-plane blip.
func TestSandboxExecAttachDoneRetriesTransientReadFailure(t *testing.T) {
	const sandboxID = "sbx_9qk5n25t2hh2rv00"
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch {
		case strings.HasSuffix(r.URL.Path, "/execs/primary"):
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"upstream unavailable"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, sandboxID):
			// The sandbox is still running; the exec read just blipped.
			_, _ = w.Write([]byte(runTestSandboxJSON(sandboxID, "running")))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
	done, err := app.sandboxExecAttachDone(t.Context(), "project-1", sandboxID, primaryExecID)
	if done {
		t.Fatalf("done = true, want a retriable failure; err = %v", err)
	}
	if err == nil {
		t.Fatal("err = nil, want the read failure surfaced for retry")
	}
}

// A pool agent refuses an attach in plain text, which the transport turns into
// an error before the websocket dialer sees a response. It is still the
// refusal: the attach reports the agent's sentence rather than the dialer's
// account of a failed handshake.
func TestAttachReportsAPlainTextRefusal(t *testing.T) {
	const sandboxID = "sbx_9qk5n25t2hh2rv00"
	const refusal = "discobox sbx_9qk5n25t2hh2rv00 is stopped: what is read here is inside it, and reading it does not start it"
	app := attachRefusedApp(t, func(w http.ResponseWriter) {
		http.Error(w, refusal, http.StatusConflict)
	})
	_, err := app.openSandboxExecAttach(t.Context(), "project-1", sandboxID, primaryExecID, true)
	if err == nil {
		t.Fatal("attach to a refused discobox succeeded")
	}
	err = app.execAttachError(t.Context(), "project-1", sandboxID, primaryExecID, err)
	if want := "attach exec: " + refusal; err.Error() != want {
		t.Fatalf("attach error =\n%s\nwant\n%s", err, want)
	}
}

// attachRefusedApp is an App whose server refuses every attach with refuse,
// and fails the test on any other request: a refusal that explains itself is
// reported without asking the server anything more.
func attachRefusedApp(t *testing.T, refuse func(http.ResponseWriter)) *App {
	t.Helper()
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/execs/primary/attach") {
			refuse(w)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	return &App{serverURL: server.URL, autoStart: autoStartServerFalse}
}
