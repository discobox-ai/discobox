package pools

import (
	"errors"
	"net/http"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/platform"
)

// A pool agent declares the one platform its pool hosts (ADR 0145 §1). One
// that is not an os/arch pair is refused; none at all is an agent from before
// platforms, which must still be able to report, and reads as the zero
// platform the store resolves (store.recordPoolPlatform).
func TestAPoolAgentDeclaresItsPlatform(t *testing.T) {
	got, err := declaredPlatform(serverapi.NewOptString("linux/arm64"))
	if err != nil || got != (platform.Platform{OS: "linux", Arch: "arm64"}) {
		t.Fatalf("declaredPlatform = %+v, %v", got, err)
	}
	if got, err := declaredPlatform(serverapi.OptString{}); err != nil || !got.IsZero() {
		t.Fatalf("an agent from before platforms: %+v, %v; want the zero platform and no error", got, err)
	}
	for _, declared := range []string{"linux", "linux/arm64/v8"} {
		_, err := declaredPlatform(serverapi.NewOptString(declared))
		var status interface{ StatusCode() int }
		if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest {
			t.Errorf("declaredPlatform(%q) = %v, want a 400", declared, err)
		}
	}
}

// A pool agent declares the kind of image its pool runs (ADR 26-10-09-106 §4).
// One that is neither OCI nor a disco-vm driver's is refused; none at all is
// an agent from before kinds, which reads as the zero kind the store records
// as OCI (store.recordPoolImageKind).
func TestAPoolAgentDeclaresItsImageKind(t *testing.T) {
	got, err := declaredImageKind(serverapi.NewOptString("discovm/boxd"))
	if err != nil || got != platform.DiscoVM("boxd") {
		t.Fatalf("declaredImageKind = %+v, %v", got, err)
	}
	if got, err := declaredImageKind(serverapi.OptString{}); err != nil || !got.IsZero() {
		t.Fatalf("an agent from before kinds: %+v, %v; want the zero kind and no error", got, err)
	}
	for _, declared := range []string{"docker", "discovm"} {
		_, err := declaredImageKind(serverapi.NewOptString(declared))
		var status interface{ StatusCode() int }
		if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest {
			t.Errorf("declaredImageKind(%q) = %v, want a 400", declared, err)
		}
	}
}
