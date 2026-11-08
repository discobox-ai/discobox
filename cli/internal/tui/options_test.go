package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

// The chip strip always names the resolved harness.
func TestTheChipStripAlwaysNamesTheResolvedHarness(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource())
	st := newStyles(false)

	if chips := m.opts.chips(st); !strings.Contains(chips, "claude") {
		t.Fatalf("chips = %q, want the resolved default harness", chips)
	}

	// Choosing one is exactly when it earns its place on the strip.
	m.opts.opts[optHarness].idx = 1
	chosen := m.opts.opts[optHarness].display()
	if chips := m.opts.chips(st); !strings.Contains(chips, chosen) {
		t.Fatalf("chips = %q, want the harness that was chosen (%q)", chips, chosen)
	}
}

func TestShiftTabCyclesTheHarnessWithoutOpeningOptions(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource())
	before := m.opts.opts[optHarness].display()

	send(t, m, keyPress("shift+tab"))

	if m.optionsOpen {
		t.Fatal("shift+tab should not open the run options")
	}
	if after := m.opts.opts[optHarness].display(); after == before {
		t.Fatalf("harness = %q, want a choice after %q", after, before)
	}
}

func TestHarnessChipAndMarkerAreMutedForDefaultAndGoldForOverride(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource())
	st := newStyles(true)
	harness := m.opts.opts[optHarness]

	if chips := m.opts.chips(st); !strings.Contains(chips, st.chip.Render(harness.display())) ||
		!strings.Contains(chips, st.chip.Render("⏵⏵ ")) {
		t.Fatalf("default chips = %q, want a muted marker and %q", chips, harness.display())
	}
	harness.cycle(1)
	if chips := m.opts.chips(st); !strings.Contains(chips, st.chipOn.Render(harness.display())) ||
		!strings.Contains(chips, st.chipOn.Render("⏵⏵ ")) {
		t.Fatalf("override chips = %q, want a gold marker and %q", chips, harness.display())
	}
}

func TestHarnessChipStripIsEntirelyMutedWithoutPromptFocus(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource())
	st := newStyles(true)
	m.opts.opts[optDetach].value = "on"

	chips := m.opts.mutedChips(st)
	for _, want := range []string{"⏵⏵ ", "claude", " · ", "detached"} {
		if !strings.Contains(chips, st.chip.Render(want)) {
			t.Fatalf("muted chips = %q, want muted %q", chips, want)
		}
	}
	if strings.Contains(chips, st.chipOn.Render("⏵⏵ ")) || strings.Contains(chips, st.chipOn.Render("claude")) {
		t.Fatalf("muted chips retain an active color: %q", chips)
	}
}

// There is no "none" among the choices: running without a coding harness is
// the `shell` harness, which is one of the project's like any other.
func TestTheHarnessChoicesOfferNoNone(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource())
	for _, choice := range m.opts.opts[optHarness].choices {
		if strings.Contains(strings.ToLower(choice), "none") {
			t.Fatalf("choices = %v, want no none-shaped entry", m.opts.opts[optHarness].choices)
		}
	}
}

// With no project default there is nothing to lead with, so index zero names no
// harness rather than promoting whichever was registered first — which is how
// the strip came to announce one nobody had chosen.
func TestWithNoProjectDefaultNoHarnessIsClaimed(t *testing.T) {
	t.Parallel()
	ds := newFakeSource()
	for i := range ds.harnesses {
		ds.harnesses[i].Default = false
	}
	m := newTestModel(t, ds)

	harness := m.opts.opts[optHarness]
	if harness.choices[0] != unsetHarness {
		t.Fatalf("leading choice = %q, want %q", harness.choices[0], unsetHarness)
	}
	if m.opts.request("", m.opts.opts[optSource].selected()).Harness != "" {
		t.Fatal("an unset harness must emit no --harness")
	}
	if chips := m.opts.chips(newStyles(false)); !strings.Contains(chips, unsetHarness) {
		t.Fatalf("chips = %q, want the unresolved harness state", chips)
	}
}

