package pools

import (
	"errors"
	"net/http"
	"testing"

	"github.com/discobox-ai/discobox/platform"
)

// A pool hosts exactly one platform, and its agent must say which: a
// registration or heartbeat that names none, or names something that is not an
// os/arch pair, is refused rather than recorded as a pool that hosts nothing
// (ADR 0145 §1).
func TestAPoolAgentMustDeclareItsPlatform(t *testing.T) {
	got, err := declaredPlatform("linux/arm64")
	if err != nil || got != (platform.Platform{OS: "linux", Arch: "arm64"}) {
		t.Fatalf("declaredPlatform = %+v, %v", got, err)
	}
	for _, declared := range []string{"", "linux", "linux/arm64/v8"} {
		_, err := declaredPlatform(declared)
		var status interface{ StatusCode() int }
		if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest {
			t.Errorf("declaredPlatform(%q) = %v, want a 400", declared, err)
		}
	}
}
