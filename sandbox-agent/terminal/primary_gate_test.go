package terminal

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/discobox-ai/discobox/sandbox-agent/config"
)

func TestPrimaryGateCreationAndRevival(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux source hooks")
	}
	for _, shell := range []bool{false, true} {
		name := "override"
		if shell {
			name = "shell"
		}
		t.Run(name, func(t *testing.T) {
			svc := newReviveService(t, config.Harness{ID: "custom", Command: []string{"custom-agent"}}).svc
			svc.harness = config.Harness{ID: "custom", Command: []string{"custom-agent", "--launch"}, RelaunchCommand: []string{"custom-agent", "--resume"}}
			if shell {
				svc.harness = config.Harness{ID: "registered-shell"}
			}
			root, err := svc.execs.SourceRoot()
			if err != nil {
				t.Fatal(err)
			}
			scripts := filepath.Join(root, ".discobox", "scripts")
			if err := os.MkdirAll(scripts, 0755); err != nil {
				t.Fatal(err)
			}
			// Even an invalid entry belongs in the foreground failure flow.
			if err := os.WriteFile(filepath.Join(scripts, "pre-harness"), []byte("invalid"), 0644); err != nil {
				t.Fatal(err)
			}
			subdir := filepath.Join(root, "subdir")
			if err := os.Mkdir(subdir, 0755); err != nil {
				t.Fatal(err)
			}
			ex, err := svc.Create(context.Background(), CreateRequest{primary: true, Workdir: subdir, Args: []string{"literal $(prompt)"}})
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"discobox-primary-run", root}
			if !shell {
				want = append(want, "custom-agent", "--launch", "literal $(prompt)")
			}
			if !slices.Equal(ex.StartupCommand, want) {
				t.Fatalf("startup %q, want %q", ex.StartupCommand, want)
			}
			aux, err := svc.Create(context.Background(), CreateRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if len(aux.StartupCommand) > 0 && aux.StartupCommand[0] == "discobox-primary-run" {
				t.Fatal("auxiliary terminal ran gate")
			}
			markExited(t, svc, ex.ID)
			revived, err := svc.Revive(context.Background(), ex.ID)
			if err != nil {
				t.Fatal(err)
			}
			want = []string{"discobox-primary-run", root}
			if !shell {
				want = append(want, "custom-agent", "--resume")
			}
			if !slices.Equal(revived.StartupCommand, want) {
				t.Fatalf("revive %q, want %q", revived.StartupCommand, want)
			}
		})
	}
}
