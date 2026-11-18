package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"path/filepath"
	"time"
)

// startPprof serves Go's pprof endpoints on addr, a loopback host:port that
// config.Load has validated, and returns the address it bound and what stops
// it. An empty addr serves nothing, binds nothing and writes no token.
//
// The endpoints get their own listener and their own mux. They are never
// routed on the API router, so no API transport — local IPC, HTTP, iroh, a
// pool guest's control-plane stream, a proxy route — reaches them. Importing
// net/http/pprof also registers these handlers on http.DefaultServeMux;
// nothing in the server serves that mux.
//
// Loopback alone does not keep a sandbox out: Docker Desktop forwards
// host.docker.internal to the host's 127.0.0.1, and a Docker Desktop pool's
// sandbox may dial that. A profile shows the server's command line and every
// goroutine's stack, and a long profile or trace stalls the server, so every
// request must carry a random token, written fresh on each start to
// tokenFile (0600, removed on stop), as its basic-auth password.
//
// A configured address that cannot be bound fails startup, as any configured
// endpoint does, rather than being logged and skipped.
func startPprof(ctx context.Context, addr, tokenFile string) (bound net.Addr, stop func(), err error) {
	if addr == "" {
		return nil, func() {}, nil
	}
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("pprofListen: %w (set pprofListen, or DISCOBOX_PPROF_LISTEN, to another loopback address, or to off)", err)
	}
	// localhost is accepted by name, so check what it resolved to: the loopback
	// rule is about the socket, not the spelling.
	if tcp, ok := listener.Addr().(*net.TCPAddr); !ok || !tcp.IP.IsLoopback() {
		_ = listener.Close()
		return nil, nil, fmt.Errorf("pprofListen: %s bound %s, which is not a loopback address", addr, listener.Addr())
	}
	token, err := writePprofToken(tokenFile)
	if err != nil {
		_ = listener.Close()
		return nil, nil, fmt.Errorf("pprofListen: write token: %w", err)
	}
	server := &http.Server{
		Handler:           requirePprofToken(token, tokenFile, pprofHandler()),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: a CPU profile or trace answers after the seconds
		// it was asked to sample, and pprof refuses a duration longer than
		// the server's write timeout.
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("pprof listener: %v", err)
		}
	}()
	log.Printf("pprof listening on http://%s/debug/pprof/, with the basic-auth password in %s", listener.Addr(), tokenFile)
	return listener.Addr(), func() {
		// Close, not Shutdown: a profile being taken is cut off rather than
		// holding the server's exit for up to its sampling duration.
		_ = server.Close()
		_ = os.Remove(tokenFile)
	}, nil
}

// writePprofToken writes a new random token to path, readable by this user
// alone, and returns it. Written to a temporary file and renamed, so a reader
// never sees a partial token and the file is never briefly wider than 0600.
func writePprofToken(path string) (string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", err
	}
	token := hex.EncodeToString(secret)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// CreateTemp opens with 0600.
	f, err := os.CreateTemp(dir, ".pprof.token-*")
	if err != nil {
		return "", err
	}
	_, writeErr := f.WriteString(token)
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return token, nil
}

// requirePprofToken refuses a request whose basic-auth password is not token.
// The username is ignored. The refusal names the file, which is no secret: it
// is readable only by the user who may profile this server.
func requirePprofToken(token, tokenFile string, next http.Handler) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, password, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(password), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="discobox pprof"`)
			http.Error(w, "send the token in "+tokenFile+" as the basic-auth password", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// pprofHandler is the standard net/http/pprof route set. The index serves
// every runtime profile by name (heap, goroutine, allocs, block, mutex,
// threadcreate); the CPU profile, trace, cmdline and symbol lookup have
// handlers of their own.
func pprofHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}
