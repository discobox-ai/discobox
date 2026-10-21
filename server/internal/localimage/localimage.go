// Package localimage says what the server pins an image its own Docker daemon
// holds to. Harness registration pins a bundle by it, and the asset API finds
// that pin's content on the daemon by it, so the two cannot drift apart.
package localimage

import (
	"strings"

	dockerclient "github.com/moby/moby/client"
)

// Digest is the digest to pin a locally-inspected image to.
//
// A pulled image carries the registry digest in RepoDigests, and that is what
// every daemon can be asked about later, whichever image store it uses. A
// locally built one has none — nothing pushed it — so its image ID is all there
// is, and it is also all the pool will have to compare against. A containerd
// image store makes that ID the image's manifest digest; the classic store
// makes it its config's.
func Digest(inspected dockerclient.ImageInspectResult) string {
	for _, repoDigest := range inspected.RepoDigests {
		if _, digest, ok := strings.Cut(repoDigest, "@"); ok && digest != "" {
			return digest
		}
	}
	return inspected.ID
}

// Is reports whether the daemon's image is the one digest pins: its image ID,
// or a registry digest it was pulled under.
func Is(inspected dockerclient.ImageInspectResult, digest string) bool {
	if inspected.ID == digest {
		return true
	}
	for _, repoDigest := range inspected.RepoDigests {
		if strings.HasSuffix(repoDigest, "@"+digest) {
			return true
		}
	}
	return false
}
