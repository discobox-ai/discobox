// Package requestprogress carries transient request progress in informational
// HTTP responses. The final status and JSON body keep their ordinary contract.
package requestprogress

import (
	"context"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
)

const Header = "X-Discobox-Progress"

type reporterKey struct{}

// Report publishes a phase change for this request; empty means the phase ended.
func Report(ctx context.Context, text string) {
	if report, ok := ctx.Value(reporterKey{}).(func(string)); ok {
		report(text)
	}
}

// Middleware attaches the response's progress writer to the request. Callers
// report synchronously before the final response is written, never concurrently.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), reporterKey{}, func(text string) {
			w.Header().Set(Header, text)
			w.WriteHeader(http.StatusProcessing)
			w.Header().Del(Header)
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Observe receives phase changes while a request is pending. It composes with
// any existing HTTP trace. A client that does not observe still gets normal JSON.
func Observe(ctx context.Context, report func(string)) context.Context {
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
			if code == http.StatusProcessing && report != nil {
				report(header.Get(Header))
			}
			return nil
		},
	})
}
