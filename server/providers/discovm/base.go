package discovm

import (
	"strings"

	"github.com/discobox-ai/vm/pkg/machine"
)

// DefaultOCIBase is the base discobox sets on every linked driver whose base
// is an OCI image (disco-vm's Definition.Base.Format, machine.OCIImage:
// archil; disco-vm's docker driver declares it too, but is not linked),
// unless the provider's configuration names another (disco-vm ADR
// 26-11-20-547 §2). It is archil's default base, discobox's own image with
// systemd, systemd-sysv, dbus and nftables (ADR 26-11-16-619 §4): the sandbox
// base image a release publishes (server/providers/dockerworker/sandbox-image),
// where Archil's own default image has no systemd.
//
// A digest, for the reason the pool VM guest is one
// (guestimage.DefaultVMImage): a tag would let whoever pushes it decide what
// every pool's machines boot. The provider pulls it with none of the server's
// credentials (disco-vm ADR 26-11-20-547 §3), so it is a public image. A
// release cannot link the digest of the image it builds alongside its
// binaries, so this names an earlier release's: `go tool task oci-base:pin`
// moves it after a release publishes its image (the release skill's step),
// and nothing refuses a release whose pin was not moved.
const DefaultOCIBase = "ghcr.io/discobox-ai/discobox-sandbox-base:v0.19.5@sha256:7826f163cbf17a74ef4023c1f77f4c2bd854add785571def11bae3ad3e597228"

// base is what the driver makes a fresh machine from (machine.Config.Base):
// the provider's base, or, when it names none and the driver's base is an OCI
// image, DefaultOCIBase. Any other driver's empty base stays empty, which is
// the driver's own default. It reads the format the driver declares, never
// its name (disco-vm ADR 26-11-20-547, Alternatives).
func (s settings) base() string {
	if base := strings.TrimSpace(s.Base); base != "" {
		return base
	}
	if takesOCIBase(s.name) {
		return DefaultOCIBase
	}
	return ""
}

// takesOCIBase is whether the named driver's base is an OCI image reference.
func takesOCIBase(name string) bool {
	def, err := machine.DefinitionOf(name)
	return err == nil && def.Base != nil && def.Base.Format == machine.OCIImage
}
