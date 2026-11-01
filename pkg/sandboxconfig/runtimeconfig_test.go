package sandboxconfig

import (
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/platform"
	"github.com/discobox-ai/discobox/pkg/sandboxpath"
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
		}, "neither a clean absolute"},
		"home-relative target that climbs out of home": {func(c *RuntimeConfig) {
			c.Sources = []RuntimeSource{{Slug: "tools", Target: "~/../etc"}}
		}, "neither a clean absolute"},
		"unclean target": {func(c *RuntimeConfig) {
			c.Sources = []RuntimeSource{{Slug: "primary", Target: "/workspace/../etc"}}
		}, "neither a clean absolute"},
		"multi-line token": {func(c *RuntimeConfig) {
			c.Sources = []RuntimeSource{{Slug: "primary", Target: "/workspace", OriginToken: "a\nprotocol=http"}}
		}, "single line"},
		"proxy without a keypair": {func(c *RuntimeConfig) { c.Proxy = &RuntimeProxy{} }, "keypair"},
		"proxy CA that is not PEM": {func(c *RuntimeConfig) {
			c.Proxy = &RuntimeProxy{MTLSCA: "not a certificate"}
		}, "proxy.mtlsCa"},
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

// A skills source's target is home-relative until the sandbox expands it
// against its user's home (ADR 26-11-01-692 §3), and expanding leaves the
// document it was given as it was, since that is the one kept and compared.
func TestRuntimeConfigHomeRelativeTarget(t *testing.T) {
	doc := RuntimeConfig{Revision: 1, Sources: []RuntimeSource{
		{Slug: "primary", Target: "/workspace/app", OriginURL: "https://pool/origins/primary"},
		{Slug: "tools", Target: "~/.discobox/skill-sources/tools", OriginURL: "https://github.com/acme/tools"},
	}}
	linux := sandboxpath.For(platform.Platform{OS: "linux"})
	if err := doc.Validate(linux); err != nil {
		t.Fatalf("Validate() = %v, want a home-relative target accepted", err)
	}
	expanded := doc.ExpandHome(linux, "/home/u")
	if got := expanded.Sources[1].Target; got != "/home/u/.discobox/skill-sources/tools" {
		t.Fatalf("expanded target = %q", got)
	}
	if got := expanded.Sources[0].Target; got != "/workspace/app" {
		t.Fatalf("an absolute target changed to %q", got)
	}
	if got := doc.Sources[1].Target; got != "~/.discobox/skill-sources/tools" {
		t.Fatalf("ExpandHome changed the document it was given: %q", got)
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

func TestProviderAwaitsRuntimeConfigWhenItNamesAPoolKey(t *testing.T) {
	if (Provider{PublicKeys: map[string]string{ControlPlanePublicKeyName: "cp"}}).AwaitsRuntimeConfig() {
		t.Fatal("a bootstrap with no pool key awaits a runtime config")
	}
	if !(Provider{PublicKeys: map[string]string{PoolPublicKeyName: "pool"}}).AwaitsRuntimeConfig() {
		t.Fatal("a bootstrap naming a pool key does not await a runtime config")
	}
}

// Both sides keep a document without its private keys, and leave the one
// they were given whole.
func TestWithoutPrivateKeysTakesOutEveryPrivateKey(t *testing.T) {
	doc := RuntimeConfig{
		Revision: 1,
		Agent:    RuntimeAgent{TLS: &AgentTLS{Cert: "agent cert", Key: "agent key"}},
		Proxy:    &RuntimeProxy{ClientCert: "client cert", ClientKey: "client key"},
	}
	kept := doc.WithoutPrivateKeys()
	if kept.Proxy.ClientKey != "" || kept.Agent.TLS.Key != "" {
		t.Fatalf("kept = %+v / %+v, want no private key", kept.Proxy, kept.Agent.TLS)
	}
	if kept.Proxy.ClientCert != "client cert" || kept.Agent.TLS.Cert != "agent cert" {
		t.Fatal("taking the keys out took a certificate with them")
	}
	if doc.Proxy.ClientKey != "client key" || doc.Agent.TLS.Key != "agent key" {
		t.Fatal("taking the keys out changed the document it was given")
	}
}
