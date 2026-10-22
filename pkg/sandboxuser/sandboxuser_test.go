package sandboxuser

import (
	"strconv"
	"strings"
	"testing"
)

// The merge matrix. Each case says what each layer named and what the three
// facets must come out as; the facets are asserted separately because the bugs
// this replaces were all one facet leaking into another's decision.
func TestMergeChoosesEachFacetIndependently(t *testing.T) {
	image := &User{Name: "node", UID: ID(1000), GID: ID(1000), HomeDirectory: "/home/node"}
	manifest := &User{Name: "dev", UID: ID(2000), GID: ID(3000), HomeDirectory: "/home/dev", AdditionalGroups: []string{"docker"}}

	for _, tc := range []struct {
		name     string
		layers   Layers
		wantName string
		wantUID  *int64
		wantGID  *int64
		wantHome string
		wantGrp  string
		wantAdd  []string
	}{
		{
			name:     "nothing requested takes the manifest whole",
			layers:   Layers{Image: image, Manifest: manifest},
			wantName: "dev", wantUID: ID(2000), wantGID: ID(3000), wantHome: "/home/dev",
			wantAdd: []string{"docker"},
		},
		{
			name:     "no manifest falls through to the image",
			layers:   Layers{Image: image},
			wantName: "node", wantUID: ID(1000), wantGID: ID(1000), wantHome: "/home/node",
		},
		{
			// The bug that silently dropped the group: a request naming only a
			// primary group by name kept the manifest's identity and lost the group.
			name:     "group name only keeps the identity and takes the group",
			layers:   Layers{Image: image, Manifest: manifest, Request: &User{GroupName: "video"}},
			wantName: "dev", wantUID: ID(2000), wantHome: "/home/dev",
			wantGrp: "video", wantAdd: []string{"docker"},
		},
		{
			// The same request spelled numerically, which used to fail outright
			// with "run user uid is required" instead of being ignored.
			name:     "numeric primary group only behaves identically",
			layers:   Layers{Image: image, Manifest: manifest, Request: &User{GID: ID(997)}},
			wantName: "dev", wantUID: ID(2000), wantGID: ID(997), wantHome: "/home/dev",
			wantAdd: []string{"docker"},
		},
		{
			// Supplementary groups cross an identity change on purpose (ADR 0025
			// §2): they describe what the sandbox may reach, not who it is.
			name:     "groups only keeps the identity and replaces the set",
			layers:   Layers{Image: image, Manifest: manifest, Request: &User{AdditionalGroups: []string{"audio", "997"}}},
			wantName: "dev", wantUID: ID(2000), wantGID: ID(3000), wantHome: "/home/dev",
			wantAdd: []string{"audio", "997"},
		},
		{
			// The primary group does NOT cross an identity change: taking the
			// manifest's gid here would run root in dev's default group.
			name:     "a named identity does not inherit the primary group beneath it",
			layers:   Layers{Image: image, Manifest: manifest, Request: &User{Name: "root", UID: ID(0)}},
			wantName: "root", wantUID: ID(0), wantGID: nil,
			wantAdd: []string{"docker"},
		},
		{
			// ...but it may still name one of its own.
			name:     "a named identity may choose its own primary group",
			layers:   Layers{Image: image, Manifest: manifest, Request: &User{Name: "root", UID: ID(0), GroupName: "wheel"}},
			wantName: "root", wantUID: ID(0), wantGrp: "wheel",
			wantAdd: []string{"docker"},
		},
		{
			name:    "an empty request layer names nobody",
			layers:  Layers{Manifest: manifest, Request: &User{}},
			wantAdd: []string{"docker"}, wantName: "dev", wantUID: ID(2000), wantGID: ID(3000), wantHome: "/home/dev",
		},
		{
			name:   "no layers at all resolves to nobody",
			layers: Layers{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Merge(tc.layers)
			if got.Name != tc.wantName {
				t.Errorf("name = %q, want %q", got.Name, tc.wantName)
			}
			if !sameID(got.UID, tc.wantUID) {
				t.Errorf("uid = %s, want %s", showID(got.UID), showID(tc.wantUID))
			}
			if !sameID(got.GID, tc.wantGID) {
				t.Errorf("gid = %s, want %s", showID(got.GID), showID(tc.wantGID))
			}
			if got.HomeDirectory != tc.wantHome {
				t.Errorf("home = %q, want %q", got.HomeDirectory, tc.wantHome)
			}
			if got.GroupName != tc.wantGrp {
				t.Errorf("groupName = %q, want %q", got.GroupName, tc.wantGrp)
			}
			if strings.Join(got.AdditionalGroups, ",") != strings.Join(tc.wantAdd, ",") {
				t.Errorf("additionalGroups = %v, want %v", got.AdditionalGroups, tc.wantAdd)
			}
		})
	}
}

