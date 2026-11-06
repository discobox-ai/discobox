package provision

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

func TestPackagesSatisfiedAndRepairDrift(t *testing.T) {
	b := newBox(t)
	b.feature("f", map[string]string{provisionspec.FileName: `apiVersion: discobox.ai/provision/v1
steps:
  - name: packages
    packages: {debian: [git, jq]}
`})
	installed := map[string]bool{"git": true, "jq": true}
	var apt []string
	b.runner.Command = func(_ context.Context, out io.Writer, _ string, _ []string, tool string, args ...string) error {
		switch tool {
		case "dpkg-query":
			if args[0] != "--admindir="+b.root+"/var/lib/dpkg" {
				t.Fatalf("query escaped box: %v", args)
			}
			if !installed[args[len(args)-1]] {
				return errors.New("missing")
			}
			_, err := fmt.Fprint(out, "install ok installed\t1.0\n")
			return err
		case "dpkg":
			return nil // Model recovery without touching the host database.
		case "apt-get":
			apt = append(apt, args[0])
			if args[0] == "install" {
				installed["jq"] = true
			}
			return nil
		default:
			t.Fatalf("unexpected command %s", tool)
			return nil
		}
	}
	b.mustApply()
	b.mustApply()
	if len(apt) != 0 {
		t.Fatalf("satisfied packages used apt: %v", apt)
	}
	if _, ok, err := b.runner.Records.Step("f", "packages"); !ok || err != nil {
		t.Fatalf("successful step not recorded: %v", err)
	}
	delete(installed, "jq")
	b.mustApply()
	if !slices.Equal(apt, []string{"update", "install"}) {
		t.Fatalf("removed package was not repaired: %v", apt)
	}
	apt = nil
	b.mustApply()
	if len(apt) != 0 {
		t.Fatalf("repaired packages used apt: %v", apt)
	}
}

func TestPackageSatisfactionRequiresInstalledStatusAndVersion(t *testing.T) {
	for _, tc := range []struct {
		name, requirement, output string
		want                      bool
	}{
		{"installed", "git", "install ok installed\t1.0\n", true},
		{"held", "git", "hold ok installed\t1.0\n", true},
		{"reinstall required", "git", "install reinstreq installed\t1.0\n", false},
		{"partial", "git", "install ok unpacked\t1.0\n", false},
		{"removed", "git", "deinstall ok config-files\t1.0\n", false},
		{"half configured", "git", "install ok half-configured\t1.0\n", false},
		{"ambiguous", "git", "install ok installed\t1.0\ninstall ok installed\t2.0\n", false},
		{"empty", "git", "", false},
		{"pinned", "git=1.0", "install ok installed\t1.0\n", true},
		{"equivalent version", "git=1.0-0", "install ok installed\t1.0\n", true},
		{"wrong version", "git=2.0", "install ok installed\t1.0\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := exec.LookPath("dpkg"); err != nil {
				t.Skip("needs dpkg version comparison")
			}
			b := newBox(t)
			b.runner.Command = func(ctx context.Context, out io.Writer, dir string, env []string, tool string, args ...string) error {
				if tool == "dpkg-query" {
					if args[len(args)-1] != strings.Split(tc.requirement, "=")[0] {
						t.Fatal(args)
					}
					_, err := io.WriteString(out, tc.output)
					return err
				}
				return runCommand(ctx, out, dir, env, tool, args...)
			}
			if got := b.runner.packagesSatisfied(context.Background(), []string{tc.requirement}); got != tc.want {
				t.Fatalf("satisfied=%v want %v", got, tc.want)
			}
		})
	}
}
