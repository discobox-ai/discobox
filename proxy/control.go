package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/proxy/internal/audit"
	"gorm.io/gorm"
)

// ControlHandler returns the read-only proxy control API.
func (s *Server) ControlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /audit/http", s.handleControlListHTTP)
	mux.HandleFunc("GET /audit/socks", s.handleControlListSOCKS)
	mux.HandleFunc("GET /audit/dropped", s.handleControlDropped)
	mux.HandleFunc("GET /audit/http/", s.handleControlHTTPArtifact)
	return s.controlAuth.Middleware(mux)
}

// ListenAndServeControl starts the optional read-only control API listener.
func (s *Server) ListenAndServeControl(ctx context.Context) error {
	if s.cfg.Control.ListenAddress == "" {
		return nil
	}
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", s.cfg.Control.ListenAddress)
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler:           s.ControlHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context {
			return contextOrBackground(ctx)
		},
	}
	go func() {
		<-ctx.Done()
		_ = listener.Close()
		_ = server.Close()
	}()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (s *Server) handleControlListHTTP(w http.ResponseWriter, r *http.Request) {
	opts, err := controlQueryOptions(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rows, err := s.audit.ListHTTP(r.Context(), opts)
	writeControlJSON(w, rows, err)
}

func (s *Server) handleControlListSOCKS(w http.ResponseWriter, r *http.Request) {
	// A SOCKS connect is a tunnel the proxy never reads, so no sentinel is ever
	// swapped in one and there is nothing for use_id to select. Refusing beats
	// answering 200 with every row, which reads as "this use touched all of
	// these" to whoever asked.
	if r.URL.Query().Has("use_id") {
		http.Error(w, "use_id does not apply to SOCKS connects", http.StatusBadRequest)
		return
	}
	opts, err := controlQueryOptions(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rows, err := s.audit.ListSOCKS(r.Context(), opts)
	writeControlJSON(w, rows, err)
}

func (s *Server) handleControlDropped(w http.ResponseWriter, _ *http.Request) {
	writeControlJSON(w, map[string]uint64{"dropped": s.audit.Dropped()}, nil)
}

func (s *Server) handleControlHTTPArtifact(w http.ResponseWriter, r *http.Request) {
	id, artifact, ok := controlHTTPArtifact(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	row, err := s.audit.GetHTTP(r.Context(), id, r.URL.Query().Get("client_id"))
	if errors.Is(err, gorm.ErrRecordNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var (
		file        httpFile
		contentType string
		format      string
		name        string
		openErr     error
	)
	switch artifact {
	case "stream":
		file, openErr = s.audit.OpenStream(row)
		contentType = "application/vnd.discobox.upgrade-stream"
		format = row.StreamFormat
		name = row.StreamFile
	case "request-body":
		file, openErr = s.audit.OpenBody(row, audit.BodyKindRequest)
		contentType = "application/octet-stream"
		format = row.RequestBodyFormat
		name = row.RequestBodyFile
	case "response-body":
		file, openErr = s.audit.OpenBody(row, audit.BodyKindResponse)
		contentType = "application/octet-stream"
		format = row.ResponseBodyFormat
		name = row.ResponseBodyFile
	default:
		http.NotFound(w, r)
		return
	}
	if errors.Is(openErr, net.ErrClosed) {
		http.NotFound(w, r)
		return
	}
	if openErr != nil {
		http.Error(w, openErr.Error(), http.StatusNotFound)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", contentType)
	if artifact == "stream" {
		w.Header().Set("X-Discobox-Stream-Format", format)
	} else {
		w.Header().Set("X-Discobox-Body-Format", format)
	}
	http.ServeContent(w, r, name, row.CreatedAt, file)
}

func controlQueryOptions(r *http.Request) (audit.QueryOptions, error) {
	query := r.URL.Query()
	limit, _ := strconv.Atoi(query.Get("limit"))
	opts := audit.QueryOptions{
		ClientID: query.Get("client_id"),
		Host:     query.Get("host"),
		UseID:    query.Get("use_id"),
		Limit:    limit,
	}
	if raw := query.Get("since"); raw != "" {
		since, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return audit.QueryOptions{}, fmt.Errorf("since %q is not an RFC 3339 time", raw)
		}
		opts.Since = since
	}
	return opts, nil
}

func controlHTTPArtifact(path string) (uint, string, bool) {
	rest := strings.TrimPrefix(path, "/audit/http/")
	idPart, artifact, ok := strings.Cut(rest, "/")
	if !ok || idPart == "" || artifact == "" {
		return 0, "", false
	}
	id, err := strconv.ParseUint(idPart, 10, 0)
	if err != nil {
		return 0, "", false
	}
	return uint(id), artifact, true
}

type httpFile interface {
	http.File
}

func writeControlJSON(w http.ResponseWriter, value any, err error) {
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