// Merge must not alias any layer's slice or pointers, or a later mutation of
// the result would reach back into the manifest every other exec reads.
func TestMergeCopiesRatherThanAliases(t *testing.T) {
	manifest := &User{Name: "dev", UID: ID(2000), AdditionalGroups: []string{"docker"}}
	got := Merge(Layers{Manifest: manifest})

	*got.UID = 999
	got.AdditionalGroups[0] = "changed"

	if *manifest.UID != 2000 {
		t.Errorf("manifest uid mutated to %d", *manifest.UID)
	}
	if manifest.AdditionalGroups[0] != "docker" {
		t.Errorf("manifest groups mutated to %v", manifest.AdditionalGroups)
	}
}

// Whitespace must not make an absent field look present, or a layer carrying
// only blanks would win the identity and supply nothing.
func TestNamedIgnoresWhitespace(t *testing.T) {
	if Named(&User{Name: "  ", GroupName: "\t", HomeDirectory: " "}) {
		t.Error("a layer of blanks named somebody")
	}
	if Named(&User{}) {
		t.Error("an empty layer named somebody")
	}
	if Named(nil) {
		t.Error("a nil layer named somebody")
	}
	if !Named(&User{AdditionalGroups: []string{"docker"}}) {
		t.Error("a groups-only layer named nobody")
	}
	if !Named(&User{GroupName: "docker"}) {
		t.Error("a primary-group-only layer named nobody")
	}
}

// An empty supplementary list is "inherit", not "run with none": groups are
// all-or-nothing and only a non-empty list is a choice.
func TestEmptyGroupListIsNotAChoice(t *testing.T) {
	manifest := &User{Name: "dev", UID: ID(2000), AdditionalGroups: []string{"docker"}}
	got := Merge(Layers{Manifest: manifest, Request: &User{AdditionalGroups: []string{}}})
	if len(got.AdditionalGroups) != 1 || got.AdditionalGroups[0] != "docker" {
		t.Fatalf("additionalGroups = %v, want the manifest's [docker]", got.AdditionalGroups)
	}
}

func TestValidateRejectsTwoPrimaryGroups(t *testing.T) {
	if err := (&User{GID: ID(1), GroupName: "docker"}).Validate(); err == nil {
		t.Fatal("gid and groupName together were accepted")
	}
	if err := (&User{GID: ID(1)}).Validate(); err != nil {
		t.Fatalf("gid alone rejected: %v", err)
	}
	if err := (*User)(nil).Validate(); err != nil {
		t.Fatalf("nil rejected: %v", err)
	}
}

func TestFieldsString(t *testing.T) {
	if got := Credential.String(); got != "uid, gid, additional groups" {
		t.Fatalf("Credential = %q", got)
	}
	if got := Fields(0).String(); got != "no fields" {
		t.Fatalf("empty = %q", got)
	}
}

func sameID(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func showID(v *int64) string {
	if v == nil {
		return "absent"
	}
	return strconv.FormatInt(*v, 10)
}
