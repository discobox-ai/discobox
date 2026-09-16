package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandboxexport"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// registerSandboxTransferRoutes exposes moving a discobox between servers
// (ADR 0123).
//
// Hand-wired rather than declared in the OpenAPI contract, for the reason the
// pool log and the git proxy are: both bodies are an unbounded stream of
// opaque bytes, which the contract has no way to describe and the generated
// scaffold would buffer. Everything about a transfer -- a workspace, its git
// history, a home directory -- is too big to hold in memory at either end.
func registerSandboxTransferRoutes(router chi.Router, service services.SandboxService) {
	router.Method(http.MethodGet, "/api/projects/{projectId}/sandboxes/{sandboxId}/export", sandboxExportHandler(service))
	router.Method(http.MethodPost, "/api/projects/{projectId}/sandboxes/import", sandboxImportHandler(service))
}

func sandboxExportHandler(service services.SandboxService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if service == nil {
			writeSandboxAgentProxyError(w, http.StatusServiceUnavailable, "sandbox service is not configured")
			return
		}
		projectID := chi.URLParam(r, "projectId")
		sandboxID := chi.URLParam(r, "sandboxId")
		stream, err := service.ExportSandbox(r.Context(), projectID, sandboxID)
		if err != nil {
			// Reported before any of the body. Once bytes are flowing, "it is
			// running" can no longer be a status -- and that refusal is the
			// whole reason the check happens up front.
			writeSandboxAgentProxyError(w, statusCodeForProxyError(err), err.Error())
			return
		}
		// The stream holds a pool-agent transport lease and a walk of the
		// sandbox's tree on the far side of it. A client that goes away mid
		// download has to close both, rather than leaving the walk writing into
		// a pipe nobody reads.
		var closeOnce sync.Once
		closeStream := func() { closeOnce.Do(func() { _ = stream.Close() }) }
		defer closeStream()
		copying := make(chan struct{})
		defer close(copying)
		go func() {
			select {
			case <-r.Context().Done():
				closeStream()
			case <-copying:
			}
		}()

		w.Header().Set("Content-Type", sandboxexport.MediaType)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Disposition", exportContentDisposition(exportFileName(r, sandboxID)))
		w.WriteHeader(http.StatusOK)
		// A failure part way through ends the response rather than appending to
		// it. The body is a tar, and a sentence in the middle of one is not a
		// diagnostic -- it is a corrupt archive.
		//
		// Ending it means aborting the connection. Returning would have net/http
		// finish the chunked body cleanly, and a client writing it to a file
		// would report an export that failed as one that succeeded. The missing
		// SHA256SUMS refuses such a file at import; the abort is what keeps it
		// from being written as though it were whole.
		if _, err := io.Copy(w, stream); err != nil {
			panic(http.ErrAbortHandler)
		}
	})
}

func sandboxImportHandler(service services.SandboxService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if service == nil {
			writeSandboxAgentProxyError(w, http.StatusServiceUnavailable, "sandbox service is not configured")
			return
		}
		projectID := chi.URLParam(r, "projectId")
		query := r.URL.Query()
		result, err := service.ImportSandbox(r.Context(), projectID, r.Body, services.SandboxImportOptions{
			Name:        strings.TrimSpace(query.Get("name")),
			PoolID:      strings.TrimSpace(query.Get("pool")),
			HarnessSlug: strings.TrimSpace(query.Get("harness")),
		})
		if err != nil {
			writeSandboxAgentProxyError(w, statusCodeForProxyError(err), err.Error())
			return
		}
		// Through the same mapper every generated sandbox route uses. Encoding
		// the persistence model straight out looks like it works -- it is JSON,
		// and it has the right values in it -- but it is a different shape from
		// the API's Sandbox (`name` is top-level on one and under `config` on
		// the other), and a client decoding the generated type refuses it
		// outright (ADR 0118: whoever serves an API is strict).
		var fallback *model.HarnessConfig
		if config, err := service.FallbackHarnessConfig(r.Context(), projectID); err == nil {
			fallback = config
		}
		body, err := services.SandboxToAPI(result.Sandbox, fallback)
		if err != nil {
			writeSandboxAgentProxyError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(sandboxImportResponse{
			Sandbox:  &body,
			Warnings: result.Warnings,
		})
	})
}

// sandboxImportResponse is the import's own answer rather than a bare sandbox,
// because an import has something to say that a create does not: what it could
// not carry over. A secret the destination has no equivalent of does not fail
// the import, and a client that only ever saw the sandbox would have no way to
// learn it is missing until the harness said so from inside.
type sandboxImportResponse struct {
	Sandbox  *serverapi.Sandbox `json:"sandbox"`
	Warnings []string           `json:"warnings,omitempty"`
}

// exportFileName is what a browser or a curl -O should call the download. The
// discobox's name is not known here without a lookup the export already did, so
// the client names it and this falls back to the ID.
func exportFileName(r *http.Request, sandboxID string) string {
	name := strings.TrimSpace(r.URL.Query().Get("filename"))
	if name == "" {
		name = sandboxID
	}
	// One path element, no directories: this string ends up in a header a
	// client may write to disk under.
	name = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(name, "/", "-"), `\`, "-"))
	if name == "" || name == "." || name == ".." {
		name = sandboxID
	}
	if !strings.HasSuffix(name, sandboxexport.FileExtension) {
		name += sandboxexport.FileExtension
	}
	return name
}

func exportContentDisposition(name string) string {
	// The quoted form for the bytes that fit in it, and RFC 5987's encoding
	// beside it for the rest: a discobox may be named in any script, and a
	// filename* parameter is how a name that is not ASCII survives the header.
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return -1
		}
		return r
	}, name)
	if ascii == "" {
		ascii = "discobox" + sandboxexport.FileExtension
	}
	return fmt.Sprintf("attachment; filename=%q; filename*=UTF-8''%s", ascii, url.PathEscape(name))
}
