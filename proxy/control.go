package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/auditid"
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
	mux.HandleFunc("GET /audit/dns", s.handleControlListDNS)
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
	// A SOCKS connect is a tunnel the proxy never reads: no sentinel is swapped
	// in one, and it has no HTTP status or policy verdict of that kind. A filter
	// on those is refused rather than ignored, because answering 200 with every
	// row reads as "all of these matched" to whoever asked.
	for _, param := range httpOnlyControlParams {
		if r.URL.Query().Has(param) {
			http.Error(w, param+" does not apply to SOCKS connects", http.StatusBadRequest)
			return
		}
	}
	opts, err := controlQueryOptions(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rows, err := s.audit.ListSOCKS(r.Context(), opts)
	writeControlJSON(w, rows, err)
}

func (s *Server) handleControlListDNS(w http.ResponseWriter, r *http.Request) {
	// A DNS query has a name, not a host, and none of an exchange's status,
	// policy verdict or credential use; a filter on those is refused rather
	// than ignored, for the reason the SOCKS read gives.
	for _, param := range []string{"host", "use_id", "min_status", "max_status", "blocked"} {
		if r.URL.Query().Has(param) {
			http.Error(w, param+" does not apply to DNS queries", http.StatusBadRequest)
			return
		}
	}
	query := r.URL.Query()
	since, ascending, limit, err := controlWindow(query)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	opts := audit.DNSQueryOptions{
		ClientID:  query.Get("client_id"),
		Name:      query.Get("name"),
		Since:     since,
		Ascending: ascending,
		Limit:     limit,
	}
	for param, field := range map[string]*auditid.DNSQueryID{"after_id": &opts.AfterID, "id": &opts.ID} {
		if raw := query.Get(param); raw != "" {
			id, err := auditid.ParseDNSQuery(raw)
			if err != nil {
				http.Error(w, fmt.Sprintf("%s %q: %v", param, raw, err), http.StatusBadRequest)
				return
			}
			*field = id
		}
	}
	rows, err := s.audit.ListDNS(r.Context(), opts)
	writeControlJSON(w, rows, err)
}

func (s *Server) handleControlDropped(w http.ResponseWriter, _ *http.Request) {
	writeControlJSON(w, map[string]uint64{"dropped": s.audit.Dropped(), "dnsDropped": s.audit.DNSDropped()}, nil)
}

func (s *Server) handleControlHTTPArtifact(w http.ResponseWriter, r *http.Request) {
	id, artifact, ok := controlHTTPArtifact(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if artifact == "" {
		// The row itself, which is every field the recorder wrote rather than
		// the summary a list returns.
		row, err := s.audit.GetHTTP(r.Context(), id, r.URL.Query().Get("client_id"))
		if errors.Is(err, gorm.ErrRecordNotFound) {
			http.NotFound(w, r)
			return
		}
		writeControlJSON(w, row, err)
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

// httpOnlyControlParams are the filters that only mean something for an HTTP
// exchange.
//
// after_id is one of them because the cursor is spelled http_<row>
// (auditid.ExchangeID): the prefix is what says which trail an ID names, and a
// SOCKS read handed one would answer an arbitrary suffix of a different table
// as though the cursor meant something there.
var httpOnlyControlParams = []string{"use_id", "min_status", "max_status", "blocked", "after_id"}

func controlQueryOptions(r *http.Request) (audit.QueryOptions, error) {
	query := r.URL.Query()
	since, ascending, limit, err := controlWindow(query)
	if err != nil {
		return audit.QueryOptions{}, err
	}
	opts := audit.QueryOptions{
		ClientID:  query.Get("client_id"),
		Host:      query.Get("host"),
		UseID:     query.Get("use_id"),
		Since:     since,
		Ascending: ascending,
		Limit:     limit,
	}
	for param, field := range map[string]*int{"min_status": &opts.MinStatus, "max_status": &opts.MaxStatus} {
		if raw := query.Get(param); raw != "" {
			status, err := strconv.Atoi(raw)
			if err != nil || status < 0 {
				return audit.QueryOptions{}, fmt.Errorf("%s %q is not a status code", param, raw)
			}
			*field = status
		}
	}
	if raw := query.Get("after_id"); raw != "" {
		after, err := auditid.ParseExchange(raw)
		if err != nil {
			return audit.QueryOptions{}, fmt.Errorf("after_id %q: %w", raw, err)
		}
		opts.AfterID = after
	}
	if raw := query.Get("blocked"); raw != "" {
		blocked, err := strconv.ParseBool(raw)
		if err != nil {
			return audit.QueryOptions{}, fmt.Errorf("blocked %q is not true or false", raw)
		}
		opts.Blocked = &blocked
	}
	return opts, nil
}

// controlWindow reads the parameters every audit list shares: where the read
// starts in time, which way it runs, and how many rows it returns.
func controlWindow(query url.Values) (since time.Time, ascending bool, limit int, err error) {
	limit, _ = strconv.Atoi(query.Get("limit"))
	if raw := query.Get("since"); raw != "" {
		if since, err = time.Parse(time.RFC3339Nano, raw); err != nil {
			return time.Time{}, false, 0, fmt.Errorf("since %q is not an RFC 3339 time", raw)
		}
	}
	switch order := query.Get("order"); order {
	case "", "desc":
	case "asc":
		ascending = true
	default:
		return time.Time{}, false, 0, fmt.Errorf("order %q is not asc or desc", order)
	}
	return since, ascending, limit, nil
}

// controlHTTPArtifact reads /audit/http/{id}, whose artifact is empty and
// which is the row, and /audit/http/{id}/{artifact}, which is what it recorded
// beside it. The id is spelled the way every API above this one spells it.
func controlHTTPArtifact(path string) (auditid.ExchangeID, string, bool) {
	rest := strings.TrimPrefix(path, "/audit/http/")
	idPart, artifact, hasArtifact := strings.Cut(rest, "/")
	if idPart == "" || (hasArtifact && artifact == "") {
		return 0, "", false
	}
	id, err := auditid.ParseExchange(idPart)
	if err != nil {
		return 0, "", false
	}
	return id, artifact, true
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
