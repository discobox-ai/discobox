package buildkitagent

import (
	"context"
	"errors"
	"fmt"
	"io"

	controlapi "github.com/moby/buildkit/api/services/control"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// PruneBuildCache empties buildkitd's build cache: every cache record, in use
// or not, the way `buildctl prune --all` does.
//
// It asks buildkitd rather than deleting StateRoot, because buildkitd holds its
// content store and snapshot metadata open for as long as it runs, and a tree
// removed underneath it leaves records naming snapshots that are gone. A prune
// is buildkitd's own operation and is safe while it serves.
//
// A build still running holds its records, and prune cannot release them; the
// caller stops the sandboxes that start builds before asking.
func PruneBuildCache(ctx context.Context) error {
	conn, err := grpc.NewClient("unix://"+resolve(Socket),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("dial buildkitd: %w", err)
	}
	defer conn.Close()
	stream, err := controlapi.NewControlClient(conn).Prune(ctx, &controlapi.PruneRequest{All: true})
	if err != nil {
		return fmt.Errorf("prune build cache: %w", err)
	}
	// The stream reports each record as it goes; the prune is done when it ends.
	for {
		if _, err := stream.Recv(); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("prune build cache: %w", err)
		}
	}
}