// The strip exists even before an override because the resolved harness is
// always useful context for the prompt.
func TestTheChipStripStartsWithTheHarness(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource())
	st := newStyles(false)

	if chips := m.opts.chips(st); !strings.Contains(chips, "⏵⏵") || !strings.Contains(chips, "claude") {
		t.Fatalf("chips = %q, want the default harness", chips)
	}

	m.opts.opts[optDetach].value = "on"
	if chips := m.opts.chips(st); !strings.Contains(chips, "⏵⏵") || !strings.Contains(chips, "detached") {
		t.Fatalf("chips = %q, want the marker back once there is something to say", chips)
	}
}

// The Source row offers what the project has been cut from, off the listing,
// with the folder the header is on leading and "no source" last. It is a
// selector rather than a text field so the common case — the two or three
// places you work in — never opens one.
func TestTheSourceRowOffersTheProjectsSources(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(testSandboxes()...))

	source := m.opts.opts[optSource]
	if source.choices[0] != m.opts.sourceLabel() {
		t.Fatalf("leading choice = %q, want the folder the header is on (%q)", source.choices[0], m.opts.sourceLabel())
	}
	if source.changed() {
		t.Fatal("the leading choice is what run would do anyway, so nothing is changed")
	}
	for _, want := range []string{"/src/obot", "https://github.com/acme/foo"} {
		if !hasSourceValue(source, want) {
			t.Fatalf("values = %v, want %q from the listing", source.values, want)
		}
	}
	if last := source.values[len(source.values)-1]; last != sourceNone {
		t.Fatalf("last value = %q, want no source", last)
	}
}

// "No source" is a create with nothing checked out in it, which is one flag
// rather than a source that happens to be empty.
func TestNoSourceAsksForTheFlagAndNoDirectory(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	m.opts.chooseSource(sourceNone)

	req := m.opts.request("do a thing", m.opts.opts[optSource].selected())
	if !req.NoSource {
		t.Fatal("request should ask for --no-source")
	}
	if req.Source != "" {
		t.Fatalf("source = %q, want nothing to cut from", req.Source)
	}
	if cmd := m.opts.command("do a thing"); !strings.Contains(cmd, "discobox run --no-source") || strings.Contains(cmd, "run .") {
		t.Fatalf("command = %q, want --no-source in SOURCE's place", cmd)
	}
	if chips := m.opts.chips(newStyles(false)); !strings.Contains(chips, console.NoSourceChoice) {
		t.Fatalf("chips = %q, want the strip to say there is no source", chips)
	}
}

// A source the listing has never seen is typed in, and keeps its place on the
// row until something else is chosen — a refresh of the listing underneath an
// open panel must not drop the one entry the listing could not know about.
func TestATypedSourceSurvivesARefresh(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(testSandboxes()...))

	m.opts.chooseSource("/src/elsewhere@main")
	m.opts.setSources(m.list.Sources())

	if got := m.opts.request("", m.opts.opts[optSource].selected()).Source; got != "/src/elsewhere@main" {
		t.Fatalf("source = %q, want the one that was typed", got)
	}
	if got := m.opts.typedSource(); got != "/src/elsewhere@main" {
		t.Fatalf("the input field should reopen holding %q, got %q", "/src/elsewhere@main", got)
	}
}

