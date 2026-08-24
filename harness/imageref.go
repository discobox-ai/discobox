package harness

import "strings"

// ImageRegistry and ImageTag locate the built-in harness images.
//
// The defaults name a purely local image — `task build:harness-images` writes
// exactly discobox-harness-<slug>:local — because a development build has no
// published image to reach for. A release overwrites both at link time with the
// registry and tag of the harness images built for that release, which is why
// they are vars and not consts, and is the same treatment
// dockerworker.DefaultPoolImage and sandbox.DefaultSandboxImageName get.
//
// One pair for all three images rather than a full reference per harness: a
// release publishes them together, to one registry, under one tag, and three
// independently-settable references could disagree about which release a
// sandbox is running.
//
// An unset registry leaves the name unqualified, which is what makes the
// development default resolve against the local daemon instead of Docker Hub.
var (
	ImageRegistry = ""
	ImageTag      = "local"
)

// ImageRef returns the reference for a built-in harness image given its
// unqualified name (e.g. discobox-harness-shell).
func ImageRef(name string) string {
	tag := strings.TrimSpace(ImageTag)
	if tag == "" {
		tag = "local"
	}
	if registry := strings.Trim(strings.TrimSpace(ImageRegistry), "/"); registry != "" {
		return registry + "/" + name + ":" + tag
	}
	return name + ":" + tag
}
