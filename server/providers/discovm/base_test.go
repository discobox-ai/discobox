package discovm

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// The pinned base is a public image by digest: a tag would let whoever
// pushes it choose what every pool's machines boot.
func TestDefaultOCIBaseIsPinnedByDigest(t *testing.T) {
	if !regexp.MustCompile(`^ghcr\.io/discobox-ai/discobox-sandbox-base:v[0-9][^@]*@sha256:[0-9a-f]{64}$`).MatchString(DefaultOCIBase) {
		t.Fatalf("DefaultOCIBase = %q, want discobox-sandbox-base at a release tag, by digest", DefaultOCIBase)
	}
}

// The image DefaultOCIBase pins is built from the sandbox base image's
// Dockerfile, and an archil machine needs systemd, systemd-sysv, dbus and
// nftables in it (ADR 26-11-16-619 §4), and sudo, which a sandbox machine's
// wiring runs (discobox-base v0.18.1, which had none, could not wire one).
// Each is named in its apt install, not
// left to another package's dependencies, so a later edit there cannot drop
// one unseen.
func TestTheOCIBaseInstallsWhatAnArchilMachineNeeds(t *testing.T) {
	data, err := os.ReadFile("../dockerworker/sandbox-image/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	var installed []string
	for line := range strings.Lines(string(data)) {
		if name, ok := strings.CutSuffix(strings.TrimSpace(line), " \\"); ok && regexp.MustCompile(`^[a-z0-9][a-z0-9.+-]*$`).MatchString(name) {
			installed = append(installed, name)
		}
	}
	for _, pkg := range []string{"systemd", "systemd-sysv", "dbus", "nftables", "sudo"} {
		if !slices.Contains(installed, pkg) {
			t.Errorf("the sandbox base image's Dockerfile installs no %s, which an archil machine booted from DefaultOCIBase needs", pkg)
		}
	}
}

// A driver whose base is an OCI image is given discobox's base when the
// provider names none, and the provider's when it names one; a driver whose
// base has no format keeps its own default (disco-vm ADR 26-11-20-547 §2).
func TestBaseIsDiscoboxsOnADriverThatTakesAnOCIImage(t *testing.T) {
	for _, tc := range []struct {
		driver, config, want string
	}{
		{"archil", `{"driver":{"apiKey":"k","region":"aws-us-east-1"}}`, DefaultOCIBase},
		{"archil", `{"driver":{"apiKey":"k","region":"aws-us-east-1"},"base":"  "}`, DefaultOCIBase},
		{"archil", `{"driver":{"apiKey":"k","region":"aws-us-east-1"},"base":" debian:sid "}`, "debian:sid"},
		{"boxd", `{"driver":{"apiKey":"k"}}`, ""},
		{"boxd", `{"driver":{"apiKey":"k"},"base":"img"}`, "img"},
		{"fake", `{}`, ""},
	} {
		s, err := decode(tc.driver, []byte(tc.config))
		if err != nil {
			t.Fatalf("decode(%s, %s): %v", tc.driver, tc.config, err)
		}
		if got := s.base(); got != tc.want {
			t.Errorf("base of %s %s = %q, want %q", tc.driver, tc.config, got, tc.want)
		}
	}
}

// Which drivers take an OCI image is disco-vm's to declare. Its pin moving
// is when a driver gains or loses the format, which this says.
func TestOCIBaseIsWhatTheDriverDeclares(t *testing.T) {
	for driver, want := range map[string]bool{"archil": true, "boxd": false, "fake": false} {
		if got := takesOCIBase(driver); got != want {
			t.Errorf("takesOCIBase(%s) = %v, want %v", driver, got, want)
		}
	}
}

// The catalog shows the base a driver of an OCI image takes when none is
// named, so a person sees what a pool's machines run.
func TestCatalogShowsTheOCIBaseAsTheDefault(t *testing.T) {
	isolateStateRoot(t)
	field := func(driver string) sandbox.ProviderConfigField {
		for _, f := range Definition(driver).ConfigFields {
			if f.Key == "base" {
				return f
			}
		}
		t.Fatalf("Definition(%s) has no base", driver)
		return sandbox.ProviderConfigField{}
	}
	if f := field("archil"); f.Default != DefaultOCIBase {
		t.Errorf("archil's base defaults to %q, want %q", f.Default, DefaultOCIBase)
	}
	if f := field("boxd"); f.Default == DefaultOCIBase {
		t.Errorf("boxd's base defaults to %q, which is no boxd image", f.Default)
	}
}

// The pool's machine is made from the base the driver is, and its bootstrap
// names it, so the pool agent makes its sandboxes' machines from it too.
func TestPoolMachineTakesTheOCIBase(t *testing.T) {
	r, _ := newFakePool(t, "")
	for _, tc := range []struct {
		cfg  settings
		want string
	}{
		{settings{name: "archil"}, DefaultOCIBase},
		{settings{name: "archil", Config: Config{Base: "debian:sid"}}, "debian:sid"},
		{settings{name: "boxd"}, ""},
	} {
		if got := newPoolMachine(r.engine, tc.cfg, nil, nil).base; got != tc.want {
			t.Errorf("pool machine of %s with base %q is made from %q, want %q", tc.cfg.name, tc.cfg.Base, got, tc.want)
		}
	}
}
