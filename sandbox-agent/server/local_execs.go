package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/agentstatus"
	"github.com/discobox-ai/discobox/sandbox-agent/config"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
	"github.com/discobox-ai/discobox/sandbox-agent/localexec"
	"github.com/discobox-ai/discobox/sandbox-agent/services"
	"github.com/discobox-ai/discobox/sandbox-agent/shimruntime"
	"github.com/discobox-ai/discobox/sandbox-agent/terminal"
)

// localExecs is the exec API the sandbox's own user reaches on a local socket
// (localexec, ADR 26-10-22-853 §2), over the same exec runtime and terminal
// layer as the agent's routes.
//
// Every process in the sandbox can connect, so a connection is served only
// when the process at its other end runs as the sandbox's default user or as
// root, by the uid the kernel records for it (peerUID): another account — a
// service's own, nobody — gets nothing, since a shell as the default user is
// that user's sudo. What it serves is bounded by that user too: it serves an
// exec only when that exec runs as that user, and creates execs only as that
// user, never as one a caller names.
type localExecs struct {
	h       *handler
	harness config.Harness
}

func (l *localExecs) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /box", l.box)
	mux.HandleFunc("GET /execs", l.list)
	mux.HandleFunc("POST /execs", l.create)
	mux.HandleFunc("POST /execs/{id}/start", l.start)
	mux.HandleFunc("GET /execs/{id}/attach", l.attach)
	mux.HandleFunc("POST /execs/{id}/input", l.input)
	mux.HandleFunc("DELETE /execs/{id}", l.delete)
	return mux
}

// serveLocalExecs serves the socket until ctx ends: a directory every user may
// enter but not list (0711) holding a socket every user may connect to (0666),
// as the hook collector's.
func serveLocalExecs(ctx context.Context, logger *slog.Logger, l *localExecs, path string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o711); err != nil {
		logger.Warn("local exec socket disabled", "error", err)
		return
	}
	if err := os.Chmod(filepath.Dir(path), 0o711); err != nil {
		logger.Warn("local exec socket disabled", "error", err)
		return
	}
	_ = os.Remove(path)
	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", path)
	if err != nil {
		logger.Warn("local exec socket disabled", "error", err)
		return
	}
	if err := os.Chmod(path, 0o666); err != nil {
		_ = ln.Close()
		logger.Warn("local exec socket disabled", "error", err)
		return
	}
	server := &http.Server{
		Handler:           l.admit(l.routes()),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			uid, err := peerUID(conn)
			if err != nil {
				return ctx
			}
			return context.WithValue(ctx, peerUIDKey{}, uid)
		},
	}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Warn("local exec socket stopped", "error", err)
	}
}

type peerUIDKey struct{}

// admit serves a request only from root or the sandbox's default user. A
// connection whose peer could not be read is no one's.
func (l *localExecs) admit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid, ok := r.Context().Value(peerUIDKey{}).(uint32)
		if !ok || !l.admitted(uid) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "only the sandbox's user may use this socket"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (l *localExecs) admitted(uid uint32) bool {
	if uid == 0 {
		return true
	}
	def, ok := l.defaultUser()
	return ok && def.UID != nil && *def.UID >= 0 && uint32(*def.UID) == uid
}

// defaultUser is the sandbox's default user, resolved as an exec that names
// nobody resolves it (execs.Manager.ResolveUser): the manifest's user with
// its ids and home filled in from the passwd entry, or — when no user is
// named — this process's own identity, which such an exec inherits. ok is
// false when it cannot be resolved, and then nothing is served.
func (l *localExecs) defaultUser() (execs.User, bool) {
	def, err := l.h.execs.ResolveUser(execs.CreateRequest{})
	if err != nil {
		return execs.User{}, false
	}
	if def == nil {
		uid := int64(os.Getuid())
		return execs.User{UID: &uid}, true
	}
	return *def, true
}

