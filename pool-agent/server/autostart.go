package server

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// onUse wraps every sandbox-directed route — the HTTP proxy, the sandbox-agent
// proxy, the git proxies — and does what the request's token says its use is
// (ADR 26-10-12-711 §2):
//
//   - an action or a read starts a stopped sandbox before next runs;
//   - an inspect is served only while the sandbox is running, after waiting
//     out a boot something else began: the lists the control plane answers
//     from the sandbox's last inventory while it is down (ADR 26-10-11-657 §4),
//     which a client that has just created or started a sandbox reads next;
//   - an observe is served only while the sandbox is running, and answers at
//     once: what an orchestrator polls.
//
// Which route is which is the control plane's table, not this one's. It marks
// the token, so a route cannot be classified one way there and another here.
// A token with no mark starts nothing. The one such token a current control
// plane leads to is a sandbox's own, fetching its own origin, and the sandbox
// asking is running. A control plane from before the mark puts none on any
// token, so while one sits over this agent — rolled back under its pools,
// until it replaces their agents with its own version — a stopped sandbox is
// refused until it is started explicitly. Starting on an unmarked token
// instead would have this agent start sandboxes for requests nobody
// classified, an older server's observes among them.
//
// The control plane has already ensured an action's sandbox is running before
// it forwards the request. The start here is the backstop for a stop that
// landed in between, and the whole of the start for a read. It runs in this
// process because this is the only one that knows the container's true state,
// and the only one that can serialize the start against a concurrent explicit
// stop without a distributed lock (ADR 0017 §12).
//
// The control operations are not wrapped: listing sandboxes must not boot the
// pool, and starting a sandbox in order to stop it is absurd.
func (s *sandboxService) onUse(need sandboxNeed, next http.Handler) http.Handler {
	start := s.startOnUse(need, next)
	inspect := s.requireRunning(waitOutBoot, next)
	observe := s.requireRunning(refuseBooting, next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, _ := SignedTokenClaimsFromContext(r.Context())
		switch {
		case claims.Use == UseAction || claims.Use == UseRead:
			start.ServeHTTP(w, r)
		case need == servedByPool:
			// Nothing to start and nothing to require: the pool answers.
			next.ServeHTTP(w, r)
		case claims.Use == UseInspect:
			inspect.ServeHTTP(w, r)
		default:
			observe.ServeHTTP(w, r)
		}
	})
}

// startOnUse starts a stopped sandbox before handing the request to next.
//
// Archived sandboxes are exempt (ADR 0022 §5). Their container is gone by
// intent, so starting one on first use would undo the archive and put the
// sandbox back beyond the reach of its retention policy, in response to nothing
// more than an exec.
//
// A sandbox with no container is refused at once. Whether one is coming is the
// control plane's to know, and it waits where it does (ADR 26-10-12-711 §6);
// waiting here held every route for a sandbox nothing was rebuilding.
//
// need is whether the route reaches into the sandbox at all. Archived and
// containerless sandboxes refuse every route. Otherwise a route that does
// answers a failed start with that failure: proxying anyway would hide it
// behind a missing IP or connection error, losing what the caller can act on,
// such as a missing bind mount, and with what to do about it. The worktree's
// git route is one of them: the repository is the sandbox's own, served by its
// agent (ADR 0126 §4), so a sandbox that cannot start cannot hand over its
// commits until it is repaired. The origin route does not — the pool serves
// it from its own origins.
func (s *sandboxService) startOnUse(need sandboxNeed, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sandboxID := chi.URLParam(r, "sandboxId")
		if sandboxID != "" {
			if err := s.runtime.EnsureSandboxRunning(r.Context(), sandboxID); err != nil {
				// These conflicts tell the caller to unarchive or repair the
				// sandbox before trying again.
				if errors.Is(err, sandboxruntime.ErrArchived) || errors.Is(err, sandboxruntime.ErrNoContainer) {
					http.Error(w, err.Error(), http.StatusConflict)
					return
				}
				if need == needsSandbox {
					if errors.Is(err, sandboxruntime.ErrNotFound) {
						http.Error(w, fmt.Sprintf("start sandbox %q: %v", sandboxID, err), http.StatusNotFound)
						return
					}
					// git prints a refused request's plain-text body to the
					// person fetching, so this is also what a Git client
					// says when the sandbox holding the work will not start.
					http.Error(w, fmt.Sprintf("start sandbox %q: %v; nothing in it can be reached until it starts, so if this persists run `discobox admin box repair %s`", sandboxID, err, sandboxID), http.StatusInternalServerError)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// requireRunning serves a read of what is inside a sandbox only while the
// sandbox is running, and never starts it.
//
// It is startOnUse's opposite, for every read the control plane marks as not
// itself a use of the sandbox: its audit data (ADR 0130) — harness hooks and
// exec events — its terminals' screens and waits (ADR 0137 §2), and the lists
// the control plane answers from the sandbox's last inventory while it is
// down — execs, services, tools (ADR 26-10-11-657 §4). Starting is right
// where a request is a use of the sandbox. Such a read is not one, and
// starting a stopped sandbox to read it would undo the stop it may be reading
// about — the idle stop of ADR 0108 above all — or, for an orchestrator
// polling its workers, keep that stop from ever holding. A sandbox that is
// not running answers 409 saying so, marked with
// model.SandboxNotRunningHeader. One this runtime does not know passes
// through, so the proxy fails on its own terms.
//
// boot is what a sandbox still booting gets — Docker calls its container
// running, but the agent this would read from is not answering yet. A read
// that refuseBooting answers 409 like a stopped one. A read that
// waitOutBoot waits for the boot something else began, which starts nothing,
// and then looks again: a boot can end without the sandbox up.
func (s *sandboxService) requireRunning(boot bootWait, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sandboxID := chi.URLParam(r, "sandboxId")
		sb, err := s.runtime.GetSandbox(r.Context(), sandboxID)
		// Whatever the container's status reads: a boot is marked before its
		// container is started, so one caught in between still reads stopped.
		if err == nil && boot == waitOutBoot && s.runtime.SandboxBooting(sandboxID) {
			if waitErr := s.runtime.AwaitSandboxBoot(r.Context(), sandboxID); waitErr == nil {
				sb, err = s.runtime.GetSandbox(r.Context(), sandboxID)
			}
		}
		if err == nil {
			status := string(sb.Status)
			if sb.Status == sandboxruntime.StatusRunning && s.runtime.SandboxBooting(sandboxID) {
				status = "starting"
			}
			if status != string(sandboxruntime.StatusRunning) {
				w.Header().Set(workerapimodel.SandboxNotRunningHeader, status)
				http.Error(w, fmt.Sprintf("discobox %s is %s: what is read here is inside it, and reading it does not start it", sandboxID, status), http.StatusConflict)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// bootWait is whether a read waits out a boot under way. See requireRunning.
type bootWait bool

const (
	// waitOutBoot is for the reads a client makes of a sandbox it is
	// starting: the lists, which answered once the sandbox was up when they
	// were what started it, and still do.
	waitOutBoot bootWait = true
	// refuseBooting is for the reads an orchestrator polls, which want an
	// answer now.
	refuseBooting bootWait = false
)

// sandboxNeed is whether a route reaches into the sandbox, so a failed start
// refuses it. See startOnUse.
type sandboxNeed bool

const (
	needsSandbox sandboxNeed = true
	servedByPool sandboxNeed = false
)