// The header and the Source row are one control in both directions. Choosing a
// source moves the list to the folder that source's discoboxes are filed under:
// a directory's, or a repository URL's (ADR 0111). "No source" goes to the
// window's own folder, which holds this machine's discoboxes with no source.
func TestChoosingASourceMovesTheList(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(testSandboxes()...))

	m.opts.chooseSource("/src/obot")
	m.followSource()
	if m.list.Filter.Folder.Key != testKey("/src/obot") {
		t.Fatalf("folder = %q, want the list to follow the source", m.list.Filter.Folder.Label)
	}
	// Followed there, the source is the folder again and emits no -C: the
	// header already says where the next discobox is cut from.
	if req := m.opts.request("", m.opts.opts[optSource].selected()); req.Source != "/src/obot" {
		t.Fatalf("source = %q, want the folder the list moved to", req.Source)
	}

	m.opts.chooseSource("https://github.com/acme/foo")
	m.followSource()
	if m.list.Filter.Folder.Key != testKey("https://github.com/acme/foo") {
		t.Fatalf("folder = %q, want the repository URL's own", m.list.Filter.Folder.Label)
	}
	if req := m.opts.request("", m.opts.opts[optSource].selected()); req.Source != "https://github.com/acme/foo" {
		t.Fatalf("source = %q, want the remote repository", req.Source)
	}

	m.opts.chooseSource(sourceNone)
	m.followSource()
	if m.list.Filter.Folder.Key != m.session.OriginKey {
		t.Fatalf("folder = %q, want the window's own: a sourceless discobox is filed under this machine", m.list.Filter.Folder.Label)
	}
}

// One repository is one choice on the Source row, however each discobox
// spelled its URL, and it reads as its short name (ADR 26-10-15-664 §4) —
// but only spellings fetched alike: ssh is another choice, since choosing
// https must never run another discobox's ssh, http or git:// URL.
func TestTheSourceRowOffersARepositoryOnce(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(
		cutFrom(console.Sandbox{ID: "sbx_a", Name: "a", State: console.StateRunning}, "https://github.com/acme/foo.git"),
		cutFrom(console.Sandbox{ID: "sbx_b", Name: "b", State: console.StateRunning}, "https://github.com/acme/foo"),
		cutFrom(console.Sandbox{ID: "sbx_c", Name: "c", State: console.StateRunning}, "git@github.com:Acme/Foo.git"),
		cutFrom(console.Sandbox{ID: "sbx_d", Name: "d", State: console.StateRunning}, "https://github.com/acme/foo@dev"),
	))
	source := m.opts.opts[optSource]
	var repos []string
	for _, choice := range source.choices {
		if strings.Contains(strings.ToLower(choice), "acme/foo") {
			repos = append(repos, choice)
		}
	}
	if want := []string{"acme/foo", "git@github.com:Acme/Foo", "acme/foo@dev"}; !slices.Equal(repos, want) {
		t.Fatalf("choices = %v, want https once, ssh once, and the other ref once: %v", source.choices, want)
	}

	m.opts.chooseSource("https://GitHub.com/acme/foo")
	if got := m.opts.opts[optSource].selected(); got != "https://github.com/acme/foo.git" {
		t.Fatalf("choosing another https spelling selected %q, want the choice already offered", got)
	}
	m.opts.chooseSource("git@github.com:acme/foo.git")
	if got := m.opts.opts[optSource].selected(); got != "git@github.com:Acme/Foo.git" {
		t.Fatalf("choosing an ssh spelling selected %q, want the ssh choice", got)
	}
}

func hasSourceValue(o *option, want string) bool {
	for _, value := range o.values {
		if value == want {
			return true
		}
	}
	return false
}

// Another machine's discoboxes with no source are a folder this window can
// list, and nothing it can cut from: the panel falls back to the window's own
// source rather than carrying the "no source" sentinel into what it draws.
func TestASourcelessFolderElsewhereCutsFromTheWindowsOwn(t *testing.T) {
	t.Parallel()
	far := console.Sandbox{ID: "sbx_far", Name: "far", State: console.StateRunning, HasRuntime: true,
		OriginKey: farHostKey, OriginHostID: farHostID}
	m := newTestModel(t, newFakeSource(append(testSandboxes(), far)...))

	chosen := false
	for _, f := range m.list.Folders() {
		if f.Key == farHostKey {
			m.applyFilter(m.list.Filter.Server, f, m.list.Filter.Tags)
			chosen = true
		}
	}
	if !chosen {
		t.Fatal("another machine's sourceless folder should be offered")
	}
	if got := m.opts.sourceDir(); got != m.session.Directory {
		t.Fatalf("source = %q, want the window's own (%q)", got, m.session.Directory)
	}
	if strings.ContainsRune(m.opts.sourceLabel(), 0) {
		t.Fatalf("the panel carries the no-source sentinel: %q", m.opts.sourceLabel())
	}
	if req := m.opts.request("x", m.opts.opts[optSource].selected()); req.NoSource || req.Source == sourceNone {
		t.Fatalf("request = %+v, want a create from the window's own source", req)
	}
}

