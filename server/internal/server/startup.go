package server

import (
	"encoding/json"
	"log"
	"net/http"
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
// harnesses — used to happen before anything was listening, so a client had
// nothing to look at but a refused connection and no way to tell a server that
// was still coming up from one that had died on startup. They are very
// different problems and they looked identical.
type startupHandler struct {
	mu    sync.Mutex
	phase string
	ready atomic.Pointer[http.Handler]
}

func newStartupHandler(phase string) *startupHandler {
	return &startupHandler{phase: phase}
}

// setPhase records what startup is doing now, and logs it so a server run in
// the foreground says the same thing its /healthz does.
func (h *startupHandler) setPhase(phase string) {
	h.mu.Lock()
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
	phase := h.phase
	h.mu.Unlock()
	return health.Status{
		Status:        health.StatusStarting,
		Phase:         phase,
		Version:       version.String(),
		UptimeSeconds: time.Since(processStart).Seconds(),
	}
}

func (h *startupHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if handler := h.ready.Load(); handler != nil {
		(*handler).ServeHTTP(w, r)
		return
	}
	// Every path answers the same way, not just the probe: a request that
	// arrives mid-startup has no handler to reach, and 503 with the reason is a
	// better answer than a 404 that suggests the route does not exist.
	writeHealthStatus(w, http.StatusServiceUnavailable, h.status())
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
