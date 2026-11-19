package server

import (
	"net/http"
	"strings"
)

// maxInventoryReportBodyBytes bounds a pool agent's inventory report. The
// generated server reads a JSON body whole before it decodes it, and this is
// the one pool route whose body grows with what sandboxes hold on their
// screens. A pool agent keeps a report's inventories under 8 MiB and sends
// more in further reports (ADR 26-10-11-657 §2); this is that, with room for
// the JSON around them.
const maxInventoryReportBodyBytes = 16 << 20

// boundInventoryReports refuses an inventory report larger than
// maxInventoryReportBodyBytes before the generated server buffers it.
func boundInventoryReports(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/pool-agent/") && strings.HasSuffix(r.URL.Path, "/sandbox-inventories") {
			r.Body = http.MaxBytesReader(w, r.Body, maxInventoryReportBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}
