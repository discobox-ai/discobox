package poolruntime

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pkg/pooloutbound"
	poolagent "github.com/discobox-ai/discobox/pool-agent"
	"github.com/discobox-ai/discobox/pool-agent/bundle"
	"github.com/discobox-ai/discobox/pool-agent/provisioner"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// A pool's box is made the way a pool makes a sandbox's (ADR 26-10-24-953
// §§2, 4), whatever the box is: this places the agent and the pool's pins in
// it, has the box make its key and enrolls it, places the bootstrap and the
// pool's bundles, read from the server's own asset store (ADR 26-10-30-875
// §5), and runs `discobox-provision apply`, which applies the pool bundle's
// features — the agent and its proxy among them — fetching nothing. Placing
// and running are the provisioner's, over the box as a machine.Guest: a
// Docker pool's container, or a VM pool's
// machine.

// BoxRecord is what a provider placed in a pool's box, kept with the pool's
// runtime state, the server's own record: a box whose bundles or placement
// moved since is provisioned again, in place. The agent alone moving
// provisions nothing (ADR 26-10-24-953 §5): it is the provisioner, not what
// the pool runs.
type BoxRecord struct {
	Agent   string   `json:"agent,omitempty"`
	Bundles []string `json:"bundles,omitempty"`
	// Placement fingerprints the bootstrap and environment placed: what the
	// provider's configuration and the pool's own settings say of the pool,
	// which move apart from a release (PlacementFingerprint).
	Placement        string `json:"placement,omitempty"`
	OutboundRevision string `json:"outboundRevision,omitempty"`
}

// Current reports whether the record placed placement: its bundles, and its
// bootstrap and environment.
func (r BoxRecord) Current(placement BoxPlacement) (bool, error) {
	fingerprint, err := PlacementFingerprint(placement)
	if err != nil {
		return false, err
	}
	return r.SameBundles(placement.Box) && r.Placement == fingerprint, nil
}

