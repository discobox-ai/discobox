package bridge

import (
	"context"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

// contractEnv names the scenario TestContractCore serves. The desktop app's
// contract suite (desktop/bridge/tests/contract.rs) builds this package's
// test binary and runs it with this set, one process per test, as the CLI
// end of a real pipe (ADR 26-11-07-289 §4).
const contractEnv = "DISCOBOX_BRIDGE_CONTRACT"

// TestContractCore is the CLI's side of the desktop app's contract suite: the
// real Server, over a fake of the core, on this process's stdin and stdout.
// Run any other way, it is skipped.
//
// What the core does is the boxes' to say, so the app can reach each case
// with a command:
//
//   - b1 is running, with a shell; a credential request waits on it, and
//     its settings are its name. It declares three services: web, running;
//     migrate, which ran and exited with what it wrote; and broken, which
//     cannot run.
//   - b2 is archived: purging it asks first, and a purge lets slow's stop
//     finish.
//   - b3 is stopped: starting it reports a step and asks a question with
//     choices, and, in the "early" scenario, lets every open attach.
//   - slow is running: stopping it waits on a purge.
//   - doomed is running: stopping it ends this process, with status 3, as a
//     CLI that exits mid-command.
//
// Archiving and unarchiving a box changes its state in the listing. Every
// terminal echoes what it is sent (fakeTerm).
func TestContractCore(t *testing.T) {
	scenario := os.Getenv(contractEnv)
	if scenario == "" {
		t.Skip("the CLI's end of the desktop app's contract suite; run by desktop/bridge/tests/contract.rs")
	}
	// The pipe is the real stdout; anything the testing package prints goes
	// to stderr, where it cannot be read as a frame.
	out := os.Stdout
	os.Stdout = os.Stderr

	core := newFakeCore("home",
		running("b1"),
		console.Sandbox{ID: "b2", Name: "b2", State: console.StateArchived},
		console.Sandbox{ID: "b3", Name: "b3", State: console.StateStopped},
		running("slow"),
		running("doomed"),
	)
	core.echo = true
	core.settings = console.BoxSettings{Name: "b1"}
	core.execs = []console.Exec{{ID: "sh", Command: []string{"/bin/bash"}, Live: true, Tty: true, CreatedAt: time.Unix(2, 0)}}
	core.requests = []console.CredentialRequest{{ID: "r1", SandboxID: "b1", Name: "github"}}
	core.services = []console.Service{
		{ID: "web", Status: "running", ExecID: "web"},
		{ID: "migrate", Status: "exited"},
		{ID: "broken", Status: "stopped", Problem: "it is not executable"},
	}
	core.serviceLogs = map[string]string{"migrate": "migrated 3 tables\n"}
	switch scenario {
	case "early":
		core.gate = make(chan struct{})
	case "default":
	default:
		t.Fatalf("%s=%q: no such scenario", contractEnv, scenario)
	}
	release := sync.OnceFunc(func() {
		if core.gate != nil {
			close(core.gate)
		}
	})
	purged := make(chan struct{})
	purge := sync.OnceFunc(func() { close(purged) })
	core.do = func(ctx context.Context, verb console.Verb, id string, n *console.Narration) (string, error) {
		switch {
		case verb == console.VerbStart && id == "b3":
			release()
			n.Report("pulling")
			key, err := n.Ask(ctx, console.Question{Title: "Git credentials", Choices: []console.Choice{{Key: "once", Label: "once"}, {Key: "never", Label: "never"}}})
			if err != nil {
				return "", err
			}
			return "chose " + key, nil
		case verb == console.VerbStop && id == "slow":
			select {
			case <-purged:
				return "stopped after the purge", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		case verb == console.VerbStop && id == "doomed":
			os.Exit(3)
		case verb == console.VerbPurge:
			purge()
		case verb == console.VerbArchive || verb == console.VerbUnarchive:
			state := console.StateArchived
			if verb == console.VerbUnarchive {
				state = console.StateStopped
			}
			// A new listing: the server holds the last one it was handed.
			listing, _ := core.List(ctx)
			boxes := slices.Clone(listing.Sandboxes)
			for i := range boxes {
				if boxes[i].ID == id {
					boxes[i].State = state
				}
			}
			core.setListing(boxes...)
		}
		return "", nil
	}

	if err := NewServer(core, out).Serve(context.Background(), os.Stdin); err != nil {
		t.Fatal(err)
	}
}