func (l *localExecs) box(w http.ResponseWriter, r *http.Request) {
	out := localexec.Box{SandboxID: l.h.identity.SandboxID, Harness: l.harness.ID, HarnessName: l.harness.Name}
	if workdir, err := l.h.execs.DefaultWorkdir(); err == nil {
		out.Workdir = workdir
	}
	// The base is the primary source's, matched by the source's own
	// directory: the working directory may be one inside it.
	root, err := l.h.execs.SourceRoot()
	if err != nil {
		root = out.Workdir
	}
	for _, source := range l.h.sources {
		if source.Target == root {
			out.DiffBase, _ = agentstatus.ResolveDiffBase(r.Context(), source, l.h.execUser)
		}
	}
	if primary, ok := l.h.terminals.CurrentPrimary(); ok {
		out.Agent = primary.ProgramStatus
	}
	writeJSON(w, http.StatusOK, out)
}

func (l *localExecs) list(w http.ResponseWriter, _ *http.Request) {
	out := []localexec.Exec{}
	for _, exec := range l.h.execs.List() {
		if l.ours(exec) {
			out = append(out, localExec(exec))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func localExec(exec execs.Exec) localexec.Exec {
	return localexec.Exec{
		ID:      exec.ID,
		Harness: terminal.HarnessID(exec),
		Primary: terminal.IsPrimary(exec),
		Live:    exec.Status == execs.StatusStarting || exec.Status == execs.StatusRunning,
		Service: exec.Metadata[services.MetadataServiceID],
		TTY:     exec.TTY,
	}
}

// create makes a shell, or another session of the sandbox's harness, as the
// sandbox's default user — the user is never the caller's to choose — and
// does not start it: the caller attaches first, as every client does, so the
// exec's first output has somewhere to go.
func (l *localExecs) create(w http.ResponseWriter, r *http.Request) {
	var req localexec.CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var (
		created execs.Exec
		err     error
	)
	if req.Shell {
		created, err = l.h.execs.Create(r.Context(), execs.CreateRequest{
			Shell: true, TTY: true, Rows: uint16(req.Rows), Cols: uint16(req.Cols),
		})
	} else {
		created, err = l.h.terminals.Create(r.Context(), terminal.CreateRequest{Rows: uint16(req.Rows), Cols: uint16(req.Cols)})
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, localExec(created))
}

// served resolves the exec a request names — the primary by its virtual id
// too, without relaunching it — and answers for it only when it is the
// default user's.
func (l *localExecs) served(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, err := l.h.resolveExecIDReadOnly(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "exec not found"})
		return "", false
	}
	exec, ok := l.h.execs.Get(id)
	if !ok || !l.ours(exec) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "exec not found"})
		return "", false
	}
	return id, true
}

func (l *localExecs) start(w http.ResponseWriter, r *http.Request) {
	id, ok := l.served(w, r)
	if !ok {
		return
	}
	exec, err := l.h.execs.Start(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, localExec(exec))
}

// attach is the agent's own attach to the exec the request names, once it is
// the default user's: the same replay, the same autostop hold for as long as
// it lasts, and the same answers when it cannot.
func (l *localExecs) attach(w http.ResponseWriter, r *http.Request) {
	id, ok := l.served(w, r)
	if !ok {
		return
	}
	l.h.attachExecHTTP(w, r, id)
}

// input types text and presses Enter, a paste when the program asked for one.
func (l *localExecs) input(w http.ResponseWriter, r *http.Request) {
	id, ok := l.served(w, r)
	if !ok {
		return
	}
	var req localexec.InputRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := l.h.execs.Input(r.Context(), id, []shimruntime.InputPart{{Text: req.Text}, {Key: "Enter"}}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (l *localExecs) delete(w http.ResponseWriter, r *http.Request) {
	id, ok := l.served(w, r)
	if !ok {
		return
	}
	if err := l.h.execs.Delete(r.Context(), id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ours is whether an exec runs as the sandbox's default user. An exec with no
// user of its own runs as that user. One that names a user is ours only when
// it is provably the same one — by uid, or by name when the exec was recorded
// without a uid — so root, or a service's own account, named any way, is not
// this socket's.
func (l *localExecs) ours(exec execs.Exec) bool {
	def, ok := l.defaultUser()
	if !ok {
		return false
	}
	if exec.User == nil {
		return true
	}
	if exec.User.UID != nil {
		return def.UID != nil && *exec.User.UID == *def.UID
	}
	return exec.User.Name != "" && exec.User.Name == def.Name
}
