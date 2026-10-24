package model

import (
	"slices"
	"testing"
)

var (
	platformPin = BundlePin{Reference: "ghcr.io/discobox-ai/discobox-platform:v1", Digest: "sha256:1111111111111111111111111111111111111111111111111111111111111111"}
	harnessPin  = BundlePin{Reference: "ghcr.io/discobox-ai/discobox-harness-codex:v1", Digest: "sha256:2222222222222222222222222222222222222222222222222222222222222222"}
	movedPin    = BundlePin{Reference: "ghcr.io/discobox-ai/discobox-platform:v2", Digest: "sha256:3333333333333333333333333333333333333333333333333333333333333333"}
)

// A sandbox pinned before ADR 26-10-24-953 is read and re-pinned in its own
// columns, so nothing in its fingerprint changes form; any other is pinned as
// the ordered list, platform first, with those columns left empty.
func TestPinsKeepTheFormASandboxWasPinnedIn(t *testing.T) {
	before := SandboxManifest{PlatformBundle: platformPin.Reference, PlatformBundleDigest: platformPin.Digest, Bundle: harnessPin.Reference, BundleDigest: harnessPin.Digest}
	if before.PlatformPin() != platformPin || before.HarnessPin() != harnessPin || !slices.Equal(before.BundlePins(), []BundlePin{platformPin, harnessPin}) {
		t.Fatalf("pins read from the columns = %+v, %+v", before.PlatformPin(), before.HarnessPin())
	}
	before.SetPins(movedPin, harnessPin)
	if before.PlatformBundleDigest != movedPin.Digest || before.Bundles != nil {
		t.Fatalf("a re-pin moved the sandbox off its columns: %+v", before)
	}

	var listed SandboxManifest
	listed.SetPins(platformPin, harnessPin)
	if !slices.Equal(listed.Bundles, []BundlePin{platformPin, harnessPin}) || listed.PlatformBundle != "" || listed.Bundle != "" {
		t.Fatalf("a new pin = %+v, want the list alone", listed)
	}
	if listed.PlatformPin() != platformPin || listed.HarnessPin() != harnessPin {
		t.Fatalf("pins read from the list = %+v, %+v", listed.PlatformPin(), listed.HarnessPin())
	}
	listed.SetPins(movedPin, BundlePin{})
	if !slices.Equal(listed.Bundles, []BundlePin{movedPin}) || listed.HarnessPin() != (BundlePin{}) {
		t.Fatalf("a harness with no bundle = %+v, want the platform alone", listed.Bundles)
	}

	// A harness with no platform is no list: the list's first pin is the
	// platform's.
	var unresolved SandboxManifest
	unresolved.SetPins(BundlePin{}, harnessPin)
	if unresolved.Bundles != nil || unresolved.HarnessPin() != harnessPin || unresolved.PlatformPin() != (BundlePin{}) {
		t.Fatalf("a harness with no platform = %+v", unresolved)
	}
}

// The agent is in neither fingerprint: it is placed at every start, and what
// the box applies is its bundles.
func TestTheAgentIsInNoFingerprint(t *testing.T) {
	var m SandboxManifest
	m.SetPins(platformPin, harnessPin)
	full, delivered := m.Fingerprint(), m.DeliveredFingerprint()
	m.SetAgentPin(BundlePin{Reference: "ghcr.io/discobox-ai/discobox-agent:v1", Digest: movedPin.Digest})
	if m.Fingerprint() != full || m.DeliveredFingerprint() != delivered {
		t.Fatal("pinning the agent changed a fingerprint")
	}
}
