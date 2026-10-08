package sandboxconfig

import (
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/platform"
	"github.com/discobox-ai/discobox/sandboxpath"
)

func TestRuntimeConfigValidate(t *testing.T) {
	valid := RuntimeConfig{
		Revision:  1,
		Agent:     RuntimeAgent{IdleTimeout: "30m"},
		SecretEnv: map[string]string{"GH_TOKEN": "sentinel"},
		Sources:   []RuntimeSource{{Slug: "primary", Target: "/workspace", OriginURL: "https://pool/origins/primary", OriginToken: "token"}},
	}
	if err := valid.Validate(sandboxpath.Paths{}); err != nil {
		t.Fatalf("valid document: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate func(*RuntimeConfig)
		want   string
	}{
		"no revision":       {func(c *RuntimeConfig) { c.Revision = 0 }, "revision"},
		"bad idle timeout":  {func(c *RuntimeConfig) { c.Agent.IdleTimeout = "soon" }, "idleTimeout"},
		"zero idle timeout": {func(c *RuntimeConfig) { c.Agent.IdleTimeout = "0s" }, "idleTimeout"},
		"bad secret name":   {func(c *RuntimeConfig) { c.SecretEnv = map[string]string{"A=B": "x"} }, "secretEnv"},
		"unnamed source":    {func(c *RuntimeConfig) { c.Sources = []RuntimeSource{{}} }, "no slug"},
		"duplicate source": {func(c *RuntimeConfig) {
			c.Sources = []RuntimeSource{{Slug: "primary"}, {Slug: "primary"}}
		}, "named twice"},
		"origin without a target": {func(c *RuntimeConfig) {
			c.Sources = []RuntimeSource{{Slug: "primary", OriginURL: "https://pool/origins/primary"}}
		}, "no target"},
		"relative target": {func(c *RuntimeConfig) {
			c.Sources = []RuntimeSource{{Slug: "primary", Target: "workspace"}}
		}, "is not a clean absolute"},
		"unclean target": {func(c *RuntimeConfig) {
			c.Sources = []RuntimeSource{{Slug: "primary", Target: "/workspace/../etc"}}
		}, "is not a clean absolute"},
		"multi-line token": {func(c *RuntimeConfig) {
			c.Sources = []RuntimeSource{{Slug: "primary", Target: "/workspace", OriginToken: "a\nprotocol=http"}}
		}, "single line"},
		"proxy without a keypair": {func(c *RuntimeConfig) { c.Proxy = &RuntimeProxy{} }, "keypair"},
		"proxy CA that is not PEM": {func(c *RuntimeConfig) {
			c.Proxy = &RuntimeProxy{MTLSCA: "not a certificate"}
		}, "proxy.mtlsCa"},
		"bad registry namespace": {func(c *RuntimeConfig) {
			c.Proxy = &RuntimeProxy{RegistryNamespace: "Has/Slash"}
		}, "registryNamespace"},
		"relative bridge upstream": {func(c *RuntimeConfig) {
			c.Proxy = &RuntimeProxy{Egress: &RuntimeBridge{UpstreamURL: "pool:17443"}}
		}, "proxy.egress.upstreamUrl"},
	} {
		t.Run(name, func(t *testing.T) {
			doc := valid
			tc.mutate(&doc)
			err := doc.Validate(sandboxpath.Paths{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want an error naming %q", err, tc.want)
			}
		})
	}
}

func TestRuntimeConfigSourcesDelivered(t *testing.T) {
	if !(RuntimeConfig{}).SourcesDelivered() {
		t.Fatal("a document with no source has nothing to wait for")
	}
	partial := RuntimeConfig{Sources: []RuntimeSource{{Slug: "a", Delivered: true}, {Slug: "b"}}}
	if partial.SourcesDelivered() {
		t.Fatal("a partial delivery counts as delivered")
	}
	partial.Sources[1].Delivered = true
	if !partial.SourcesDelivered() {
		t.Fatal("every source delivered does not count as delivered")
	}
}

func TestRuntimeConfigSameDocument(t *testing.T) {
	a := RuntimeConfig{Revision: 1, SecretEnv: map[string]string{}}
	b := RuntimeConfig{Revision: 1}
	if !a.SameDocument(b) {
		t.Fatal("an empty and an absent secret map read the same, so they are the same document")
	}
	b.SecretEnv = map[string]string{"A": "x"}
	if a.SameDocument(b) {
		t.Fatal("documents that differ compare the same")
	}
}

// A source target is judged by the sandbox's own platform (ADR 0145 §6): a
// drive path is a target on Windows and not on Linux, and the reverse.
func TestRuntimeConfigTargetsAreTheSandboxPlatforms(t *testing.T) {
	windows := sandboxpath.For(platform.Platform{OS: "windows", Arch: "amd64"})
	linux := sandboxpath.For(platform.Platform{OS: "linux", Arch: "amd64"})
	doc := func(target string) RuntimeConfig {
		return RuntimeConfig{Revision: 1, Sources: []RuntimeSource{{Slug: "primary", Target: target, OriginURL: "https://pool/o"}}}
	}
	for _, tc := range []struct {
		paths  sandboxpath.Paths
		target string
		ok     bool
	}{
		{windows, `C:\workspace\app`, true},
		{windows, "/workspace/app", false},
		{windows, `C:\workspace\..\app`, false},
		{linux, "/workspace/app", true},
		{linux, `C:\workspace\app`, false},
	} {
		err := doc(tc.target).Validate(tc.paths)
		if (err == nil) != tc.ok {
			t.Errorf("Validate(%s) of target %q = %v, want ok=%v", tc.paths.OS(), tc.target, err, tc.ok)
		}
	}
}
