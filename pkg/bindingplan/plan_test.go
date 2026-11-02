package bindingplan

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)

func at(i int) time.Time { return t0.Add(time.Duration(i) * time.Minute) }

func fates(p Plan) map[string]Fate {
	out := map[string]Fate{}
	for _, s := range p.Steps {
		out[s.Binding.ID] = s.Fate
	}
	return out
}

func TestUseKeepsTheOneInTheAskedVariable(t *testing.T) {
	standing := []Binding{
		{ID: "old", Use: true, EnvName: "GITHUB_TOKEN", Created: at(0)},
		{ID: "asked", Use: true, EnvName: "GH_TOKEN", Created: at(1)},
		{ID: "both", Use: true, Delegate: true, EnvName: "GH_B", Created: at(2)},
		{ID: "del", Delegate: true, Created: at(3)},
	}
	p := Use(standing, "GH_TOKEN")
	if p.Keep != "asked" {
		t.Fatalf("keep %q, want asked", p.Keep)
	}
	want := map[string]Fate{"old": Deleted, "asked": Updated, "both": LosesUse}
	got := fates(p)
	if len(got) != len(want) {
		t.Fatalf("fates %v, want %v", got, want)
	}
	for id, f := range want {
		if got[id] != f {
			t.Fatalf("fates %v, want %v", got, want)
		}
	}
}

func TestUseKeepsTheOldestWhenNoneIsInTheVariable(t *testing.T) {
	standing := []Binding{
		{ID: "newer", Use: true, EnvName: "B", Created: at(5)},
		{ID: "older", Use: true, EnvName: "A", Created: at(1)},
	}
	p := Use(standing, "C")
	if p.Keep != "older" || fates(p)["newer"] != Deleted || p.Steps[0].Binding.ID != "older" {
		t.Fatalf("plan %+v, want older kept and first", p)
	}
}

func TestUseWithOnlyACombinedBindingMakesANewOne(t *testing.T) {
	p := Use([]Binding{{ID: "both", Use: true, Delegate: true, EnvName: "A"}}, "A")
	if p.Keep != "" || fates(p)["both"] != LosesUse {
		t.Fatalf("plan %+v, want a new binding and both losing its use", p)
	}
	if p := Use(nil, "A"); p.Keep != "" || len(p.Steps) != 0 {
		t.Fatalf("plan %+v, want nothing touched", p)
	}
}

func TestDelegateUpdatesOrSplits(t *testing.T) {
	if p := Delegate([]Binding{{ID: "u", Use: true, EnvName: "A"}, {ID: "d", Delegate: true}}); p.Keep != "d" || fates(p)["d"] != Updated || len(p.Steps) != 1 {
		t.Fatalf("plan %+v, want d updated alone", p)
	}
	if p := Delegate([]Binding{{ID: "both", Use: true, Delegate: true, EnvName: "A"}}); p.Keep != "both" || fates(p)["both"] != Split {
		t.Fatalf("plan %+v, want both split", p)
	}
	if p := Delegate([]Binding{{ID: "u", Use: true, EnvName: "A"}}); p.Keep != "" || len(p.Steps) != 0 {
		t.Fatalf("plan %+v, want a new delegation", p)
	}
}

// Both parts approved over one binding that uses and delegates: the use part
// takes its use away and makes a new use binding, and the delegate part then
// updates what is left — the delegation alone — in place. Planned from the
// same snapshot, the delegate part would split it, showing a second new use
// binding that is never made.
func TestAnAnswerPlansTheDelegatePartAfterTheUsePart(t *testing.T) {
	standing := []Binding{{ID: "both", Use: true, Delegate: true, EnvName: "GH_LEAD", Created: at(0)}}
	use, delegate := Answer(standing, "GH_TOKEN", true, true)
	if use.Keep != "" || fates(use)["both"] != LosesUse {
		t.Fatalf("use plan %+v, want a new binding and both losing its use", use)
	}
	if delegate.Keep != "both" || fates(delegate)["both"] != Updated || len(delegate.Steps) != 1 {
		t.Fatalf("delegate plan %+v, want both updated in place, not split", delegate)
	}
	if delegate.Steps[0].Binding.Use {
		t.Fatalf("delegate step %+v, want the binding as the use part leaves it", delegate.Steps[0])
	}
	// The delegate part alone still splits it, and a part not approved plans
	// nothing.
	use, delegate = Answer(standing, "GH_TOKEN", false, true)
	if len(use.Steps) != 0 || fates(delegate)["both"] != Split {
		t.Fatalf("plans %+v / %+v, want the delegate part alone to split", use, delegate)
	}
}

func TestAfterIsWhatTheUsePartLeaves(t *testing.T) {
	standing := []Binding{
		{ID: "old", Use: true, EnvName: "A", Created: at(0)},
		{ID: "asked", Use: true, EnvName: "B", Created: at(1)},
		{ID: "both", Use: true, Delegate: true, EnvName: "C", Created: at(2)},
	}
	left := After(standing, Use(standing, "B"), "B")
	if len(left) != 2 || left[0].ID != "asked" || left[1].ID != "both" || left[1].Use || left[1].EnvName != "" {
		t.Fatalf("after %+v", left)
	}
}

// Two bindings written within one clock tick are still ordered the same way
// every time, by ID, so the preview and the server keep the same one.
func TestASharedTimestampIsOrderedByID(t *testing.T) {
	at := time.Unix(1700000000, 0)
	for range 10 {
		plan := Use([]Binding{
			{ID: "bind_b", Use: true, EnvName: "GH_B", Created: at},
			{ID: "bind_a", Use: true, EnvName: "GH_A", Created: at},
		}, "GITHUB_TOKEN")
		if plan.Keep != "bind_a" {
			t.Fatalf("kept %q, want bind_a: the lower ID breaks the tie", plan.Keep)
		}
	}
}

// Within one second the server's stored time is finer than the API's, which
// the CLI and console plan from: the order is the ID's, not the sub-second,
// so the preview keeps the binding the server keeps.
func TestWithinASecondTheIDDecidesWhateverThePrecision(t *testing.T) {
	at := time.Unix(1700000000, 0)
	server := []Binding{
		{ID: "bind_b", Use: true, EnvName: "GH_B", Created: at.Add(100 * time.Millisecond)},
		{ID: "bind_a", Use: true, EnvName: "GH_A", Created: at.Add(900 * time.Millisecond)},
	}
	preview := []Binding{
		{ID: "bind_b", Use: true, EnvName: "GH_B", Created: at},
		{ID: "bind_a", Use: true, EnvName: "GH_A", Created: at},
	}
	if s, p := Use(server, "GITHUB_TOKEN").Keep, Use(preview, "GITHUB_TOKEN").Keep; s != p || s != "bind_a" {
		t.Fatalf("server keeps %q, preview %q; want both bind_a", s, p)
	}
}
