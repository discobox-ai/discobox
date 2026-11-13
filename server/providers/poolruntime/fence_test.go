package poolruntime

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// fenceGuest is a pool machine as the fence step sees one: it records every
// command it runs and every file placed, answers the step's two commands as a
// base would, and lists back what listing says, or else what was placed.
type fenceGuest struct {
	machine.Guest

	mu     sync.Mutex
	events []string
	files  map[string]string
	// uplinks is what the machine names as its devices; uplinkErr, when
	// set, is what the device command writes before it fails.
	uplinks   string
	uplinkErr string
	// listing, when set, is what nft lists back instead of what was placed.
	listing *string
}

func (g *fenceGuest) Exec(_ context.Context, req machine.ExecRequest) (machine.Process, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.events = append(g.events, "exec "+strings.Join(req.Argv, " "))
	script := ""
	if len(req.Argv) == 3 && req.Argv[0] == "sh" {
		script = req.Argv[2]
	}
	switch script {
	case uplinkCommand:
		if g.uplinkErr != "" {
			return &doneProcess{stderr: g.uplinkErr, code: 1}, nil
		}
		return &doneProcess{stdout: g.uplinks}, nil
	case loadFenceCommand:
		if g.listing != nil {
			return &doneProcess{stdout: *g.listing}, nil
		}
		_, table, _ := strings.Cut(g.files[FenceRulesFile], "delete table inet "+fenceTable+"\n")
		return &doneProcess{stdout: table}, nil
	}
	return &doneProcess{stderr: "unexpected command", code: 127}, nil
}

func (g *fenceGuest) CopyTo(_ context.Context, dir string, stream io.Reader) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.files == nil {
		g.files = map[string]string{}
	}
	tr := tar.NewReader(stream)
	var placed []string
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		name := path.Join(dir, header.Name) // a guest path, slash-separated on every host
		g.files[name] = string(data)
		placed = append(placed, name)
	}
	g.events = append(g.events, "copy "+strings.Join(placed, " "))
	return nil
}

func (g *fenceGuest) recorded() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.events...)
}

type doneProcess struct {
	stdout, stderr string
	code           int
}

func (p *doneProcess) Write(b []byte) (int, error) { return len(b), nil }
func (p *doneProcess) CloseStdin() error           { return nil }
func (p *doneProcess) Resize(uint16, uint16) error { return nil }
func (p *doneProcess) Close() error                { return nil }
func (p *doneProcess) Wait(stdout, stderr io.Writer) (int, error) {
	_, _ = io.WriteString(stdout, p.stdout)
	_, _ = io.WriteString(stderr, p.stderr)
	return p.code, nil
}

// The fence places the rule set and the pool units' drop-in and nothing
// else, naming each device the machine has, and loads and reads it back
// (ADR 26-11-11-963 §6).
func TestFenceBoxPlacesTheRuleSetAndDropInAndReadsItBack(t *testing.T) {
	guest := &fenceGuest{uplinks: "eth0\nenp0s2\n"}
	if err := FenceBox(context.Background(), guest); err != nil {
		t.Fatal(err)
	}
	events := guest.recorded()
	want := []string{
		"exec sh -ec " + uplinkCommand,
		"copy " + FenceRulesFile + " " + FenceDropIn,
		"exec sh -ec " + loadFenceCommand,
	}
	if strings.Join(events, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the fence ran\n%s\nwant\n%s", strings.Join(events, "\n"), strings.Join(want, "\n"))
	}
	rules := guest.files[FenceRulesFile]
	for _, dev := range []string{"eth0", "enp0s2"} {
		if !strings.Contains(rules, `iifname "`+dev+`" drop`) {
			t.Errorf("the rule set does not fence %s:\n%s", dev, rules)
		}
	}
	dropIn := guest.files[FenceDropIn]
	if !strings.Contains(dropIn, "Requires=nftables.service") || !strings.Contains(dropIn, "After=nftables.service") {
		t.Errorf("the drop-in does not hold a pool unit to nftables.service:\n%s", dropIn)
	}
}

