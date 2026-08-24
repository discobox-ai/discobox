package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	rootopenapi "github.com/discobox-ai/discobox/api/openapi"
	"github.com/discobox-ai/discobox/health"
)

const scalarDocsHTML = `<!doctype html>
<html>
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Discobox API Reference</title>
  <style>
    body { margin: 0; }
  </style>
</head>
<body>
  <script id="api-reference" data-url="/openapi.yaml"></script>
  <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script>
</body>
</html>
`

// RegisterDocsRoutes serves the canonical OpenAPI contract and Scalar API docs.
func RegisterDocsRoutes(router chi.Router) {
	router.Get("/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/openapi+yaml; charset=utf-8")
		_, _ = w.Write(rootopenapi.ServerYAML)
	})
	router.Get("/docs", serveScalarDocs)
	router.Get("/docs/", serveScalarDocs)
}

// RegisterHealthRoutes serves process readiness.
//
// It reports rather than merely answering: a caller waiting on a server it
// launched wants to know which server it reached and how long it has been up,
// and a bare 204 tells it neither. The starting half of the same contract is
// served by startupHandler before this router exists.
func RegisterHealthRoutes(router chi.Router) {
	router.Get(health.Path, func(w http.ResponseWriter, _ *http.Request) {
		writeHealthStatus(w, http.StatusOK, readyStatus())
	})
}

func serveScalarDocs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(scalarDocsHTML))
}
