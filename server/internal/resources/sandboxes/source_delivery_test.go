package sandboxes

import (
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/internal/originkey"
	"github.com/discobox-ai/discobox/pkg/platform"
	"github.com/discobox-ai/discobox/server/internal/model"
)

func TestSourceDataKeyUsesHostAndNormalizedSourceRoot(t *testing.T) {
	directory := "/foo/bar/baz"
	source := &model.GitSource{Kind: "git", LocalDirectory: &directory}
	if got, want := sourceDataKey("host_abc", source), originkey.Of("host_abc", directory); got != want {
		t.Fatalf("sourceDataKey = %q, want %q", got, want)
	}
	if sourceDataKey("host_abc", source) == sourceDataKey("host_other", source) {
		t.Fatal("the same path on different client hosts shares source data")
	}
	if got := sourceDataKey("", source); got != "" {
		t.Fatalf("sourceDataKey without host = %q, want empty", got)
	}
	if got := sourceDataKey("host_abc", nil); got != "" {
		t.Fatalf("sourceDataKey without source = %q, want empty", got)
	}
}

func TestSourceDataFingerprintIsStableAndChangesTheRuntimeSpec(t *testing.T) {
	refsA := map[string]string{"z": strings.Repeat("a", 64), "a": strings.Repeat("b", 64)}
	refsB := map[string]string{"a": strings.Repeat("b", 64), "z": strings.Repeat("a", 64)}
	first := sourceDataFingerprint("base", strings.Repeat("c", 64), refsA)
	if second := sourceDataFingerprint("base", strings.Repeat("c", 64), refsB); first != second {
		t.Fatalf("map iteration changed fingerprint: %q != %q", first, second)
	}
	if first == "base" || first == sourceDataFingerprint("base", strings.Repeat("d", 64), refsA) {
		t.Fatal("source data identity did not change the runtime fingerprint")
	}
	if got := sourceDataFingerprint("base", "", nil); got != "base" {
		t.Fatalf("fingerprint without source data = %q, want base unchanged", got)
	}
}

// Every source is delivered by a client into its pool's cache, whatever it is
// and wherever it came from; what differs is which cache (ADR 26-10-10-545
// §§1-2).
func TestEverySourceIsDeliveredIntoTheCacheOfItsRemote(t *testing.T) {
	str := func(value string) *string { return &value }
	origin := &model.Origin{HostID: "host_clientbbbbbbbb"}
	const directory = "/src/alpha"

	tests := []struct {
		name   string
		source model.GitSource
		origin *model.Origin
		want   string
		why    string
	}{
		{
			name:   "a remote URL",
			source: model.GitSource{Kind: "git", Slug: str("primary"), URL: str("https://github.com/Acme/widget.git")},
			origin: origin,
			want:   "remote/github.com/Acme/widget",
			why:    "a remote-URL source's remote is its URL",
		},
		{
			name:   "a local checkout of the same remote, over SSH",
			source: model.GitSource{Kind: "git", Slug: str("primary"), LocalDirectory: str(directory), UpstreamURL: str("git@github.com:Acme/widget.git")},
			origin: origin,
			want:   "remote/github.com/Acme/widget",
			why:    "the checkout and the URL share one cache, however the remote is spelled",
		},
		{
			name:   "the same remote from another client",
			source: model.GitSource{Kind: "git", Slug: str("primary"), LocalDirectory: str("/home/other/widget"), UpstreamURL: str("https://github.com/Acme/widget")},
			origin: &model.Origin{HostID: "host_someoneelse"},
			want:   "remote/github.com/Acme/widget",
			why:    "the host identity is deliberately not part of a remote's key",
		},
		{
			name:   "a local checkout with no remote",
			source: model.GitSource{Kind: "git", Slug: str("primary"), LocalDirectory: str(directory)},
			origin: origin,
			want:   "directory/" + originkey.Of(origin.HostID, directory),
			why:    "with no remote the cache is the directory's on its client's host",
		},
		{
			name:   "a remote that is a path on the client",
			source: model.GitSource{Kind: "git", Slug: str("primary"), LocalDirectory: str(directory), UpstreamURL: str("/srv/git/alpha.git")},
			origin: origin,
			want:   "directory/" + originkey.Of(origin.HostID, directory),
			why:    "a path names a repository only on the machine it is a path on",
		},
		{
			name:   "no remote and no host identity",
			source: model.GitSource{Kind: "git", Slug: str("primary"), LocalDirectory: str(directory)},
			origin: nil,
			want:   "sandbox/sbx-1/primary",
			why:    "with neither the source has a cache of its own, removed with the sandbox",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			source := tc.source
			if err := resolveSourceDelivery("sbx-1", &source, nil, tc.origin); err != nil {
				t.Fatal(err)
			}
			if source.Delivery != model.GitSourceDeliveryCache {
				t.Fatalf("delivery = %q, want cache", source.Delivery)
			}
			if source.CacheKey == nil || *source.CacheKey != tc.want {
				t.Fatalf("cache key = %v, want %q: %s", source.CacheKey, tc.want, tc.why)
			}
		})
	}
}

