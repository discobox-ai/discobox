package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/obot-platform/discobox/proxy/internal/audit"
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
	rows, err := s.audit.ListHTTP(r.Context(), controlQueryOptions(r))
	writeControlJSON(w, rows, err)
}

func (s *Server) handleControlListSOCKS(w http.ResponseWriter, r *http.Request) {
	rows, err := s.audit.ListSOCKS(r.Context(), controlQueryOptions(r))
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

func controlQueryOptions(r *http.Request) audit.QueryOptions {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	return audit.QueryOptions{
		ClientID: r.URL.Query().Get("client_id"),
		Host:     r.URL.Query().Get("host"),
		Limit:    limit,
	}
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
