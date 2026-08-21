package buildkitagent_test

import (
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pool-agent/buildkitagent"
)

func TestTheBuildIdentityNeverReachesTheContainer(t *testing.T) {
	const sandbox = "sbx_abc123"
	injected := buildkitagent.BuildProxyURL(sandbox)

	// The mediator sets the identity on every proxy spelling, so the wrapper
	// has to clean every one of them. Cleaning only HTTP_PROXY left the owning
	// sandbox's ID sitting in HTTPS_PROXY, where any RUN step's `env` shows it
	// and any layer can bake it in.
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		got := buildkitagent.StripProxyEnv(name, injected)
		if strings.Contains(got, sandbox) {
			t.Errorf("%s still carries the owning sandbox: %q", name, got)
		}
		if buildkitagent.SandboxFromProxyURL(got) != "" {
			t.Errorf("%s is still readable as an identity: %q", name, got)
		}
		if !strings.HasSuffix(got, buildkitagent.BuildProxyAddress()) {
			t.Errorf("%s no longer names the forwarder, so the build loses egress: %q", name, got)
		}
	}
}

func TestUnrelatedEnvIsLeftAlone(t *testing.T) {
	// The wrapper edits every container's spec, so a rule that is too eager
	// rewrites variables that have nothing to do with the build's proxy.
	for _, name := range []string{"PATH", "NO_PROXY", "no_proxy", "PROXY_MODE"} {
		if got := buildkitagent.StripProxyEnv(name, "keep/me:1"); got != "keep/me:1" {
			t.Errorf("%s was rewritten to %q", name, got)
		}
	}
}

func TestAUserSetProxyIsNeverMistakenForAnIdentity(t *testing.T) {
	// A build may legitimately set its own proxy. Reading a sandbox out of one
	// would hand that build another tenant's certificate.
	for _, value := range []string{
		"http://corp-proxy:3128",
		"http://someone@corp-proxy:3128",
		"https://sbx_abc123@127.0.0.1:17009",
		"http://sbx_abc123@127.0.0.1:9999",
		"http://@127.0.0.1:17009",
		"",
	} {
		if got := buildkitagent.SandboxFromProxyURL(value); got != "" {
			t.Errorf("%q was read as an identity for %q", value, got)
		}
		if got := buildkitagent.StripProxyEnv("HTTP_PROXY", value); got != value {
			t.Errorf("%q was rewritten to %q", value, got)
		}
	}
}

func TestTheInjectedURLRoundTrips(t *testing.T) {
	// The mediator writes it and the wrapper reads it back; if these two ever
	// disagree the build silently loses its egress instead of failing.
	const sandbox = "sbx_abc123"
	if got := buildkitagent.SandboxFromProxyURL(buildkitagent.BuildProxyURL(sandbox)); got != sandbox {
		t.Errorf("the wrapper reads %q out of what the mediator wrote", got)
	}
}
