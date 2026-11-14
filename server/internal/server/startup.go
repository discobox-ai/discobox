package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/api/openapi"
	"github.com/discobox-ai/discobox/pkg/health"
	"github.com/discobox-ai/discobox/pkg/version"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/go-faster/jx"
	"gopkg.in/yaml.v3"
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
	mu       sync.Mutex
	phase    string
	warnings []string
	ready    atomic.Pointer[http.Handler]
	// readyAt is when the router took over, which the wait for pools to
	// upgrade counts from; pools counts them. Both are set by setReady.
	readyAt time.Time
	pools   services.PoolUpgradeService
	// upgradeWait is how long health reports upgrading pools before it
	// reports ready anyway (health.PoolUpgradeWait).
	upgradeWait time.Duration
}

func newStartupHandler(phase string) *startupHandler {
	handler := &startupHandler{upgradeWait: health.PoolUpgradeWait}
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
// server exactly as if it had bound its listeners only now. pools answers the
// pools its health still waits on (ADR 26-11-14-624); nil waits on none.
func (h *startupHandler) setReady(handler http.Handler, pools services.PoolUpgradeService) {
	h.mu.Lock()
	h.readyAt, h.pools = time.Now(), pools
	h.mu.Unlock()
	h.ready.Store(&handler)
}

func (h *startupHandler) addWarning(warning string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.warnings = append(h.warnings, warning)
}

func (h *startupHandler) status() health.Status {
	h.mu.Lock()
	phase := h.phase
	warnings := append([]string(nil), h.warnings...)
	h.mu.Unlock()
	return health.Status{
		Status:        health.StatusStarting,
		Phase:         phase,
		Warnings:      warnings,
		Version:       version.String(),
		UptimeSeconds: time.Since(processStart).Seconds(),
	}
}

func (h *startupHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if handler := h.ready.Load(); handler != nil {
		if r.URL.Path == health.Path {
			code, status := h.servingStatus(r.Context())
			writeHealthStatus(w, code, status)
			return
		}
		(*handler).ServeHTTP(w, r)
		return
	}
	status := h.status()
	if r.URL.Path == health.Path {
		writeHealthStatus(w, http.StatusServiceUnavailable, status)
		return
	}
	message := "server is starting: " + status.Phase
	w.Header().Set("Cache-Control", "no-store")
	var body interface{ Encode(*jx.Encoder) }
	_, pattern := startupLegacyErrors().Handler(r)
	if pattern != "" {
		w.Header().Set("Content-Type", "application/json")
		body = &serverapi.ErrorResponse{Error: message}
	} else {
		w.Header().Set("Content-Type", "application/problem+json")
		body = &serverapi.ErrorModel{
			Status: serverapi.NewOptInt64(http.StatusServiceUnavailable),
			Title:  serverapi.NewOptString(http.StatusText(http.StatusServiceUnavailable)),
			Detail: serverapi.NewOptString(message),
		}
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	encoder := new(jx.Encoder)
	body.Encode(encoder)
	if _, err := encoder.WriteTo(w); err != nil {
		log.Printf("write startup error: %v", err)
	}
}

// servingStatus is what /healthz reports once the router serves: upgrading
// pools, with 503, while a pool registered before this start has not been
// brought onto this server's agent and the wait for it has not run out; then
// ready, with a warning saying how many pools are still upgrading, until none
// is. It counts them and never names them: this answers anyone, and the log
// names them (ADR 26-11-14-624 §2).
func (h *startupHandler) servingStatus(ctx context.Context) (int, health.Status) {
	status := readyStatus()
	h.mu.Lock()
	status.Warnings = append([]string(nil), h.warnings...)
	pools, readyAt, wait := h.pools, h.readyAt, h.upgradeWait
	h.mu.Unlock()
	if pools == nil {
		return http.StatusOK, status
	}
	status.PoolsUpgrading = pools.PoolsUpgrading(ctx)
	if status.PoolsUpgrading == 0 {
		return http.StatusOK, status
	}
	if time.Since(readyAt) < wait {
		status.Status = health.StatusUpgradingPools
		return http.StatusServiceUnavailable, status
	}
	status.Warnings = append(status.Warnings, fmt.Sprintf(
		"%s not upgraded after %s; the server's log says which and why",
		countPools(status.PoolsUpgrading), wait))
	return http.StatusOK, status
}

func countPools(n int) string {
	if n == 1 {
		return "1 pool"
	}
	return fmt.Sprintf("%d pools", n)
}

// readyStatus is what /healthz reports once the real router is serving, before
// servingStatus adds the pools still upgrading. The router this is registered
// on does not serve until startup has finished.
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

// The contract mixes legacy JSON errors and problem JSON, even within /api.
// Derive the legacy route set from the embedded contract so new operations and
// changes to their error format cannot silently drift from the startup responder.
// Defer parsing until an API request arrives; listener binding and health probes
// do not wait for it, and the ready router never uses it.
var startupLegacyErrors = sync.OnceValue(func() *http.ServeMux {
	var doc struct {
		Paths map[string]map[string]struct {
			Responses map[string]struct {
				Content map[string]any `yaml:"content"`
			} `yaml:"responses"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(openapi.ServerYAML, &doc); err != nil {
		panic(fmt.Sprintf("parse embedded server contract: %v", err))
	}
	mux := http.NewServeMux()
	for path, methods := range doc.Paths {
		for method, operation := range methods {
			if _, legacy := operation.Responses["default"].Content["application/json"]; legacy {
				mux.HandleFunc(strings.ToUpper(method)+" "+path, func(http.ResponseWriter, *http.Request) {})
			}
		}
	}
	return mux
})