// testPools are a project's pools with the second one its default.
func testPools() []console.Pool {
	return []console.Pool{
		{ID: "pool-arm", Name: "arm", Platform: "linux/arm64"},
		{ID: "pool-local", Name: "local", Platform: "linux/amd64", Default: true},
		{ID: "pool-gpu", Name: "gpu"},
	}
}

// The Pool row leads with the project default, which asks for no flag, and
// stepping off it is --pool, on the strip and in the command both.
func TestThePoolRowSendsTheChosenPool(t *testing.T) {
	t.Parallel()
	ds := newFakeSource()
	ds.pools = testPools()
	m := newTestModel(t, ds)
	st := newStyles(false)

	row := m.opts.opts[optPool]
	if !slices.Equal(row.choices, []string{"local", "arm", "gpu"}) {
		t.Fatalf("choices = %q, want the default leading", row.choices)
	}
	if req := m.opts.request("", m.opts.opts[optSource].selected()); req.Pool != "" {
		t.Fatalf("Pool = %q, want none for the default", req.Pool)
	}
	if strings.Contains(m.opts.command(""), "--pool") || strings.Contains(m.opts.chips(st), "pool") {
		t.Fatal("the default pool is not worth a flag or a chip")
	}

	send(t, m, keyPress("ctrl+o"))
	m.opts.moveTo(optPool)
	send(t, m, keyPress("right"))
	if req := m.opts.request("", m.opts.opts[optSource].selected()); req.Pool != "arm" {
		t.Fatalf("Pool = %q, want arm", req.Pool)
	}
	if command := m.opts.command(""); !strings.Contains(command, "--pool arm") {
		t.Fatalf("command = %q, want --pool arm", command)
	}
	if chips := m.opts.chips(st); !strings.Contains(chips, "pool arm") {
		t.Fatalf("chips = %q, want the chosen pool", chips)
	}

	// A refresh keeps the choice where it is, however the listing reorders.
	m.opts.setPools([]console.Pool{testPools()[2], testPools()[1], testPools()[0]})
	if got := m.opts.opts[optPool].selected(); got != "arm" {
		t.Fatalf("after a refresh the row is on %q, want arm", got)
	}
}

// Two pools of one name are told apart by ID, which is the only thing --pool
// can say that picks one of them.
func TestASharedPoolNameIsSentAsItsID(t *testing.T) {
	t.Parallel()
	o := newOptions(console.Session{Directory: "/src"})
	o.setPools([]console.Pool{{ID: "p1", Name: "same", Default: true}, {ID: "p2", Name: "same"}})
	if got := o.opts[optPool].values; !slices.Equal(got, []string{"", "p2"}) {
		t.Fatalf("values = %q, want the second pool by ID", got)
	}
}

// `discobox run --pool` opens the row on that pool, whether the listing has
// come back or not, and a pool the listing does not hold stays on it so the
// create meets the server's answer about it.
func TestARequestedPoolSurvivesTheListing(t *testing.T) {
	t.Parallel()
	o := newOptions(console.Session{Directory: "/src"})
	o.prefill(console.RunRequest{Pool: "pool-gpu"})
	o.setPools(testPools())
	if got := o.opts[optPool].display(); got != "gpu" {
		t.Fatalf("row shows %q, want the pool named by ID", got)
	}
	if req := o.request("", "/src"); req.Pool != "gpu" {
		t.Fatalf("Pool = %q, want gpu", req.Pool)
	}

	o = newOptions(console.Session{Directory: "/src"})
	o.prefill(console.RunRequest{Pool: "elsewhere"})
	o.setPools(testPools())
	if req := o.request("", "/src"); req.Pool != "elsewhere" {
		t.Fatalf("Pool = %q, want the unlisted pool kept", req.Pool)
	}
}

