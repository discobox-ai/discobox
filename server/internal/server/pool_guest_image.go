package server

import (
	"errors"
	"io"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/services"
)

// Headers for a guest image build.
//
// The destination is known before the build starts and is sent as a header. The
// outcome is not, and cannot be a status code either — the response has begun
// long before the build ends — so it is an HTTP trailer, which is what trailers
// are for. A client that reads the body to the end finds the trailer set only
// when the build failed, and never has to parse the build's own output to know.
const (
	guestImageDestinationHeader = "X-Discobox-Guest-Image-Destination"
	guestImageErrorTrailer      = "X-Discobox-Guest-Image-Error"
)

// registerPoolGuestImageRoutes exposes rebuilding the guest image a pool's
// backend boots, on that pool's own host (ADR 0062 §7).
//
// Hand-wired beside the console and the host log, and for the same reason: it
// goes through the provider driver rather than the pool agent, and it streams.
// It is a POST because it writes — to the pool's Docker daemon and to the guest
// image directory on this machine.
func registerPoolGuestImageRoutes(router chi.Router, service services.PoolService) {
	router.Method(http.MethodPost, "/api/projects/{projectId}/pools/{poolId}/guest-image", poolGuestImageHandler(service))
}

func poolGuestImageHandler(service services.PoolService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if service == nil {
			writeSandboxAgentProxyError(w, http.StatusServiceUnavailable, "pool service is not configured")
			return
		}
		opts := sandbox.GuestImageBuildOptions{
			SourceDir:   r.URL.Query().Get("source"),
			RestartHost: r.URL.Query().Get("restart") == "true",
		}
		build, err := service.BuildPoolGuestImage(r.Context(), chi.URLParam(r, "projectId"), chi.URLParam(r, "poolId"), opts)
		if err != nil {
			// Everything knowable before the build starts is reported as a
			// status: an unbuildable backend, a source directory that is not a
			// checkout, a pool whose Docker cannot be reached.
			writeSandboxAgentProxyError(w, statusCodeForProxyError(err), err.Error())
			return
		}
		var closeOnce sync.Once
		closeStream := func() { closeOnce.Do(func() { _ = build.Close() }) }
		defer closeStream()
		building := make(chan struct{})
		defer close(building)
		go func() {
			select {
			case <-r.Context().Done():
				// The client went away. Closing the build releases the pool's
				// Docker lease and the BuildKit session; the build itself is
				// ended by the request context it was started with.
				closeStream()
			case <-building:
			}
		}()

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set(guestImageDestinationHeader, build.Destination)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Trailer", guestImageErrorTrailer)
		w.WriteHeader(http.StatusOK)
		_, copyErr := io.Copy(&flushingWriter{writer: w, controller: http.NewResponseController(w)}, build)
		// io.EOF is the build finishing; anything else is the build failing,
		// which the stream carries as its own read error.
		if copyErr != nil && !errors.Is(copyErr, io.EOF) {
			w.Header().Set(http.TrailerPrefix+guestImageErrorTrailer, copyErr.Error())
		}
	})
}
