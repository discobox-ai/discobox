// Package pprofserve serves Go's pprof endpoints for one process on a loopback
// listener of their own, behind a token written fresh on every start (ADR
// 26-11-24-418 §1). The server, the pool agent, the pool proxy and the
// BuildKit mediator each start one; the rules are the same for all of them.
//
//   - Opt-in: an empty address serves nothing, binds nothing and writes no
//     token. Off is the word for "none" where a setting has a default.
//   - Loopback, checked twice: the configured host must be a loopback IP or
//     localhost (Validate), and the socket must have bound a loopback address
//     (Start), since the rule is about the socket, not the spelling.
//   - Its own http.Server and mux. Nothing routes them on an API router, a
//     control-plane channel, a tunnel or a proxy listener, and nothing serves
//     http.DefaultServeMux, where importing net/http/pprof also registers them.
//   - A token on every request, as the basic-auth password: loopback alone is
//     not enough, since Docker Desktop forwards host.docker.internal to the
//     host's 127.0.0.1, and a relay or forward added later could reach a
//     pool's. A profile shows the process's command line and every goroutine's
//     stack, and a long one stalls it.
//   - No WriteTimeout, since a CPU profile or trace answers only after the
//     seconds it samples; stop is Close, not Shutdown.
//   - A configured address that cannot be bound fails the process's start.
package pprofserve

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
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Off is the address that opens no profiling listener. Empty means the same;
// the word exists for a setting that has a default to turn off, such as the
// dev loop's, where an empty value reads as unset.
const Off = "off"

// Config is one process's profiling listener.
type Config struct {
	// Setting names where Addr was configured, and begins every error, so an
	// operator knows what to change.
	Setting string
	// Addr is the loopback host:port to serve on, already Validated. Empty
	// serves nothing.
	Addr string
	// TokenFile is where the token is written (0600) on start, and removed on
	// stop.
	TokenFile string
	// Remedy says, after a bind failure, what to change for the process to
	// start.
	Remedy string
}

// Validate accepts an empty address (no listener) or a host:port whose host
// is a loopback IP or localhost. A wildcard, a LAN address and a hostname
// that may resolve anywhere are all refused. setting names the address in the
// error.
func Validate(setting, addr string) error {
	if addr == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s: %q is not host:port: %w", setting, addr, err)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("%s: %q does not name a port", setting, addr)
	}
	if !isLoopbackHost(host) {
		return fmt.Errorf("%s: %q is not a loopback address: profiling is served only on 127.0.0.1, ::1 or localhost", setting, addr)
	}
	return nil
}

// isLoopbackHost reports whether host is localhost or a loopback IP literal.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// Start serves the pprof endpoints as cfg says and returns the address it
// bound and what stops it. An empty cfg.Addr serves nothing, binds nothing and
// writes no token.
func Start(ctx context.Context, cfg Config) (bound net.Addr, stop func(), err error) {
	if cfg.Addr == "" {
		return nil, func() {}, nil
	}
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", cfg.Addr)
	if err != nil {
		if cfg.Remedy != "" {
			return nil, nil, fmt.Errorf("%s: %w (%s)", cfg.Setting, err, cfg.Remedy)
		}
		return nil, nil, fmt.Errorf("%s: %w", cfg.Setting, err)
	}
	// localhost is accepted by name, so check what it resolved to: the loopback
	// rule is about the socket, not the spelling.
	if tcp, ok := listener.Addr().(*net.TCPAddr); !ok || !tcp.IP.IsLoopback() {
		_ = listener.Close()
		return nil, nil, fmt.Errorf("%s: %s bound %s, which is not a loopback address", cfg.Setting, cfg.Addr, listener.Addr())
	}
	token, err := writeToken(cfg.TokenFile)
	if err != nil {
		_ = listener.Close()
		return nil, nil, fmt.Errorf("%s: write token: %w", cfg.Setting, err)
	}
	server := &http.Server{
		Handler:           requireToken(token, cfg.TokenFile, handler()),
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
	log.Printf("pprof listening on http://%s/debug/pprof/, with the basic-auth password in %s", listener.Addr(), cfg.TokenFile)
	return listener.Addr(), func() {
		// Close, not Shutdown: a profile being taken is cut off rather than
		// holding the process's exit for up to its sampling duration.
		_ = server.Close()
		_ = os.Remove(cfg.TokenFile)
	}, nil
}

// writeToken writes a new random token to path, readable by this user alone,
// and returns it. Written to a temporary file and renamed, so a reader never
// sees a partial token and the file is never briefly wider than 0600.
func writeToken(path string) (string, error) {
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

// requireToken refuses a request whose basic-auth password is not token. The
// username is ignored. The refusal names the file, which is no secret: it is
// readable only by the user who may profile this process.
func requireToken(token, tokenFile string, next http.Handler) http.Handler {
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

// handler is the standard net/http/pprof route set. The index serves every
// runtime profile by name (heap, goroutine, allocs, block, mutex,
// threadcreate); the CPU profile, trace, cmdline and symbol lookup have
// handlers of their own.
func handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}
