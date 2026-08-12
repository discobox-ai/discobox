package sandboxes

import (
	"strings"
	"testing"
	"time"

	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/sandbox"
)

func TestSourceNeedsPush(t *testing.T) {
	const serverHost = "host_serveraaaaaaaa"
	const clientHost = "host_clientbbbbbbbb"

	localSource := func() *model.GitSource {
		dir := "/src/alpha"
		return &model.GitSource{Kind: "git", LocalDirectory: &dir}
	}
	remoteSource := func() *model.GitSource {
		url := "https://github.com/obot-platform/discobox.git"
		return &model.GitSource{Kind: "git", URL: &url}
	}
	binds := sandbox.ProviderDefinition{LocalSourceBind: true}
	remoteProvider := sandbox.ProviderDefinition{LocalSourceBind: false}
	sameHost := &model.Origin{HostID: serverHost, ProjectPath: "/src/alpha"}
	otherHost := &model.Origin{HostID: clientHost, ProjectPath: "/src/alpha"}

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
