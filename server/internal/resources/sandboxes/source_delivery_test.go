package sandboxes

import (
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
			sandbox: &model.Sandbox{Source: pushSource()},
			want:    true,
			why:     "the workspace is empty until the client pushes",
		},
		{
			name:    "push source reported delivered proceeds",
			sandbox: &model.Sandbox{Source: pushSource(), SourceDeliveredAt: &delivered},
			want:    false,
			why:     "the client reported the push complete",
		},
		{
			name:    "clone source never waits",
			sandbox: &model.Sandbox{Source: &model.GitSource{Kind: "git", Delivery: model.GitSourceDeliveryClone}},
			want:    false,
			why:     "the sandbox fetches a clone-delivered source itself",
		},
		{
			name:    "source without delivery never waits",
			sandbox: &model.Sandbox{Source: &model.GitSource{Kind: "git"}},
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

// The deadline anchors the timeout, so it must be written once and never
// re-derived. Re-deriving it on each reconcile would push it out every time the
// sandbox was looked at, and it could never expire.
func TestSourceAwaitDeadlineIsSetOnceAndExpires(t *testing.T) {
	source := func() *model.GitSource {
		return &model.GitSource{Kind: "git", Delivery: model.GitSourceDeliveryPush}
	}

	t.Run("first park sets a future deadline", func(t *testing.T) {
		sb := &model.Sandbox{Source: source()}
		before := time.Now().UTC()
		if err := parkForSourcePush(sb); err != nil {
			t.Fatalf("park: %v", err)
		}
		if sb.SourceAwaitDeadline == nil {
			t.Fatal("no deadline was set; the sandbox would wait forever")
		}
		if !sb.SourceAwaitDeadline.After(before) {
			t.Fatalf("deadline %v is not in the future", sb.SourceAwaitDeadline)
		}
	})

	t.Run("re-parking keeps the original deadline", func(t *testing.T) {
		original := time.Now().UTC().Add(time.Minute)
		sb := &model.Sandbox{Source: source(), SourceAwaitDeadline: &original}
		if err := parkForSourcePush(sb); err != nil {
			t.Fatalf("park: %v", err)
		}
		if !sb.SourceAwaitDeadline.Equal(original) {
			t.Fatalf("deadline moved to %v, want it pinned at %v", sb.SourceAwaitDeadline, original)
		}
	})

	t.Run("a passed deadline fails instead of parking again", func(t *testing.T) {
		passed := time.Now().UTC().Add(-time.Second)
		sb := &model.Sandbox{Source: source(), SourceAwaitDeadline: &passed}
		err := parkForSourcePush(sb)
		if err == nil {
			t.Fatal("expired wait: got nil error, want a timeout failure")
		}
		if sb.Phase == model.SandboxPhaseAwaitingSource {
			t.Fatal("sandbox parked again after its deadline passed")
		}
	})
}
