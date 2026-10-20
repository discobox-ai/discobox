package policy

import (
	"testing"
)

var (
	yes     = Parts{Action: Yes, Resource: Yes, Scope: Yes, When: Yes, Unless: No}
	no      = Parts{Action: No, Resource: Yes, Scope: Yes, When: Yes, Unless: No}
	unknown = Parts{Action: Yes, Resource: Unknown, Scope: Yes, When: Yes, Unless: No}
)

func TestPartsMatch(t *testing.T) {
	conditional := Statement{When: ptr("w"), Unless: ptr("u")}
	plain := Statement{}
	for _, tc := range []struct {
		name  string
		s     Statement
		parts Parts
		want  Match
	}{
		{"every part fits", conditional, yes, Yes},
		{"one part does not", conditional, no, No},
		{"one part is unknown", conditional, unknown, Unknown},
		{"a no outweighs an unknown", conditional, Parts{Action: Unknown, Resource: No, Scope: Yes, When: Yes, Unless: No}, No},
		{"when does not hold", conditional, Parts{Action: Yes, Resource: Yes, Scope: Yes, When: No, Unless: No}, No},
		{"unless holds", conditional, Parts{Action: Yes, Resource: Yes, Scope: Yes, When: Yes, Unless: Yes}, No},
		{"unless is unknown", conditional, Parts{Action: Yes, Resource: Yes, Scope: Yes, When: Yes, Unless: Unknown}, Unknown},
		{"no conditions ignores theirs", plain, Parts{Action: Yes, Resource: Yes, Scope: Yes, When: No, Unless: Yes}, Yes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.parts.Matches(tc.s); got != tc.want {
				t.Fatalf("Matches = %v, want %v", got, tc.want)
			}
		})
	}
}

func twoAllowsOneDeny() Policy {
	return Policy{Statements: []Statement{{Effect: Allow}, {Effect: Allow}, {Effect: Deny}}}
}

// writes is each atom's parts, none of them a supporting read.
func writes(parts [][]Parts) []Atom {
	var atoms []Atom
	for _, p := range parts {
		atoms = append(atoms, Atom{Parts: p, Supporting: No})
	}
	return atoms
}

func TestEvaluate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		atoms     []Atom
		verdict   Verdict
		atom      int
		statement int
	}{
		{"every atom allowed, none denied", writes([][]Parts{{yes, no, no}, {no, yes, no}}), Allowed, -1, -1},
		{"a deny refuses an atom an allow covers", writes([][]Parts{{yes, no, no}, {yes, yes, yes}}), Refused, 1, 2},
		{"a deny refuses even when another atom is unknown", writes([][]Parts{{unknown, unknown, unknown}, {yes, no, yes}}), Refused, 1, 2},
		{"an atom no allow covers is refused", writes([][]Parts{{yes, no, no}, {no, no, no}}), Refused, 1, -1},
		{"an uncovered atom refuses despite an unknown one", writes([][]Parts{{unknown, no, no}, {no, no, no}}), Refused, 1, -1},
		{"an atom only possibly covered is undecided", writes([][]Parts{{yes, no, no}, {unknown, no, no}}), Undecided, 1, 0},
		{"a deny that might match is undecided", writes([][]Parts{{yes, no, unknown}}), Undecided, 0, 2},
		{"an unknown deny is named before an unknown allow", writes([][]Parts{{unknown, no, unknown}}), Undecided, 0, 2},
		{"no atoms is undecided", nil, Undecided, -1, -1},
		{"an uncovered supporting read is allowed", []Atom{{Parts: []Parts{yes, no, no}, Supporting: No}, {Parts: []Parts{no, no, no}, Supporting: Yes}}, Allowed, -1, -1},
		{"an uncovered read that might support is undecided", []Atom{{Parts: []Parts{no, no, no}, Supporting: Unknown}}, Undecided, 0, -1},
		{"a supporting read is still denied", []Atom{{Parts: []Parts{no, no, yes}, Supporting: Yes}}, Refused, 0, 2},
		{"an uncovered write refuses despite a read that might support", []Atom{{Parts: []Parts{no, no, no}, Supporting: Unknown}, {Parts: []Parts{no, no, no}, Supporting: No}}, Refused, 1, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Evaluate(twoAllowsOneDeny(), tc.atoms)
			if err != nil {
				t.Fatal(err)
			}
			if got.Verdict != tc.verdict || got.Atom != tc.atom || got.Statement != tc.statement {
				t.Fatalf("Evaluate = %+v (%s), want %v atom %d statement %d", got, got.Reason(), tc.verdict, tc.atom, tc.statement)
			}
		})
	}
}

// The order of the statements never changes the verdict: a deny listed first
// or last refuses the same request, and an allow after a deny is not undone
// by it nor undoes it.
func TestEvaluateIgnoresOrder(t *testing.T) {
	cases := [][][]Parts{
		{{yes, no, no}, {no, yes, no}},
		{{yes, no, no}, {yes, yes, yes}},
		{{yes, no, no}, {no, no, no}},
		{{yes, no, no}, {unknown, no, no}},
		{{yes, no, unknown}},
	}
	orders := [][]int{{0, 1, 2}, {2, 1, 0}, {1, 2, 0}, {2, 0, 1}}
	base := twoAllowsOneDeny()
	for i, parts := range cases {
		var first Verdict
		for j, order := range orders {
			p := Policy{}
			reordered := make([][]Parts, len(parts))
			for _, s := range order {
				p.Statements = append(p.Statements, base.Statements[s])
			}
			for a, atom := range parts {
				for _, s := range order {
					reordered[a] = append(reordered[a], atom[s])
				}
			}
			got, err := Evaluate(p, writes(reordered))
			if err != nil {
				t.Fatal(err)
			}
			if j == 0 {
				first = got.Verdict
			} else if got.Verdict != first {
				t.Fatalf("case %d: order %v decided %v, order %v decided %v", i, orders[0], first, order, got.Verdict)
			}
		}
	}
}

// An allowed request names the allow that covered each atom, or none for a
// supporting read.
func TestEvaluateNamesWhatCovered(t *testing.T) {
	got, err := Evaluate(twoAllowsOneDeny(), []Atom{
		{Parts: []Parts{no, yes, no}, Supporting: No},
		{Parts: []Parts{yes, yes, no}, Supporting: No},
		{Parts: []Parts{no, no, no}, Supporting: Yes},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Verdict != Allowed || len(got.Covered) != 3 || got.Covered[0] != 1 || got.Covered[1] != 0 || got.Covered[2] != -1 {
		t.Fatalf("Evaluate = %+v, want atoms covered by statements 2 and 1, and a supporting read", got)
	}
}

func TestEvaluateRefusesPartsThatDoNotFitThePolicy(t *testing.T) {
	if _, err := Evaluate(twoAllowsOneDeny(), writes([][]Parts{{yes}})); err == nil {
		t.Fatal("parts for one statement of three were evaluated")
	}
}
