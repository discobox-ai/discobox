package agentcreds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Service is the server half of the protocol. An implementation owns the three
// decisions the protocol does not make: whose credentials these are, what Get
// returns, and how a request reaches a human.
type Service interface {
	// List returns the credentials the caller may use and their approved uses.
	// It never returns values.
	List(ctx context.Context) ([]Credential, error)
	// Request records an ask and returns immediately with a pending status.
	// Approval is human-latency, so this must not block on it.
	Request(ctx context.Context, body RequestBody) (RequestStatus, error)
	// RequestStatus reads a request's current status.
	RequestStatus(ctx context.Context, requestID string) (RequestStatus, error)
	// Get returns a value for one declared command.
	Get(ctx context.Context, body UseBody) (UseResponse, error)
	// ReportDenial records a verdict for a command the judge refused, which
	// never reached Get (ADR 0091 §3).
	ReportDenial(ctx context.Context, body DenialReport) error
}

// ErrNotFound makes a handler answer 404. It is the "this id means nothing to
// me" signal, distinct from a use that exists but is no longer live.
var ErrNotFound = errors.New("not found")

// ErrDenied makes a handler answer 403: the id is real but the caller may not
// use it now (revoked, expired, or never approved for this).
var ErrDenied = errors.New("denied")

// ErrInvalid makes a handler answer 400.
var ErrInvalid = errors.New("invalid request")

// NewHandler serves the protocol over svc. The handler carries no
// authentication of its own: the transport it is mounted on decides who the
// caller is, which is what lets Discobox serve it on sandbox loopback with no
// token and another implementation front it with a bearer token.
func NewHandler(svc Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PathCredentials, func(w http.ResponseWriter, r *http.Request) {
		credentials, err := svc.List(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		if credentials == nil {
			credentials = []Credential{}
		}
		writeJSON(w, http.StatusOK, ListResponse{Credentials: credentials})
	})
	mux.HandleFunc("POST "+PathRequests, func(w http.ResponseWriter, r *http.Request) {
		var body RequestBody
		if !decode(w, r, &body) {
			return
		}
		status, err := svc.Request(r.Context(), body)
		if err != nil {
			writeError(w, err)
			return
		}
		// 202: the ask was recorded, and nothing has been decided yet.
		writeJSON(w, http.StatusAccepted, status)
	})
	mux.HandleFunc("GET "+PathRequests+"/{requestId}", func(w http.ResponseWriter, r *http.Request) {
		status, err := svc.RequestStatus(r.Context(), r.PathValue("requestId"))
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, status)
	})
	mux.HandleFunc("POST "+PathUse, func(w http.ResponseWriter, r *http.Request) {
		var body UseBody
		if !decode(w, r, &body) {
			return
		}
		out, err := svc.Get(r.Context(), body)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("POST "+PathDenials, func(w http.ResponseWriter, r *http.Request) {
		var body DenialReport
		if !decode(w, r, &body) {
			return
		}
		if err := svc.ReportDenial(r.Context(), body); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// maxBodyBytes bounds a request body. Every body in this protocol is a handful
// of short strings, so anything larger is a mistake or an attack.
const maxBodyBytes = 64 << 10

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(target); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: fmt.Sprintf("parse request body: %v", err),
			Code:  CodeInvalid,
		})
		return false
	}
	return true
}

// Code maps an error onto its stable protocol code. An error that is none of
// the package's sentinels is CodeUnavailable: from a caller's point of view an
// unclassified server failure is a "try again", not a "you did something
// wrong".
func Code(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNotFound):
		return CodeNotFound
	case errors.Is(err, ErrDenied):
		return CodeDenied
	case errors.Is(err, ErrInvalid):
		return CodeInvalid
	default:
		return CodeUnavailable
	}
}

// Message renders an error as the sentence that belongs beside its Code.
//
// It strips the sentinel prefix that %w wrapping puts in front — the code
// already says "denied", so a message of "denied: no live approved use" states
// it twice, and every hop that re-wraps would state it again. Code and Message
// are two halves of one answer, not two copies of the same one.
func Message(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	for _, sentinel := range []error{ErrNotFound, ErrDenied, ErrInvalid} {
		if !errors.Is(err, sentinel) {
			continue
		}
		if trimmed, ok := strings.CutPrefix(message, sentinel.Error()+": "); ok {
			return trimmed
		}
	}
	return message
}

// codeStatus maps a protocol code onto the HTTP status that carries it.
func codeStatus(code string) int {
	switch code {
	case CodeNotFound:
		return http.StatusNotFound
	case CodeDenied:
		return http.StatusForbidden
	case CodeInvalid:
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func writeError(w http.ResponseWriter, err error) {
	code := Code(err)
	status := codeStatus(code)
	message := Message(err)
	if message == "" {
		message = http.StatusText(status)
	}
	writeJSON(w, status, ErrorResponse{Error: message, Code: code})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	// Marshal before touching the response: once the status line is written the
	// only way to report a failure is to hang up, and the client would read a
	// truncated body as a protocol error rather than a server one.
	body, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
