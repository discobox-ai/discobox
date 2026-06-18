package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	rootopenapi "github.com/obot-platform/discobox/api/openapi"
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
	router.Get("/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/openapi+yaml; charset=utf-8")
		_, _ = w.Write(rootopenapi.ServerYAML)
	})
	router.Get("/docs", serveScalarDocs)
	router.Get("/docs/", serveScalarDocs)
}

func serveScalarDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(scalarDocsHTML))
}
