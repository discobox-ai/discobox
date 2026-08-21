package sandboxes

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
)

// hostPath spells a POSIX fixture path the way the machine running the test
// does. sourceNeedsPush compares a source's directory against the provider's
// local source roots with filepath containment, which is a question about the
// host the server runs on — and on Windows "/src/alpha" is not even absolute,
// so every containment answer flips.
func hostPath(p string) string {
	if runtime.GOOS != "windows" {
		return p
	}
	if p == "/" {
		return `C:\`
	}
	return `C:` + filepath.FromSlash(p)
}

func TestSourceNeedsPush(t *testing.T) {
	const serverHost = "host_serveraaaaaaaa"
	const clientHost = "host_clientbbbbbbbb"

	localSource := func() *model.GitSource {
		dir := hostPath("/src/alpha")
		return &model.GitSource{Kind: "git", LocalDirectory: &dir}
	}
	directorySource := func() *model.GitSource {
		source := localSource()
		source.NoLocalRepository = true
		return source
	}
	remoteSource := func() *model.GitSource {
		url := "https://github.com/discobox-ai/discobox.git"
		return &model.GitSource{Kind: "git", URL: &url}
	}
	binds := sandbox.ProviderDefinition{LocalSourceRoots: []string{hostPath("/src")}}
	remoteProvider := sandbox.ProviderDefinition{}
	// A provider that reaches this filesystem, but not where the source lives.
	elsewhere := sandbox.ProviderDefinition{LocalSourceRoots: []string{hostPath("/home"), hostPath("/Users")}}
	everything := sandbox.ProviderDefinition{LocalSourceRoots: []string{hostPath("/")}}
	sameHost := &model.Origin{HostID: serverHost, ProjectPath: hostPath("/src/alpha")}
	otherHost := &model.Origin{HostID: clientHost, ProjectPath: hostPath("/src/alpha")}

	tests := []struct {
		name       string
		definition sandbox.ProviderDefinition
		serverHost string
		origin     *model.Origin
		source     *model.GitSource
		want       bool
		why        string
	}{
		{
			name: "local source on the same host binds", definition: binds, serverHost: serverHost,
			origin: sameHost, source: localSource(), want: false,
			why: "the provider runs here and the client is here, so the files resolve",
		},
		{
			name: "a directory with no repository pushes even from this host", definition: binds, serverHost: serverHost,
			origin: sameHost, source: directorySource(), want: true,
			why: "the path resolves here, but holds no repository to clone from",
		},
		{
			name: "local source from another host pushes", definition: binds, serverHost: serverHost,
			origin: otherHost, source: localSource(), want: true,
			why: "the provider can bind, but not to a path that only exists on the client",
		},
		{
			name: "local source on a non-binding provider pushes", definition: remoteProvider, serverHost: serverHost,
			origin: sameHost, source: localSource(), want: true,
			why: "the client is here, but the provider runs sandboxes elsewhere",
		},
		{
			name: "local source outside the provider's roots pushes", definition: elsewhere, serverHost: serverHost,
			origin: sameHost, source: localSource(), want: true,
			why: "the provider runs here, but carries no host mount the directory sits under",
		},
		{
			name: "a provider that shares the whole filesystem binds", definition: everything, serverHost: serverHost,
			origin: sameHost, source: localSource(), want: false,
			why: "a root of / covers every path on this machine",
		},
		{
			name: "a root is not a prefix match on the name", definition: sandbox.ProviderDefinition{LocalSourceRoots: []string{"/src-old"}},
			serverHost: serverHost, origin: sameHost, source: localSource(), want: true,
			why: "/src-old does not contain /src/alpha, however alike the two spell",
		},
		{
			name: "the root itself is covered", definition: sandbox.ProviderDefinition{LocalSourceRoots: []string{"/src/alpha"}},
			serverHost: serverHost, origin: sameHost, source: localSource(), want: false,
			why: "a directory mounted exactly is reachable at itself",
		},
		{
			name: "a relative root covers nothing", definition: sandbox.ProviderDefinition{LocalSourceRoots: []string{"src"}},
			serverHost: serverHost, origin: sameHost, source: localSource(), want: true,
			why: "a relative path names no place on this machine",
		},
		{
			name: "remote source never pushes", definition: remoteProvider, serverHost: serverHost,
			origin: otherHost, source: remoteSource(), want: false,
			why: "the sandbox clones the URL itself; no client is involved",
		},
		{
			name: "remote source on a binding provider never pushes", definition: binds, serverHost: serverHost,
			origin: sameHost, source: remoteSource(), want: false,
			why: "a reachable provider does not make a URL a local directory",
		},
		{
			name: "no source never pushes", definition: binds, serverHost: serverHost,
			origin: sameHost, source: nil, want: false,
			why: "there is nothing to deliver",
		},
		{
			name: "missing origin pushes", definition: binds, serverHost: serverHost,
			origin: nil, source: localSource(), want: true,
			why: "a client that reported no origin cannot be proven to be on this machine",
		},
		{
			name: "empty origin host pushes", definition: binds, serverHost: serverHost,
			origin: &model.Origin{ProjectPath: "/src/alpha"}, source: localSource(), want: true,
			why: "an origin without a host identifies no machine",
		},
		{
			name: "unknown server host pushes", definition: binds, serverHost: "",
			origin: sameHost, source: localSource(), want: true,
			why: "a server that cannot identify itself cannot claim the client's files",
		},
		{
			name: "empty local directory is not a local source", definition: remoteProvider, serverHost: serverHost,
			origin: otherHost, source: &model.GitSource{Kind: "git", LocalDirectory: new(string)}, want: false,
			why: "an empty path is not a directory to deliver",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sourceNeedsPush(tc.definition, tc.serverHost, tc.origin, tc.source); got != tc.want {
				t.Fatalf("sourceNeedsPush = %t, want %t: %s", got, tc.want, tc.why)
			}
		})
	}
}

func TestAwaitingSourcePush(t *testing.T) {
	pushSource := func() *model.GitSource {
		return &model.GitSource{Kind: "git", Delivery: model.GitSourceDeliveryPush}
	}
	delivered := time.Now().UTC()

	tests := []struct {
		name    string
		sandbox *model.Sandbox
		want    bool
		why     string
	}{
		{
			name:    "push source with no delivered commit waits",
			sandbox: &model.Sandbox{SandboxManifest: model.SandboxManifest{Source: pushSource()}},
			want:    true,
			why:     "the workspace is empty until the client pushes",
		},
		{
			name:    "push source reported delivered proceeds",
			sandbox: &model.Sandbox{SourceDeliveredAt: &delivered, SandboxManifest: model.SandboxManifest{Source: pushSource()}},
			want:    false,
			why:     "the client reported the push complete",
		},
		{
			name:    "clone source never waits",
			sandbox: &model.Sandbox{SandboxManifest: model.SandboxManifest{Source: &model.GitSource{Kind: "git", Delivery: model.GitSourceDeliveryClone}}},
			want:    false,
			why:     "the sandbox fetches a clone-delivered source itself",
		},
		{
			name:    "source without delivery never waits",
			sandbox: &model.Sandbox{SandboxManifest: model.SandboxManifest{Source: &model.GitSource{Kind: "git"}}},
			want:    false,
			why:     "delivery defaults to clone; absence must not imply a push",
		},
		{
			name:    "sandbox without a source never waits",
			sandbox: &model.Sandbox{},
			want:    false,
			why:     "there is nothing to deliver",
		},
		{
			name: "a push-delivered reference waits even behind a bound primary source",
			sandbox: &model.Sandbox{SandboxManifest: model.SandboxManifest{
				Source: &model.GitSource{Kind: "git", Delivery: model.GitSourceDeliveryClone},
				SourceCodeReferences: model.SourceCodeReferences{
					"/src/foo": *pushSource(),
				},
			}},
			want: true,
			why:  "starting now would run the harness against a workspace missing that source",
		},
		{
			name: "clone-delivered references never wait",
			sandbox: &model.Sandbox{SandboxManifest: model.SandboxManifest{
				Source: &model.GitSource{Kind: "git", Delivery: model.GitSourceDeliveryClone},
				SourceCodeReferences: model.SourceCodeReferences{
					"/src/foo": model.GitSource{Kind: "git", Delivery: model.GitSourceDeliveryClone},
				},
			}},
			want: false,
			why:  "the sandbox fetches every one of them itself",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := awaitingSourcePush(tc.sandbox); got != tc.want {
				t.Fatalf("awaitingSourcePush = %t, want %t: %s", got, tc.want, tc.why)
			}
		})
	}
}

// The deadline is derived from StateChangedAt, so the anchor must be stamped
// once and left alone. Restamping it on each reconcile would push the deadline
// out every time the sandbox was looked at, and it could never expire.
func TestParkForSourcePushAnchorsAndExpires(t *testing.T) {
	source := func() *model.GitSource {
		return &model.GitSource{Kind: "git", Delivery: model.GitSourceDeliveryPush}
	}

	t.Run("first park stamps the anchor and sets a future deadline", func(t *testing.T) {
		sb := &model.Sandbox{SandboxManifest: model.SandboxManifest{Source: source()}}
		sb.SetState(model.SandboxStatePending)
		before := time.Now().UTC()
		if err := parkForSourcePush(sb); err != nil {
			t.Fatalf("park: %v", err)
		}
		if sb.State != model.SandboxStateAwaitingSource {
			t.Fatalf("phase = %q, want awaiting_source", sb.State)
		}
		if sb.StateChangedAt.Before(before) {
			t.Fatalf("anchor %v predates the park", sb.StateChangedAt)
		}
		if !sourceAwaitDeadline(sb).After(before) {
			t.Fatalf("deadline %v is not in the future", sourceAwaitDeadline(sb))
		}
	})

	t.Run("re-parking keeps the original anchor", func(t *testing.T) {
		sb := &model.Sandbox{SandboxManifest: model.SandboxManifest{Source: source()}}
		sb.SetState(model.SandboxStateAwaitingSource)
		anchor := sb.StateChangedAt
		deadline := sourceAwaitDeadline(sb)

		// Whatever else reconciles this sandbox, the clock must not restart.
		for range 3 {
			if err := parkForSourcePush(sb); err != nil {
				t.Fatalf("re-park: %v", err)
			}
		}
		if !sb.StateChangedAt.Equal(anchor) {
			t.Fatalf("anchor moved to %v, want it pinned at %v", sb.StateChangedAt, anchor)
		}
		if !sourceAwaitDeadline(sb).Equal(deadline) {
			t.Fatalf("deadline moved to %v, want %v", sourceAwaitDeadline(sb), deadline)
		}
	})

	t.Run("a passed deadline fails instead of parking again", func(t *testing.T) {
		sb := &model.Sandbox{SandboxManifest: model.SandboxManifest{Source: source()}}
		sb.SetState(model.SandboxStateAwaitingSource)
		// Parked longer ago than the timeout allows.
		sb.StateChangedAt = time.Now().UTC().Add(-sourcePushTimeout - time.Second)

		err := parkForSourcePush(sb)
		if err == nil {
			t.Fatal("expired wait: got nil error, want a timeout failure")
		}
		if !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("error = %v, want a timeout failure", err)
		}
	})

	t.Run("a sandbox still inside its deadline keeps waiting", func(t *testing.T) {
		sb := &model.Sandbox{SandboxManifest: model.SandboxManifest{Source: source()}}
		sb.SetState(model.SandboxStateAwaitingSource)
		sb.StateChangedAt = time.Now().UTC().Add(-sourcePushTimeout / 2)

		if err := parkForSourcePush(sb); err != nil {
			t.Fatalf("park within the deadline: %v", err)
		}
		if sb.State != model.SandboxStateAwaitingSource {
			t.Fatalf("phase = %q, want it still awaiting_source", sb.State)
		}
	})
}

// The client reports every push-delivered source at once, so a report that
// covers only some of them must not resume the sandbox: it would start against
// a workspace missing whatever was left out.
func TestVerifySourcePushCommits(t *testing.T) {
	const primaryCommit = "0123456789abcdef0123456789abcdef01234567"
	const referenceCommit = "fedcba9876543210fedcba9876543210fedcba98"
	slug := func(value string) *string { return &value }

	pending := func() []gitSourceEntry {
		return pushDeliveredSources(&model.Sandbox{SandboxManifest: model.SandboxManifest{
			Source: &model.GitSource{
				Kind: "git", Slug: slug("primary"), Delivery: model.GitSourceDeliveryPush,
				Checkout: &model.GitSourceCheckout{Commit: slug(primaryCommit)},
			},
			SourceCodeReferences: model.SourceCodeReferences{
				"/src/foo": {
					Kind: "git", Slug: slug("foo"), Delivery: model.GitSourceDeliveryPush,
					Checkout: &model.GitSourceCheckout{Commit: slug(referenceCommit)},
				},
			},
		}})
	}

	if err := verifySourcePushCommits(pending(), map[string]string{
		"primary": strings.ToUpper(primaryCommit),
		"foo":     referenceCommit,
	}); err != nil {
		t.Fatalf("a complete report was rejected: %v", err)
	}
	if err := verifySourcePushCommits(pending(), map[string]string{"primary": primaryCommit}); err == nil {
		t.Fatal("a report missing a source was accepted")
	}
	if err := verifySourcePushCommits(pending(), map[string]string{
		"primary": primaryCommit,
		"foo":     primaryCommit,
	}); err == nil {
		t.Fatal("a source reported at another source's commit was accepted")
	}
	if err := verifySourcePushCommits(pending(), map[string]string{
		"primary": primaryCommit,
		"foo":     referenceCommit,
		"bar":     referenceCommit,
	}); err == nil {
		t.Fatal("a report naming a source the sandbox does not push was accepted")
	}
}
