package harness

import "strings"

// BundleRegistry and BundleTag locate the bundles a build ships: the platform
// bundle and every built-in harness's (ADR 26-10-16-423 §§2, 7).
//
// The defaults name a purely local bundle — `task build:harness-bundles`
// writes exactly discobox-harness-<slug>:local — because a development build
// has no published bundle to reach for. A release overwrites both at link time
// with the registry and tag of the bundles built for that release, which is
// why they are vars and not consts, and is the same treatment
// dockerworker.DefaultPoolImage gets.
//
// One pair for every bundle rather than a full reference per harness: a
// release publishes them together, to one registry, under one tag, and
// independently-settable references could disagree about which release a
// sandbox is running.
//
// An unset registry leaves the name unqualified, which is what makes the
// development default resolve locally instead of against Docker Hub.
var (
	BundleRegistry = ""
	BundleTag      = "local"
)

// BundleRef returns the reference for a bundle this build ships given its
// unqualified name (e.g. discobox-harness-shell).
func BundleRef(name string) string {
	tag := strings.TrimSpace(BundleTag)
	if tag == "" {
		tag = "local"
	}
	if registry := strings.Trim(strings.TrimSpace(BundleRegistry), "/"); registry != "" {
		return registry + "/" + name + ":" + tag
	}
	return name + ":" + tag
}

// PlatformBundleRef returns the reference for the platform bundle this build
// pins (ADR 26-10-16-423 §7): published with the harness bundles, to the same
// registry under the same tag, so it is located as they are.
func PlatformBundleRef() string {
	return BundleRef("discobox-platform")
}
