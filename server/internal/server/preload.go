package server

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/discobox-ai/discobox/server/internal/services"
)

const (
	// preloadTimeout bounds the whole preload.
	//
	// Readiness waits on preloading, so this is also how long a server can
	// refuse to answer. That is the reason it is bounded at all: a pool whose
	// VM will never boot must not make the server permanently unreachable,
	// because the commands for diagnosing that pool are served by the server.
	//
	// Long, because the thing being waited on is a VM boot and some gigabytes
	// of image over a home connection, and the entire point is to pay that
	// once here rather than in front of a user later.
	preloadTimeout = 30 * time.Minute
)

// preloadImages pulls the images sandboxes will want onto every known pool,
// before the server reports itself ready.
//
// Doing it here is a trade, and worth naming. It makes the first start slow and
// says so, in exchange for every later command being fast: without it the first
// sandbox on a cold machine waits for a VM to boot and two gigabytes of harness
// image to arrive, at whatever moment the user happened to ask for one, with a
// status line as the only clue.
//
// It never fails startup. Preloading is an optimisation for a wait that would
// otherwise happen later, so a pool that cannot come up or an image that no
// longer exists costs exactly that optimisation — the sandbox that needs the
// image will pull it then, as it always did.
func preloadImages(ctx context.Context, svc services.Services, startup *startupHandler) {
	if svc.Preload == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, preloadTimeout)
	defer cancel()

	startup.setPhase("preloading images")
	started := time.Now()
	err := svc.Preload.PreloadImages(ctx, func(line string) {
		startup.setPhase("preloading images — " + line)
	})
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		log.Printf("preloading images gave up after %s; the images not yet pulled will be pulled when a sandbox needs one", preloadTimeout)
	case errors.Is(err, context.Canceled):
		// The server is shutting down. Nothing to report about images.
	case err != nil:
		log.Printf("preloading images finished with errors after %s: %v", time.Since(started).Round(time.Second), err)
	default:
		log.Printf("preloaded images in %s", time.Since(started).Round(time.Second))
	}
}
