package policy

import (
	"strings"
	"testing"
)

func mustParse(t *testing.T, sentences ...string) Policy {
	t.Helper()
	statements, err := ParseStatements(sentences)
	if err != nil {
		t.Fatal(err)
	}
	return Policy{Statements: statements}
}

// A lead holds a delegation of its own repository; the worker it hands on to
// is in the same repository unless a case says otherwise. Values are each
// side's own: $this is the lead on one side and the worker on the other.
var (
	leadValues   = map[string]string{VarPrimaryRepo: "acme/web", VarThis: "sbx_lead"}
	workerValues = map[string]string{VarPrimaryRepo: "acme/web", VarThis: "sbx_worker"}
)

func TestWithin(t *testing.T) {
	delegated := mustParse(t,
		`allow push, read on branch "discobox/*" in repository $primary.repo`,
		`allow manage on pull request in repository $primary.repo when "its base is main"`,
		`allow read on anything in repository "acme/docs"`,
		`deny delete on anything in anywhere`,
	)
	tests := []struct {
		name    string
		given   []string
		values  map[string]string
		refused string // the error names this; empty is within
		judge   int    // allows left to a judge
	}{
		{name: "the same", given: []string{
			`allow push, read on branch "discobox/*" in repository $primary.repo`,
			`deny delete on anything in anywhere`,
		}},
		{name: "fewer actions, a name the glob matches", given: []string{
			`allow push on branch "discobox/issue-46" in repository $primary.repo`,
			`deny delete on anything in anywhere`,
		}},
		{name: "manage covers every action, and the same condition", given: []string{
			`allow create, update on pull request in repository $primary.repo when "its base is main"`,
			`deny delete on anything in anywhere`,
		}},
		{name: "anything covers a type", given: []string{
			`allow read on issue within issue "4" in repository "acme/docs"`,
			`deny delete on anything in anywhere`,
		}},
		{name: "a deny of its own", given: []string{
			`allow push on branch "discobox/x" in repository $primary.repo`,
			`deny delete on anything in anywhere`,
			`deny push on branch "main" in anywhere`,
		}},
		{name: "a literal for the same repository", given: []string{
			`allow push on branch "discobox/x" in repository "acme/web"`,
			`deny delete on anything in anywhere`,
		}},
		{name: "an allow that matches nothing", values: map[string]string{VarPrimaryRepo: "acme/web"}, given: []string{
			`allow manage on anything in repository $primary.upstream`,
			`deny delete on anything in anywhere`,
		}},
		{name: "the delegated deny dropped", refused: `deny delete on anything in anywhere`, given: []string{
			`allow push on branch "discobox/x" in repository $primary.repo`,
		}},
		{name: "an action not delegated", refused: "allow push, delete on branch", given: []string{
			`allow push, delete on branch "discobox/x" in repository $primary.repo`,
			`deny delete on anything in anywhere`,
		}},
		{name: "a name the glob does not match", refused: `"main"`, given: []string{
			`allow push on branch "main" in repository $primary.repo`,
			`deny delete on anything in anywhere`,
		}},
		{name: "a wider glob", refused: `"*"`, given: []string{
			`allow push on branch "*" in repository $primary.repo`,
			`deny delete on anything in anywhere`,
		}},
		{name: "another repository, by the same variable", values: map[string]string{VarPrimaryRepo: "acme/other"}, refused: `"acme/other"`, given: []string{
			`allow push on branch "discobox/x" in repository $primary.repo`,
			`deny delete on anything in anywhere`,
		}},
		{name: "the delegated condition dropped", judge: 1, given: []string{
			`allow update on pull request in repository $primary.repo`,
			`deny delete on anything in anywhere`,
		}},
		{name: "another condition", judge: 1, given: []string{
			`allow update on pull request in repository $primary.repo when "its base is main and it is not a draft"`,
			`deny delete on anything in anywhere`,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := tt.values
			if values == nil {
				values = workerValues
			}
			unsettled, err := Within(mustParse(t, tt.given...), values, delegated, leadValues)
			switch {
			case tt.refused != "":
				if err == nil || !strings.Contains(err.Error(), tt.refused) {
					t.Fatalf("err = %v, want it to name %q", err, tt.refused)
				}
			case err != nil:
				t.Fatalf("refused: %v", err)
			case len(unsettled) != tt.judge:
				t.Fatalf("left %d allows to a judge, want %d: %+v", len(unsettled), tt.judge, unsettled)
			}
		})
	}
}