// PlacementFingerprint is what of a placement is not its bundles: the
// bootstrap and the agent's environment.
func PlacementFingerprint(placement BoxPlacement) (string, error) {
	data, err := json.Marshal(struct {
		Bootstrap poolagent.Bootstrap   `json:"bootstrap"`
		Env       map[string]string     `json:"env"`
		Outbound  pooloutbound.Resolved `json:"outbound"`
		// Fence is left out unless set, so a box that is not fenced keeps
		// the fingerprint it was placed with.
		Fence bool `json:"fence,omitempty"`
	}{placement.Bootstrap, placement.Env, placement.Outbound, placement.Fence})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// RestartPoolBuilder drains the builds on the pool's builder and restarts
// BuildKit and the registry, their data and caches kept in place: an outbound
// rotation, which they read only at start. The agent has rendered their new
// configuration and restored egress before, in ProvisionBox's apply of the
// pool's units.
func RestartPoolBuilder(ctx context.Context, guest machine.Guest) error {
	argv := []string{"sh", "-ec", `
resume=/run/discobox/pool-outbound-resume-mediator
if [ -e "$resume" ] || systemctl is-active --quiet discobox-buildkit-mediator.service; then
 umask 077
 : > "$resume"
 trap 'systemctl start discobox-buildkit-mediator.service && rm -f "$resume"' EXIT
 trap 'exit 1' HUP INT TERM
 systemctl stop discobox-buildkit-mediator.service
fi
systemctl try-restart discobox-buildkitd.service discobox-registry.service
if [ -e "$resume" ]; then
 systemctl start discobox-buildkit-mediator.service
 rm -f "$resume"
 trap - EXIT HUP INT TERM
fi
`}
	var stderr strings.Builder
	code, err := machine.Run(ctx, guest, machine.ExecRequest{Argv: argv}, nil, io.Discard, &stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("pool builder restart exited with status %d: %s", code, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// SameBundles reports whether box pins the bundles the record placed.
func (r BoxRecord) SameBundles(box sandbox.PoolBox) bool {
	if len(r.Bundles) != len(box.Bundles) {
		return false
	}
	for i, pin := range box.Bundles {
		if r.Bundles[i] != pin.Name+"@"+pin.Digest {
			return false
		}
	}
	return true
}

// RecordOf is the record of placing placement.
func RecordOf(placement BoxPlacement) (BoxRecord, error) {
	fingerprint, err := PlacementFingerprint(placement)
	if err != nil {
		return BoxRecord{}, err
	}
	record := BoxRecord{Agent: placement.Box.Agent.Name + "@" + placement.Box.Agent.Digest, Placement: fingerprint, OutboundRevision: placement.Outbound.Revision()}
	for _, pin := range placement.Box.Bundles {
		record.Bundles = append(record.Bundles, pin.Name+"@"+pin.Digest)
	}
	return record, nil
}

// BoxFile is a file placed in a pool's box, root's alone: the bootstrap and
// the environment beside it, or what the pool agent reads that the bootstrap
// does not carry, such as a machine pool's provisioning key
// (bundle.ProvisioningKeyFile). It may hold a secret: nothing of its contents
// is put in an error.
type BoxFile struct {
	Path string
	Data []byte
}

// BoxPlacement is what a provider makes a pool's box with: the pool's
// bootstrap and what its box is placed with, and the policy the agent reads
// from its environment (bundle.EnvironmentFile).
type BoxPlacement struct {
	Outbound  pooloutbound.Resolved
	Bootstrap poolagent.Bootstrap
	Box       sandbox.PoolBox
	Env       map[string]string
	// Fence fences the box's network before anything of the pool is placed
	// (FenceBox): a held driver's pool machine's (ADR 26-11-11-963 §6).
	Fence bool
	// HandOver has the box hand over to a new agent and proxy beside the
	// running ones when they move, rather than restart them (bundle.HandOverEnv).
	// It is not in the placement's fingerprint: it changes how a move is
	// applied, and is no move itself.
	HandOver bool
}

// unitsCommand runs the pool-agent feature's units step after an apply, which
// restarts or hands over to what the bundle and placement now say
// (poolunits). A bundle from before it has no such step: its apply restarts
// what moved bundles move, and its agent and proxy, which read the placement
// once at start, are restarted when the placement moved ($1 is "moved"), as
// they were then. What ran in their place is not an agent's subcommand it
// knows.
const unitsCommand = `step="$(dirname "$(readlink -f ` + bundle.AgentBinary + `)")/` + bundle.UnitsStep + `"
if [ -x "$step" ]; then exec "$step"; fi
if [ "$1" = moved ]; then systemctl try-restart discobox-pool-agent.service discobox-proxy.service; fi`

// Environment is what apply and the units step run with: the outbound
// routing's, and whether the box hands over.
func (p BoxPlacement) Environment() []string {
	env := p.Outbound.Environment()
	if p.HandOver {
		env = append(env, bundle.HandOverEnv+"=1")
	}
	return env
}

// provisionTimeout bounds one provisioning of a pool's box: placing the pool
// bundle and every one of its steps, blocking.
const provisionTimeout = 15 * time.Minute

// ProvisionBox makes the running box guest a pool's box, or makes it so
// again: it fences the box's network first when the placement asks
// (FenceBox), and fails before placing anything else when that does not
// hold; it places the agent and pins, enrolls the key the box makes or
// already holds, places the bootstrap and the agent's environment with every
// blob of the pool's bundles (applyTree), and applies those bundles, which
// the box fetches nothing for (ADR 26-10-30-875 §5). What else the agent
// reads (PlaceBoxFiles) is placed before. Every step is idempotent, so a box
// provisioned before is provisioned again in place, its key the same. placed
// is the record of what the box was placed with before, the zero record for
// a box never placed: a bundle from before slots restarts its agent only
// when the placement moved from it (unitsCommand).
func ProvisionBox(ctx context.Context, guest machine.Guest, maker PoolMaker, placement BoxPlacement, placed BoxRecord) (BoxRecord, error) {
	facts, reportErr := sandbox.ReportBootstrap(ctx, model.BootstrapEvidence{Operation: "provision_pool_box", Bounded: true})
	if reportErr != nil {
		return BoxRecord{}, reportErr
	}
	if facts != nil {
		if reason := facts.Expired(time.Now()); reason != "" {
			return BoxRecord{}, fmt.Errorf("bootstrap provision_pool_box: %s", reason)
		}
		var stop context.CancelFunc
		ctx, stop = context.WithDeadline(ctx, facts.Allowance())
		defer stop()
	}

	ctx, cancel := context.WithTimeout(ctx, provisionTimeout)
	defer cancel()
	if placement.Fence {
		if err := FenceBox(ctx, guest); err != nil {
			return BoxRecord{}, err
		}
	}
	bootstrap, box := placement.Bootstrap, placement.Box
	agentDir, err := os.MkdirTemp("", "discobox-pool-agent-")
	if err != nil {
		return BoxRecord{}, err
	}
	defer func() { _ = os.RemoveAll(agentDir) }()
	agentPath := filepath.Join(agentDir, "discobox-sandbox-agent")
	layer, _, err := box.OpenBlob(ctx, box.AgentLayer)
	if err != nil {
		return BoxRecord{}, fmt.Errorf("read the agent bundle %s: %w", box.Agent.Name, err)
	}
	err = agentlayer.ExtractAgent(layer, agentPath)
	layer.Close()
	if err != nil {
		return BoxRecord{}, fmt.Errorf("the agent bundle %s@%s: %w", box.Agent.Name, box.Agent.Digest, err)
	}
	info, err := os.Stat(agentPath)
	if err != nil {
		return BoxRecord{}, err
	}
	pins := make(agentlayer.Pins, 0, len(box.Bundles))
	for _, pin := range box.Bundles {
		pins = append(pins, agentlayer.Pin{Name: pin.Name, Digest: pin.Digest})
	}
	agent := agentlayer.Agent{
		Digest: box.AgentLayer,
		Size:   info.Size(),
		Open:   func() (io.ReadCloser, error) { return os.Open(agentPath) },
	}
	var agentTree bytes.Buffer
	if err := agentlayer.Archive(&agentTree, agent, false, pins, nil); err != nil {
		return BoxRecord{}, err
	}
	// Every unit the pool bundle lays is systemd's: a base without it is
	// refused before anything is placed, naming why, rather than provisioned into a box whose
	// agent never starts (ADR 26-11-16-619 §4).
	if err := provisioner.RequireSystemd(ctx, guest); err != nil {
		return BoxRecord{}, fmt.Errorf("the pool's box: %w", err)
	}
	keyPath := layout.Container().PoolIdentityKey(bootstrap.ProjectID, bootstrap.PoolID)
	identity, err := provisioner.Provision(ctx, guest, provisioner.Plan{
		Dir:     "/",
		Tree:    &agentTree,
		Command: machine.ExecRequest{Argv: []string{agentlayer.AgentPath, "provision", "identity", "--key", keyPath}, Env: placement.Outbound.Environment()},
	})
	if err != nil {
		return BoxRecord{}, placement.Outbound.RedactError(provisionError("make the pool's identity", identity, err))
	}
	publicKey := strings.TrimSpace(string(identity.Stdout))
	if publicKey == "" {
		return BoxRecord{}, errors.New("the pool's box answered no identity")
	}
	if err := maker.Enroll(ctx, publicKey); err != nil {
		return BoxRecord{}, fmt.Errorf("enroll the pool's identity: %w", err)
	}
	files, err := bootstrapFiles(placement)
	if err != nil {
		return BoxRecord{}, err
	}
	outboundFiles, err := placement.Outbound.Files()
	if err != nil {
		return BoxRecord{}, err
	}
	for name, data := range outboundFiles {
		files = append(files, BoxFile{Path: name, Data: data})
	}
	tree := applyTree(ctx, files, box)
	applied, err := provisioner.Provision(ctx, guest, provisioner.Plan{
		Dir:     "/",
		Tree:    tree,
		Command: machine.ExecRequest{Argv: []string{agentlayer.AgentPath, "provision", "apply", "--wait"}, Env: placement.Environment()},
	})
	_ = tree.Close()
	if err != nil {
		return BoxRecord{}, placement.Outbound.RedactError(provisionError("provision the pool's box", applied, err))
	}
	// The bundle's own apply runs the pool's units when they or the agent
	// moved. A placement that moved alone, or a box whose bundle was applied
	// before any bootstrap was placed, takes effect here: the command moves
	// nothing that has not moved (poolunits).
	record, err := RecordOf(placement)
	if err != nil {
		return BoxRecord{}, err
	}
	moved := "kept"
	if placed.Placement != "" && placed.Placement != record.Placement {
		moved = "moved"
	}
	units, err := provisioner.Run(ctx, guest, machine.ExecRequest{Argv: []string{"sh", "-ec", unitsCommand, "units", moved}, Env: placement.Environment()}, nil)
	if err != nil {
		return BoxRecord{}, provisionError("start the pool's agent", units, err)
	}
	if _, err := sandbox.ReportBootstrap(ctx, model.BootstrapEvidence{Operation: "provision_pool_box", Complete: true, Registration: true}); err != nil {
		return BoxRecord{}, err
	}
	return record, nil
}

// PlaceBoxFiles places files in a running pool's box and runs nothing: what
// the agent reads again on its own, such as a renewed provisioning key, which
// its engine reads on every call.
func PlaceBoxFiles(ctx context.Context, guest machine.Guest, files ...BoxFile) error {
	tree, err := boxTree(files)
	if err != nil {
		return err
	}
	return guest.CopyTo(ctx, "/", tree)
}

// provisionError names what failed with what the command wrote last, which
// is where it says why.
func provisionError(what string, result provisioner.Result, err error) error {
	if out := strings.TrimSpace(string(result.Stderr)); out != "" {
		return fmt.Errorf("%s: %w: %s", what, err, lastLines(out, 20))
	}
	return fmt.Errorf("%s: %w", what, err)
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// bootstrapFiles is what the pool agent reads beside its bundle: the
// bootstrap (bundle.BootstrapFile) and the policy it reads from its
// environment (bundle.EnvironmentFile).
func bootstrapFiles(placement BoxPlacement) ([]BoxFile, error) {
	data, err := json.MarshalIndent(placement.Bootstrap, "", "  ")
	if err != nil {
		return nil, err
	}
	env := make([]string, 0, len(placement.Env))
	for _, entry := range placement.Outbound.Environment()[1:] {
		key, value, _ := strings.Cut(entry, "=")
		env = append(env, key+"="+strconv.Quote(value)+"\n")
	}
	for key, value := range placement.Env {
		env = append(env, key+"="+value+"\n")
	}
	sort.Strings(env)
	return []BoxFile{
		{Path: bundle.BootstrapFile, Data: data},
		{Path: bundle.EnvironmentFile, Data: []byte(strings.Join(env, ""))},
	}, nil
}

// applyTree is the tree `discobox-provision apply` applies: files, and every
// blob of box's bundles the box's content store reads (box.Blobs), each at
// agentlayer.PlacedBlob, read from the server's asset store as it is written
// (ADR 26-10-30-875 §5). It streams: the caller closes it once it is placed,
// or given up.
func applyTree(ctx context.Context, files []BoxFile, box sandbox.PoolBox) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		tw := tar.NewWriter(writer)
		err := writeBoxFiles(tw, files)
		if err == nil {
			err = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: strings.TrimPrefix(agentlayer.PlacedDir, "/") + "/", Mode: 0o700})
		}
		for _, digest := range box.Blobs {
			if err != nil {
				break
			}
			err = placeBlob(ctx, tw, box, digest)
		}
		if err == nil {
			err = tw.Close()
		}
		writer.CloseWithError(err)
	}()
	return reader
}

// placeBlob writes the blob digest of box at agentlayer.PlacedBlob.
func placeBlob(ctx context.Context, tw *tar.Writer, box sandbox.PoolBox, digest string) error {
	body, size, err := box.OpenBlob(ctx, digest)
	if err != nil {
		return fmt.Errorf("read %s of the pool's bundles: %w", digest, err)
	}
	defer body.Close()
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: strings.TrimPrefix(agentlayer.PlacedBlob(digest), "/"), Mode: 0o600, Size: size}); err != nil {
		return err
	}
	if _, err := io.CopyN(tw, body, size); err != nil {
		return fmt.Errorf("read %s of the pool's bundles: %w", digest, err)
	}
	return nil
}

// boxTree is files as a tar to place at "/", each root's alone, under the
// directories they name.
func boxTree(files []BoxFile) (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := writeBoxFiles(tw, files); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return &buf, nil
}

// writeBoxFiles writes files to tw, each root's alone, under the directories
// they name.
func writeBoxFiles(tw *tar.Writer, files []BoxFile) error {
	dirs := map[string]bool{}
	for _, file := range files {
		if !path.IsAbs(file.Path) {
			return fmt.Errorf("a pool's box file %q is not an absolute path", file.Path)
		}
		if dir := path.Dir(file.Path); !dirs[dir] {
			dirs[dir] = true
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: strings.TrimPrefix(dir, "/") + "/", Mode: 0o755}); err != nil {
				return err
			}
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: strings.TrimPrefix(file.Path, "/"), Mode: 0o600, Size: int64(len(file.Data))}); err != nil {
			return err
		}
		if _, err := tw.Write(file.Data); err != nil {
			return err
		}
	}
	return nil
}
