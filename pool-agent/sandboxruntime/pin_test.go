package sandboxruntime

import "testing"

// "The image ID" is not one thing. The classic image store reports the config
// digest; the containerd store, the default in current Docker, reports the
// index digest. A pin recorded as one and compared against the other never
// matches — which is how every sandbox on a published multi-arch image refused
// to launch against an image that was on the daemon and correct.
func TestPinMatchesTheRegistryDigestFromRepoDigests(t *testing.T) {
	const (
		indexDigest  = "sha256:4a572614b0379e034bfa2f905ad414248fbd8e71d2eeb1c75f08f0dfbb5019d1"
		configDigest = "sha256:6a50664615dc30e638e1a98b2974a76c2ad2566533e30cc48d3d14436436e5db"
	)
	repoDigests := []string{"ghcr.io/discobox-ai/discobox-harness-shell@" + indexDigest}

	// A containerd-backed daemon: the ID is the index digest.
	if !imageMatchesPinDigests(indexDigest, repoDigests, indexDigest) {
		t.Fatal("a containerd daemon's own ID did not match the pin")
	}
	// A classic daemon: the ID is the config digest, and RepoDigests carries
	// the registry digest the pin was recorded from.
	if !imageMatchesPinDigests(configDigest, repoDigests, indexDigest) {
		t.Fatal("the registry digest in RepoDigests did not match the pin")
	}
}

// A locally built image was never pushed, so it has no RepoDigests and its ID
// is all there is to compare against.
func TestPinMatchesALocallyBuiltImageByID(t *testing.T) {
	const id = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	if !imageMatchesPinDigests(id, nil, id) {
		t.Fatal("a local build did not match its own ID")
	}
}

// A genuinely different image still fails, or the pin means nothing.
func TestPinRejectsADifferentImage(t *testing.T) {
	const (
		pinned = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
		other  = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	)
	if imageMatchesPinDigests(other, []string{"example.com/x@" + other}, pinned) {
		t.Fatal("the pin accepted an image it does not name")
	}
}

// An empty pin is unpinned: sandboxes on the default image, and those created
// before pinning existed, run whatever their reference names.
func TestEmptyPinMatchesAnything(t *testing.T) {
	if !imageMatchesPinDigests("sha256:whatever", nil, "") {
		t.Fatal("an unpinned sandbox was refused")
	}
}
