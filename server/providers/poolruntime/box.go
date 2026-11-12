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

// RestartPoolUnits first waits for the agent to render consumers' configuration
// and restore egress. An outbound rotation then drains builds before restarting
// the builder and registry; their data and caches stay in place.
func RestartPoolUnits(ctx context.Context, guest machine.Guest, restartBuilder bool) error {
	commands := [][]string{append([]string{"systemctl", "try-restart"}, bundle.BootstrapUnits...)}
	if restartBuilder {
		commands = append(commands, []string{"sh", "-ec", `
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
`})
	}
	for _, argv := range commands {
		var stderr strings.Builder
		code, err := machine.Run(ctx, guest, machine.ExecRequest{Argv: argv}, nil, io.Discard, &stderr)
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("pool consumer restart exited with status %d: %s", code, strings.TrimSpace(stderr.String()))
		}
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
// provisioned before is provisioned again in place, its key the same.
func ProvisionBox(ctx context.Context, guest machine.Guest, maker PoolMaker, placement BoxPlacement) (BoxRecord, error) {
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
		Command: machine.ExecRequest{Argv: []string{agentlayer.AgentPath, "provision", "apply", "--wait"}, Env: placement.Outbound.Environment()},
	})
	_ = tree.Close()
	if err != nil {
		return BoxRecord{}, placement.Outbound.RedactError(provisionError("provision the pool's box", applied, err))
	}
	if _, err := sandbox.ReportBootstrap(ctx, model.BootstrapEvidence{Operation: "provision_pool_box", Complete: true, Registration: true}); err != nil {
		return BoxRecord{}, err
	}
	return RecordOf(placement)
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