// One server's pool is not another's: a change of server puts the row back
// on the default.
func TestAChangeOfServerForgetsThePool(t *testing.T) {
	t.Parallel()
	ds := newFakeSource()
	ds.session.Servers = []string{"alpha", "beta"}
	ds.pools = testPools()
	m := newTestModel(t, ds)
	m.opts.opts[optPool].idx = 1

	send(t, m, keyPress("ctrl+o"))
	m.opts.moveTo(optServer)
	send(t, m, keyPress("right"))
	if m.opts.opts[optPool].changed() {
		t.Fatalf("row is on %q after a change of server, want the default", m.opts.opts[optPool].display())
	}
}

// A project with one pool has nothing to choose, and the panel does not name
// what it has never heard of: the row is not drawn, and the cursor steps past
// it.
func TestThePoolRowIsHiddenWithNothingToChoose(t *testing.T) {
	t.Parallel()
	ds := newFakeSource()
	ds.session.Servers = []string{"alpha", "beta"}
	ds.pools = []console.Pool{{ID: "pool-local", Name: "local", Default: true}}
	m := newTestModel(t, ds)
	send(t, m, keyPress("ctrl+o"))
	if view := m.opts.view(m.st, &m.zones, 120, ""); strings.Contains(view, "Pool") {
		t.Fatalf("one pool is nothing to choose, but the panel shows the row:\n%s", view)
	}
	m.opts.moveTo(optInclude)
	send(t, m, keyPress("down"))
	if m.opts.cursor != optServer {
		t.Fatalf("cursor = %d, want down from the last source row to step past the pool to the server", m.opts.cursor)
	}
	send(t, m, keyPress("up"))
	if m.opts.cursor != optInclude {
		t.Fatalf("cursor = %d, want up from the server to step past the pool", m.opts.cursor)
	}
	m.opts.moveTo(optPool)
	if m.opts.cursor == optPool {
		t.Fatal("a press cannot land on a row that is not drawn")
	}
}

// `discobox run --pool` names a pool the listing may already hold, under its
// ID or its name, and the row lands on that pool rather than on a second copy
// of it — the default named outright included, which asks for no flag.
func TestARequestedPoolAfterTheListingIsTheListedPool(t *testing.T) {
	t.Parallel()
	o := newOptions(console.Session{Directory: "/src"})
	o.setPools(testPools())
	o.prefill(console.RunRequest{Pool: "pool-gpu"})
	if row := o.opts[optPool]; len(row.choices) != 3 || row.display() != "gpu" {
		t.Fatalf("row = %q on %q, want the listed gpu and no copy of it", row.choices, row.display())
	}

	o = newOptions(console.Session{Directory: "/src"})
	o.setPools(testPools())
	o.prefill(console.RunRequest{Pool: "local"})
	if req := o.request("", "/src"); req.Pool != "" || len(o.opts[optPool].choices) != 3 {
		t.Fatalf("Pool = %q with choices %q, want the default and no flag", req.Pool, o.opts[optPool].choices)
	}
}

// A pool renamed between listings is still the pool that was chosen.
func TestARenamedPoolStaysChosen(t *testing.T) {
	t.Parallel()
	o := newOptions(console.Session{Directory: "/src"})
	o.setPools(testPools())
	o.opts[optPool].idx = 1 // arm
	renamed := testPools()
	renamed[0].Name = "graviton"
	o.setPools(renamed)
	if req := o.request("", "/src"); req.Pool != "graviton" || len(o.opts[optPool].choices) != 3 {
		t.Fatalf("Pool = %q with choices %q, want the renamed pool", req.Pool, o.opts[optPool].choices)
	}
}
