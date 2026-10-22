package agentcreds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/discobox-ai/discobox/pkg/policy"
)

// Service is the server half of the protocol. An implementation owns the four
// decisions the protocol does not make: whose credentials these are, what Get
// returns, how a request reaches a human, and whether a command is judged
// before Get answers.
type Service interface {
	// List returns the credentials the caller may use and their approved uses.
	// It never returns values.
	List(ctx context.Context) ([]Credential, error)
	// Request records an ask and returns immediately with a pending status.
	// Approval is human-latency, so this must not block on it.
	Request(ctx context.Context, body RequestBody) (RequestStatus, error)
	// RequestStatus reads a request's current status.
	RequestStatus(ctx context.Context, requestID string) (RequestStatus, error)
	// Get returns a value for one declared command, or ErrDenied when the
	// implementation judged the command and refused it.
	Get(ctx context.Context, body UseBody) (UseResponse, error)
	// Trusts returns the host trusts the caller holds (ADR 0149).
	Trusts(ctx context.Context) ([]Trust, error)
	// RequestTrust records an ask to trust a host and returns immediately:
	// pending when a human must answer, or already settled as StatusUnneeded
	// when the host's chain verifies without a pin.
	RequestTrust(ctx context.Context, body TrustRequestBody) (TrustRequestStatus, error)
	// TrustRequestStatus reads a trust request's current status.
	TrustRequestStatus(ctx context.Context, requestID string) (TrustRequestStatus, error)
	// AuditHTTP returns the caller's own audited HTTP exchanges, newest first
	// (ADR 26-10-02-678). The query's limit is already settled. An
	// implementation that keeps no egress record returns none.
	AuditHTTP(ctx context.Context, query AuditQuery) ([]AuditExchange, error)
	// Facts returns what the implementation recorded about the caller and
	// what each policy variable resolves to for it (ADR 26-10-20-782 §4). An
	// implementation that records nothing returns empty facts and the
	// variables resolved against them.
	Facts(ctx context.Context) (FactsResponse, error)
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
		// host is the field a request once named its destination by. A client
		// that predates hosts sends it on every ask, empty when it named none,
		// which is an ask for nothing in particular and is read as one. A host
		// it did name is refused rather than dropped (ADR 26-10-02-393 §4):
		// dropped, a free-form ask would name no destination, and an ask by a
		// well-known ID would widen from the host it named to the ID's whole
		// site — wider than it meant, and what a person would be shown to
		// approve. Such a client cannot send hosts, so the refusal says what
		// it can do: be replaced.
		var body struct {
			RequestBody
			Host *string `json:"host"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Host != nil && strings.TrimSpace(*body.Host) != "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{
				Error: `"host" is no longer read; destinations are "hosts", a list. A client that sends "host" predates it: upgrade or recreate this sandbox for one that does`,
				Code:  CodeInvalid,
			})
			return
		}
		status, err := svc.Request(r.Context(), body.RequestBody)
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
	mux.HandleFunc("GET "+PathTrusts, func(w http.ResponseWriter, r *http.Request) {
		trusts, err := svc.Trusts(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		if trusts == nil {
			trusts = []Trust{}
		}
		writeJSON(w, http.StatusOK, TrustListResponse{Trusts: trusts})
	})
	mux.HandleFunc("POST "+PathTrustRequests, func(w http.ResponseWriter, r *http.Request) {
		var body TrustRequestBody
		if !decode(w, r, &body) {
			return
		}
		status, err := svc.RequestTrust(r.Context(), body)
		if err != nil {
			writeError(w, err)
			return
		}
		// 202 while a human has yet to answer; 200 for an ask that settled
		// on the spot because it needed no one.
		code := http.StatusAccepted
		if status.Settled() {
			code = http.StatusOK
		}
		writeJSON(w, code, status)
	})
	mux.HandleFunc("GET "+PathTrustRequests+"/{requestId}", func(w http.ResponseWriter, r *http.Request) {
		status, err := svc.TrustRequestStatus(r.Context(), r.PathValue("requestId"))
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, status)
	})
	mux.HandleFunc("GET "+PathAuditHTTP, func(w http.ResponseWriter, r *http.Request) {
		query, err := parseAuditQuery(r.URL.Query())
		if err != nil {
			writeError(w, err)
			return
		}
		exchanges, err := svc.AuditHTTP(r.Context(), query)
		if err != nil {
			writeError(w, err)
			return
		}
		if exchanges == nil {
			exchanges = []AuditExchange{}
		}
		writeJSON(w, http.StatusOK, AuditHTTPResponse{Exchanges: exchanges})
	})
	mux.HandleFunc("GET "+PathFacts, func(w http.ResponseWriter, r *http.Request) {
		facts, err := svc.Facts(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		if facts.Variables == nil {
			facts.Variables = []policy.Variable{}
		}
		writeJSON(w, http.StatusOK, facts)
	})
	return mux
}

// MaxBodyBytes bounds a request body. Every body in this protocol is a
// handful of short strings and one verdict, whose prompt a caller keeps small
// enough to fit here even escaped, so anything larger is a mistake or an
// attack.
const MaxBodyBytes = 64 << 10

// MaxResponseBytes bounds a response body a client decodes. Every answer but
// one is as small as a request; an audit page of MaxAuditLimit exchanges is
// the one that is not.
const MaxResponseBytes = 1 << 20

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBodyBytes)).Decode(target); err != nil {
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