// A delegation's created by $this is resources the lead created; the worker's
// are its own, which are not the lead's, so they are not covered.
func TestWithinComparesCreatedByAsResolved(t *testing.T) {
	delegated := mustParse(t, `allow update on pull request created by $this in repository $primary.repo`)
	given := mustParse(t, `allow update on pull request created by $this in repository $primary.repo`)
	if _, err := Within(given, workerValues, delegated, leadValues); err == nil {
		t.Fatal("pull requests the worker created were within ones the lead created")
	}
	anyCreator := mustParse(t, `allow update on pull request in repository $primary.repo`)
	if _, err := Within(given, workerValues, anyCreator, leadValues); err != nil {
		t.Fatalf("a narrower created-by was refused: %v", err)
	}
}

// A word means what the delegation says it means: one defined otherwise could
// widen an action the delegation names.
func TestWithinHoldsWordsToTheDelegationsDefinitions(t *testing.T) {
	delegated := mustParse(t, `allow push on branch in repository $primary.repo`)
	delegated.Definitions = map[string]string{"push": "send commits to a branch, never forced"}
	given := mustParse(t, `allow push on branch in repository $primary.repo`)
	given.Definitions = map[string]string{"push": "send commits, forced or not"}
	if _, err := Within(given, workerValues, delegated, leadValues); err == nil || !strings.Contains(err.Error(), `"push"`) {
		t.Fatalf("err = %v, want the redefined word named", err)
	}
	given.Definitions["push"] = delegated.Definitions["push"]
	if _, err := Within(given, workerValues, delegated, leadValues); err != nil {
		t.Fatalf("the same definition was refused: %v", err)
	}
}

// The judge is asked about the delegated allows that cover an allow but for
// their conditions, each as it resolved, so it reads values, not variables.
func TestWithinLeavesConditionsToAJudge(t *testing.T) {
	delegated := mustParse(t, `allow update on pull request in repository $primary.repo when "its base is main"`)
	given := mustParse(t, `allow update on pull request in repository $primary.repo when "it is ready"`)
	unsettled, err := Within(given, workerValues, delegated, leadValues)
	if err != nil || len(unsettled) != 1 || len(unsettled[0].Delegated) != 1 {
		t.Fatalf("unsettled = %+v, %v", unsettled, err)
	}
	if got := Format(unsettled[0].Given); got != `allow update on pull request in repository "acme/web" when "it is ready"` {
		t.Fatalf("given = %q", got)
	}
}

// A name with an escape is a pattern, not the literal it looks like: one the
// delegated glob matches as text can mean a name it excludes. And a value a
// variable resolved to is a literal, however much it looks like a glob.
func TestWithinReadsEscapesAsTheGlobsTheyAre(t *testing.T) {
	delegated := mustParse(t, `allow push on branch "release-[^m]*" in repository "acme/web"`)
	given := mustParse(t, `allow push on branch "release-\\main" in repository "acme/web"`)
	if _, err := Within(given, workerValues, delegated, leadValues); err == nil {
		t.Fatal(`release-\\main, which matches release-main, was within release-[^m]*`)
	}
	ok := mustParse(t, `allow push on branch "release-1" in repository "acme/web"`)
	if _, err := Within(ok, workerValues, delegated, leadValues); err != nil {
		t.Fatalf("a literal the glob matches was refused: %v", err)
	}

	byVariable := mustParse(t, `allow push on branch $primary.branch in repository "acme/web"`)
	wide := mustParse(t, `allow push on branch "*" in repository "acme/web"`)
	if _, err := Within(wide, map[string]string{}, byVariable, map[string]string{VarPrimaryBranch: "*"}); err == nil {
		t.Fatal(`a branch named * by a variable covered every branch`)
	}
	same := mustParse(t, `allow push on branch $primary.branch in repository "acme/web"`)
	if _, err := Within(same, map[string]string{VarPrimaryBranch: "*"}, byVariable, map[string]string{VarPrimaryBranch: "*"}); err != nil {
		t.Fatalf("the same value on both sides was refused: %v", err)
	}
}
