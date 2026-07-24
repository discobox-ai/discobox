package boot

import (
	"slices"
	"testing"
)

// fakeGroups backs a booter's lookup/run for ensureAdditionalGroups tests: the
// set of groups getent would report present, and the run calls recorded.
type fakeGroups struct {
	present map[string]bool
	runs    [][]string
}

func newFakeGroupsBooter(present ...string) (*booter, *fakeGroups) {
	f := &fakeGroups{present: map[string]bool{}}
	for _, g := range present {
		f.present[g] = true
	}
	return &booter{
		run: func(name string, args ...string) error {
			f.runs = append(f.runs, append([]string{name}, args...))
			return nil
		},
		lookup: func(name string, args ...string) (string, bool) {
			if name == "getent" && len(args) == 2 && args[0] == "group" {
				return args[1] + ":x:1:", f.present[args[1]]
			}
			return "", false
		},
	}, f
}

func TestEnsureAdditionalGroupsAddsPresentGroups(t *testing.T) {
	b, f := newFakeGroupsBooter("docker")
	if err := b.ensureAdditionalGroups(identity{uid: 1000, name: "dev"}, []string{"docker"}); err != nil {
		t.Fatalf("ensureAdditionalGroups: %v", err)
	}
	want := []string{"usermod", "--append", "--groups", "docker", "dev"}
	if len(f.runs) != 1 || !slices.Equal(f.runs[0], want) {
		t.Fatalf("runs = %v, want [%v]", f.runs, want)
	}
}

func TestEnsureAdditionalGroupsSkipsMissingGroup(t *testing.T) {
	b, f := newFakeGroupsBooter() // "docker" group doesn't exist on this image
	if err := b.ensureAdditionalGroups(identity{uid: 1000, name: "dev"}, []string{"docker"}); err != nil {
		t.Fatalf("ensureAdditionalGroups: %v", err)
	}
	if len(f.runs) != 0 {
		t.Fatalf("runs = %v, want none: a missing group must not fail boot", f.runs)
	}
}

func TestEnsureAdditionalGroupsSkipsRoot(t *testing.T) {
	b, f := newFakeGroupsBooter("docker")
	if err := b.ensureAdditionalGroups(identity{uid: 0, name: "root"}, []string{"docker"}); err != nil {
		t.Fatalf("ensureAdditionalGroups: %v", err)
	}
	if len(f.runs) != 0 {
		t.Fatalf("runs = %v, want none: root needs no supplementary groups", f.runs)
	}
}

func TestEnsureAdditionalGroupsNoGroups(t *testing.T) {
	b, f := newFakeGroupsBooter("docker")
	if err := b.ensureAdditionalGroups(identity{uid: 1000, name: "dev"}, nil); err != nil {
		t.Fatalf("ensureAdditionalGroups: %v", err)
	}
	if len(f.runs) != 0 {
		t.Fatalf("runs = %v, want none", f.runs)
	}
}