// A rule set the kernel does not hold as written fails the box, and nothing
// of the pool is placed in it: not the agent, not its bundles.
func TestProvisionBoxPlacesNothingWhenTheFenceDoesNotReadBack(t *testing.T) {
	for name, guest := range map[string]*fenceGuest{
		"read back differs": {uplinks: "eth0\n", listing: new("table inet discobox_fence {\n}\n")},
		"no nft":            {uplinkErr: "the machine's base has no nft"},
		"no device":         {uplinks: "\n"},
		"strange device":    {uplinks: "eth0\"; flush ruleset\n"},
	} {
		t.Run(name, func(t *testing.T) {
			box := sandbox.PoolBox{
				AgentLayer: "sha256:" + strings.Repeat("1", 64),
				OpenBlob: func(context.Context, string) (io.ReadCloser, int64, error) {
					t.Error("the agent bundle was read for a box that is not fenced")
					return nil, 0, errors.New("unreachable")
				},
			}
			_, err := ProvisionBox(context.Background(), guest, nil, BoxPlacement{Box: box, Fence: true})
			if err == nil {
				t.Fatal("a box whose fence does not hold was provisioned")
			}
			t.Log(err)
			for _, event := range guest.recorded() {
				if strings.HasPrefix(event, "copy ") && event != "copy "+FenceRulesFile+" "+FenceDropIn {
					t.Errorf("placed in a box that is not fenced: %s", event)
				}
			}
		})
	}
}

// The fence runs first: the agent bundle is read only once it reads back.
func TestProvisionBoxFencesBeforeAnythingElse(t *testing.T) {
	guest := &fenceGuest{uplinks: "eth0\n"}
	read := false
	box := sandbox.PoolBox{
		AgentLayer: "sha256:" + strings.Repeat("1", 64),
		OpenBlob: func(context.Context, string) (io.ReadCloser, int64, error) {
			read = true
			if events := guest.recorded(); len(events) != 3 {
				t.Errorf("the agent bundle was read after %v, before the fence held", events)
			}
			return nil, 0, errors.New("stop here")
		},
	}
	if _, err := ProvisionBox(context.Background(), guest, nil, BoxPlacement{Box: box, Fence: true}); err == nil || !read {
		t.Fatalf("provision went on past the agent bundle, or never read it: %v", err)
	}
}

// Fencing a box moves its fingerprint, so a box placed before it is fenced
// is provisioned again; one that is not fenced keeps the fingerprint it was
// placed with (Fence is omitted), so no Docker or boxd pool is.
func TestPlacementFingerprintFollowsTheFence(t *testing.T) {
	plain, err := PlacementFingerprint(BoxPlacement{Env: map[string]string{"A": "b"}})
	if err != nil {
		t.Fatal(err)
	}
	fenced, err := PlacementFingerprint(BoxPlacement{Env: map[string]string{"A": "b"}, Fence: true})
	if err != nil {
		t.Fatal(err)
	}
	if plain == fenced {
		t.Fatal("fencing a box does not move its fingerprint, so a box fenced since is never provisioned again")
	}
}

// The rule set loads in a real nft, again over itself, and lists back as it
// is written: what FenceBox's read back compares. Run where this host can
// make a network namespace of its own.
func TestFenceRulesListBackFromNft(t *testing.T) {
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("no nft on this host")
	}
	if err := exec.Command("unshare", "-rn", "true").Run(); err != nil {
		t.Skipf("cannot make a network namespace here: %v", err)
	}
	rules := fenceRules([]string{"eth0", "enp0s1", "ens3"})
	file := filepath.Join(t.TempDir(), "fence.nft")
	if err := os.WriteFile(file, []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	cmd := exec.Command("unshare", "-rn", "sh", "-ec", `nft -f "$1"; nft -f "$1"; nft list table inet `+fenceTable, "sh", file)
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("nft: %v: %s", err, stderr.String())
	}
	_, want, _ := strings.Cut(rules, "delete table inet "+fenceTable+"\n")
	if !sameRules(out.String(), want) {
		t.Fatalf("nft lists\n%s\nwhere the rule set says\n%s", out.String(), want)
	}
}
