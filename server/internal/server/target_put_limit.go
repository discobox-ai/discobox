package server

import (
	"net/http"
	"strings"
)

// maxTargetPutBodyBytes bounds a put of a project's target entry. The
// generated server reads a JSON body whole before it decodes it, and this is
// the one person's route whose body carries documents' text. A document of
// more than this, or several that add up to more, is given by URL instead.
const maxTargetPutBodyBytes = 64 << 20

// boundTargetPuts refuses a target entry put larger than maxTargetPutBodyBytes
// before the generated server buffers it.
func boundTargetPuts(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/projects/") && strings.Contains(r.URL.Path, "/targets/") {
			r.Body = http.MaxBytesReader(w, r.Body, maxTargetPutBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}