// The delivery and the cache are the server's record, for a source code
// reference as for the primary source: a client names neither.
func TestAClientNamesNeitherADeliveryNorACache(t *testing.T) {
	str := func(value string) *string { return &value }
	for _, delivery := range []string{model.GitSourceDeliveryPush, model.GitSourceDeliveryClone, model.GitSourceDeliveryCache} {
		source := &model.GitSource{Kind: "git", Slug: str("primary"), URL: str("https://github.com/acme/widget"), Delivery: delivery}
		if err := resolveSourceDelivery("sbx-1", source, nil, nil); err == nil {
			t.Fatalf("a client's delivery %q was accepted", delivery)
		}
	}

	forged := "remote/github.com/someone/else"
	source := &model.GitSource{Kind: "git", Slug: str("primary"), URL: str("https://github.com/acme/widget"), CacheKey: &forged}
	refs := model.SourceCodeReferences{
		"/src/lib": {Kind: "git", Slug: str("lib"), URL: str("https://github.com/acme/lib"), CacheKey: &forged},
	}
	if err := resolveSourceDelivery("sbx-1", source, refs, nil); err != nil {
		t.Fatal(err)
	}
	if got := *source.CacheKey; got != "remote/github.com/acme/widget" {
		t.Fatalf("primary cache key = %q, want the server's", got)
	}
	if got := refs["/src/lib"]; got.Delivery != model.GitSourceDeliveryCache || got.CacheKey == nil || *got.CacheKey != "remote/github.com/acme/lib" {
		t.Fatalf("reference = %+v, want cache-delivered into its own remote's cache", got)
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
			name:    "a cache-delivered source with no delivery reported waits",
			sandbox: &model.Sandbox{SandboxManifest: model.SandboxManifest{Source: &model.GitSource{Kind: "git", Delivery: model.GitSourceDeliveryCache}}},
			want:    true,
			why:     "its namespace of the pool's cache is empty until a client delivers",
		},
		{
			name:    "a cache-delivered source reported delivered proceeds",
			sandbox: &model.Sandbox{SourceDeliveredAt: &delivered, SandboxManifest: model.SandboxManifest{Source: &model.GitSource{Kind: "git", Delivery: model.GitSourceDeliveryCache}}},
			want:    false,
			why:     "the client reported the delivery complete, however early",
		},
		{
			name:    "clone source never waits",
			sandbox: &model.Sandbox{SandboxManifest: model.SandboxManifest{Source: &model.GitSource{Kind: "git", Delivery: model.GitSourceDeliveryClone}}},
			want:    false,
			why:     "the sandbox fetches a clone-delivered source itself",
		},
		{
			name: "a local source once served live, in a sandbox that has not run, waits",
			sandbox: func() *model.Sandbox {
				directory := "/src/alpha"
				sb := &model.Sandbox{SandboxManifest: model.SandboxManifest{Source: &model.GitSource{Kind: "git", Delivery: model.GitSourceDeliveryClone, LocalDirectory: &directory}}}
				sb.State = model.SandboxStatePending
				return sb
			}(),
			want: true,
			why:  "no pool serves the developer's directory now, so there is nothing to clone until a client pushes",
		},
		{
			name: "a local source once served live, in a sandbox that ran, does not wait",
			sandbox: func() *model.Sandbox {
				directory := "/src/alpha"
				sb := &model.Sandbox{SandboxManifest: model.SandboxManifest{Source: &model.GitSource{Kind: "git", Delivery: model.GitSourceDeliveryClone, LocalDirectory: &directory}}}
				sb.State = model.SandboxStateReady
				return sb
			}(),
			want: false,
			why:  "it cloned its source long ago, and parking it would stop a working sandbox",
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
		return clientDeliveredSources(&model.Sandbox{SandboxManifest: model.SandboxManifest{
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

// Skills sources name skills references, in order, at clean paths; a skills
// reference no directory names, a skills primary, and a directory naming a
// source being worked on are each refused. An explicit work purpose is
// recorded as absent.
func TestValidateSkillsSources(t *testing.T) {
	skills := func() model.GitSource { return model.GitSource{Kind: "git", Purpose: model.GitSourcePurposeSkills} }
	work := model.GitSource{Kind: "git", Purpose: model.GitSourcePurposeWork}
	refs := model.SourceCodeReferences{"/workspace/skill-sources/tools": skills(), "/workspace/lib": work}
	ordered := []model.SkillsSource{
		{Source: "/workspace/skill-sources/tools", Directory: "agents/skills"},
		{Source: "/workspace/skill-sources/tools", Directory: "."},
	}
	if err := validateSkillsSources(nil, refs, ordered); err != nil {
		t.Fatalf("validate skills sources: %v", err)
	}
	if got := refs["/workspace/lib"].Purpose; got != "" {
		t.Fatalf("work purpose recorded as %q, want absent", got)
	}

	refused := map[string]func() (*model.GitSource, model.SourceCodeReferences, []model.SkillsSource){
		"a skills primary": func() (*model.GitSource, model.SourceCodeReferences, []model.SkillsSource) {
			primary := skills()
			return &primary, nil, nil
		},
		"an unnamed skills reference": func() (*model.GitSource, model.SourceCodeReferences, []model.SkillsSource) {
			return nil, model.SourceCodeReferences{"/s": skills()}, nil
		},
		"a directory naming a worked-on source": func() (*model.GitSource, model.SourceCodeReferences, []model.SkillsSource) {
			return nil, model.SourceCodeReferences{"/w": {Kind: "git"}}, []model.SkillsSource{{Source: "/w", Directory: "."}}
		},
		"a directory naming no source": func() (*model.GitSource, model.SourceCodeReferences, []model.SkillsSource) {
			return nil, nil, []model.SkillsSource{{Source: "/nowhere", Directory: "."}}
		},
		"a directory leaving the repository": func() (*model.GitSource, model.SourceCodeReferences, []model.SkillsSource) {
			return nil, model.SourceCodeReferences{"/s": skills()}, []model.SkillsSource{{Source: "/s", Directory: "../x"}}
		},
		"an unknown purpose on the primary": func() (*model.GitSource, model.SourceCodeReferences, []model.SkillsSource) {
			return &model.GitSource{Kind: "git", Purpose: "docs"}, nil, nil
		},
		"an unknown purpose": func() (*model.GitSource, model.SourceCodeReferences, []model.SkillsSource) {
			return nil, model.SourceCodeReferences{"/s": {Kind: "git", Purpose: "docs"}}, nil
		},
	}
	for name, build := range refused {
		t.Run(name, func(t *testing.T) {
			if err := validateSkillsSources(build()); err == nil {
				t.Fatal("validate skills sources: nil, want a refusal")
			}
		})
	}
}

// A source's working directory reaches host tools as stored -- an editor's
// `ssh-remote+<host><workdir>` URL above all -- so the server, which holds
// the only copy, takes nothing but a path inside the source's own directory
// (ADR 26-10-15-417). `@attacker.example/x` is the value that, joined to a
// host with no separator, names another host.
func TestValidateSourceWorkingDirectory(t *testing.T) {
	linux := platform.Platform{OS: "linux", Arch: "amd64"}
	windows := platform.Platform{OS: "windows", Arch: "amd64"}
	str := func(v string) *string { return &v }
	src := func(directory, workingDirectory *string) *model.GitSource {
		return &model.GitSource{Destination: &model.GitSourceDestination{Directory: directory, WorkingDirectory: workingDirectory}}
	}
	for _, tc := range []struct {
		name     string
		source   *model.GitSource
		platform platform.Platform
		ok       bool
	}{
		{"no source", nil, linux, true},
		{"no destination", &model.GitSource{}, linux, true},
		{"no working directory", src(str("/workspace/app"), nil), linux, true},
		{"the directory itself", src(str("/workspace/app"), str("/workspace/app")), linux, true},
		{"a subdirectory", src(str("/home/dev/app"), str("/home/dev/app/services/api")), linux, true},
		{"a path with a space, as the client mirrors one", src(str("/Users/ada/My Projects/app"), str("/Users/ada/My Projects/app/cmd")), linux, true},
		{"absolute with no directory named", src(nil, str("/workspace/sub")), linux, true},
		{"a host in disguise", src(str("/workspace/app"), str("@attacker.example/x")), linux, false},
		{"a host in disguise with no directory named", src(nil, str("@attacker.example/x")), linux, false},
		{"relative", src(str("/workspace/app"), str("app/cmd")), linux, false},
		{"outside the directory", src(str("/workspace/app"), str("/etc")), linux, false},
		{"a sibling sharing a prefix", src(str("/workspace/app"), str("/workspace/application")), linux, false},
		{"climbing out", src(str("/workspace/app"), str("/workspace/app/../../etc")), linux, false},
		{"leading space", src(str("/workspace/app"), str(" /workspace/app")), linux, false},
		{"a control character", src(str("/workspace/app"), str("/workspace/app/\x1b[2J")), linux, false},
		{"empty", src(str("/workspace/app"), str("")), linux, false},
		{"windows drive path", src(str("C:/src/app"), str(`C:\src\app\cmd`)), windows, true},
		{"POSIX path on windows", src(str("C:/src/app"), str("/src/app")), windows, false},
		{"undeclared platform, POSIX", src(str("/workspace/app"), str("/workspace/app/cmd")), platform.Platform{}, true},
		{"undeclared platform, windows", src(str("C:/src/app"), str(`C:\src\app\cmd`)), platform.Platform{}, true},
		{"undeclared platform, a host in disguise", src(nil, str("@attacker.example/x")), platform.Platform{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSourceWorkingDirectory(tc.source, tc.platform)
			if tc.ok && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// A skills source is placed below home, or at a clean absolute path as every
// sandbox before ADR 26-11-01-692 holds it; nothing else is placed below
// home, under either of the directories a source names.
func TestValidateSourceDirectories(t *testing.T) {
	linux := platform.Platform{OS: "linux", Arch: "amd64"}
	str := func(v string) *string { return &v }
	skills := func(directory *string) model.GitSource {
		ref := model.GitSource{Kind: "git", Purpose: model.GitSourcePurposeSkills}
		if directory != nil {
			ref.Destination = &model.GitSourceDestination{Directory: directory}
		}
		return ref
	}
	home := "~/.discobox/skill-sources/tools"
	for _, tc := range []struct {
		name     string
		source   *model.GitSource
		refs     model.SourceCodeReferences
		platform platform.Platform
		ok       bool
	}{
		{"skills below home", nil, model.SourceCodeReferences{home: skills(str(home))}, linux, true},
		{"skills below home, platform not declared", nil, model.SourceCodeReferences{home: skills(str(home))}, platform.Platform{}, true},
		{"skills under the working root, as before", nil, model.SourceCodeReferences{"/workspace/skill-sources/tools": skills(str("/workspace/skill-sources/tools"))}, linux, true},
		{"work under the working root", &model.GitSource{Destination: &model.GitSourceDestination{Directory: str("/workspace/app")}}, model.SourceCodeReferences{"/workspace/lib": {Kind: "git"}}, linux, true},
		{"work keyed below home", nil, model.SourceCodeReferences{"~/lib": {Kind: "git"}}, linux, false},
		{"work placed below home", nil, model.SourceCodeReferences{"/workspace/lib": {Kind: "git", Destination: &model.GitSourceDestination{Directory: str("~/lib")}}}, linux, false},
		{"primary below home", &model.GitSource{Destination: &model.GitSourceDestination{Directory: str("~/app")}}, nil, linux, false},
		{"skills climbing out of home", nil, model.SourceCodeReferences{"~/../etc": skills(nil)}, linux, false},
		{"skills placed out of home", nil, model.SourceCodeReferences{home: skills(str("~/../etc"))}, linux, false},
		{"skills at a relative path", nil, model.SourceCodeReferences{"skill-sources/tools": skills(nil)}, linux, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSourceDirectories(tc.source, tc.refs, tc.platform)
			if tc.ok && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
