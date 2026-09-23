package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/discobox-ai/discobox/health"
	"github.com/discobox-ai/discobox/version"
)

// processStart is when this server process began. Uptime is reported against
// it rather than against the moment the router was built, because the question
// being asked is how long the thing has been running.
var processStart = time.Now()

// startupHandler serves the endpoints from the moment they bind, reporting what
// startup is doing, and hands over to the real router once there is one.
//
// Binding first is the point. Everything expensive — opening the database,
// migrating it, building the services, reaching a registry to seed the built-in
// harnesses — happens after the endpoints are bound. Done before, it would leave
// a client nothing to look at but a refused connection and no way to tell a
// server that is still coming up from one that has died on startup: very
// different problems that would look identical.
type startupHandler struct {
	mu    sync.Mutex
	phase string
	// choice is the question startup is held on, and answer where its answer
	// goes; both nil unless it is (see awaitChoice).
	choice *health.Choice
	answer chan string
	ready  atomic.Pointer[http.Handler]
}

func newStartupHandler(phase string) *startupHandler {
	handler := &startupHandler{}
	// Through setPhase, so the first step is logged like every other one. It
	// was the only step that never was, which left a foreground run's log
	// starting at the second one and saying nothing about the step it was on
	// if it failed there.
	handler.setPhase(phase)
	return handler
}

// setPhase records what startup is doing now, and logs it so a server run in
// the foreground says the same thing its /healthz does.
func (h *startupHandler) setPhase(phase string) {
	h.mu.Lock()
	if h.phase == phase {
		h.mu.Unlock()
		return
	}
	h.phase = phase
	h.mu.Unlock()
	log.Printf("starting: %s", phase)
}

// ready hands over to the real router. Requests arriving after this see the
// server exactly as if it had bound its listeners only now.
func (h *startupHandler) setReady(handler http.Handler) {
	h.ready.Store(&handler)
}

func (h *startupHandler) status() health.Status {
	h.mu.Lock()
	phase, choice := h.phase, h.choice
	h.mu.Unlock()
	status := health.Status{
		Status:        health.StatusStarting,
		Phase:         phase,
		Version:       version.String(),
		UptimeSeconds: time.Since(processStart).Seconds(),
	}
	if choice != nil {
		status.Status = health.StatusNeedsChoice
		status.Choice = choice
	}
	return status
}

// awaitChoice holds startup on choice until it is answered through
// health.SetupDefaultProviderPath, and returns the answer (ADR 0148 §2).
//
// The server stays bound and says why, rather than exiting: an exited server
// leaves its reason only in a log, and a CLI that started it as a systemd user
// service cannot even see its exit code. The log says it too, with the answers
// a server started by hand or by a service manager can be given.
func (h *startupHandler) awaitChoice(ctx context.Context, choice health.Choice) (string, error) {
	answer := make(chan string, 1)
	h.mu.Lock()
	h.choice, h.answer = &choice, answer
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.choice, h.answer = nil, nil
		h.mu.Unlock()
	}()
	log.Printf("starting: waiting for a default provider to be chosen: the %s provider cannot run on this host (%s: %s). "+
		"Answer with `discobox admin server choose-provider %s`, or set defaultProvider in the server configuration and restart",
		choice.Provider, choice.Reason, choice.Detail, strings.Join(choice.Alternatives, "|"))
	select {
	case chosen := <-answer:
		log.Printf("starting: %s chosen as the default provider", chosen)
		return chosen, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// serveChoice takes the answer to the question startup is held on. It answers
// only over the local IPC endpoint; on TCP, iroh, or a pool guest's carrier it
// is one more path a starting server has no handler for. The endpoint trusts
// whatever can connect to the socket, which is what every request the CLI
// makes to its own server relies on. Pool agents reach that socket too — a
// Docker pool's is bind-mounted, and a libkrun guest's control plane is
// forwarded to it — but none can exist while a first start is held: no
// provider is installed yet, so no pool has been started.
func (h *startupHandler) serveChoice(w http.ResponseWriter, r *http.Request) {
	if !localIPCRequest(r) {
		writeHealthStatus(w, http.StatusServiceUnavailable, h.status())
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body health.DefaultProviderChoice
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, fmt.Sprintf("decode the choice: %v", err), http.StatusBadRequest)
		return
	}
	if err := h.answerChoice(body.Provider); err != nil {
		code := http.StatusConflict
		if errors.Is(err, errNotAnAlternative) {
			code = http.StatusBadRequest
		}
		http.Error(w, err.Error(), code)
		return
	}
	writeHealthStatus(w, http.StatusAccepted, h.status())
}

var errNotAnAlternative = errors.New("not one of the providers this server will install instead")

func (h *startupHandler) answerChoice(provider string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.choice == nil {
		return errors.New("this server is not waiting for a default provider to be chosen")
	}
	if !slices.Contains(h.choice.Alternatives, provider) {
		return fmt.Errorf("%q is %w: %s", provider, errNotAnAlternative, strings.Join(h.choice.Alternatives, ", "))
	}
	// The question is taken down in the same critical section that answers
	// it, not when awaitChoice next runs: the 202 and every probe after it
	// must say the server is starting, or a CLI that has just answered sees
	// the question again and reports it unanswered.
	h.answer <- provider
	h.choice, h.answer = nil, nil
	return nil
}

func (h *startupHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if handler := h.ready.Load(); handler != nil {
		(*handler).ServeHTTP(w, r)
		return
	}
	if r.URL.Path == health.SetupDefaultProviderPath {
		h.serveChoice(w, r)
		return
	}
	// Every path answers the same way, not just the probe: a request that
	// arrives mid-startup has no handler to reach, and 503 with the reason is a
	// better answer than a 404 that suggests the route does not exist.
	writeHealthStatus(w, http.StatusServiceUnavailable, h.status())
}

type localIPCKey struct{}

// markLocalIPC is the server's ConnContext. It records whether a connection
// arrived on a Unix socket or a Windows named pipe — the local IPC endpoint —
// as opposed to TCP, iroh, or a pool guest's carrier. It says nothing about
// which process connected.
func markLocalIPC(ctx context.Context, conn net.Conn) context.Context {
	switch conn.LocalAddr().Network() {
	case "unix", "pipe":
		return context.WithValue(ctx, localIPCKey{}, true)
	}
	return ctx
}

func localIPCRequest(r *http.Request) bool {
	local, _ := r.Context().Value(localIPCKey{}).(bool)
	return local
}

// readyStatus is what /healthz reports once the real router is serving. It is
// unconditionally ready: the router this is registered on does not serve until
// startup has finished.
func readyStatus() health.Status {
	return health.Status{
		Status:        health.StatusReady,
		Version:       version.String(),
		UptimeSeconds: time.Since(processStart).Seconds(),
	}
}

func writeHealthStatus(w http.ResponseWriter, code int, status health.Status) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Startup status is a snapshot of a moving thing; a cached one is worse
	// than none.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(status); err != nil {
		// The header is already written, so this can only be reported here.
		log.Printf("write health status: %v", err)
	}
}
