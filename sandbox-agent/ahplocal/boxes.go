// Package ahplocal is what the AHP host in a discobox (`discobox-sandbox-agent
// ahp`, ADR 26-10-22-853) needs of that discobox: its execs, through the
// sandbox agent's local exec socket (Boxes), and its filesystem and git, as
// the user the host runs as (Workspace).
package ahplocal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/discobox-ai/discobox/pkg/ahphost"
	"github.com/discobox-ai/discobox/sandbox-agent/localexec"
)

var _ ahphost.Boxes = (*Boxes)(nil)

// Boxes is the one discobox this runs in, as the host sees discoboxes,
// over the sandbox agent's local exec socket.
type Boxes struct {
	client  *localexec.Client
	started time.Time
}

// NewBoxes is this discobox, reached through the local exec socket's client.
func NewBoxes(client *localexec.Client) *Boxes {
	return &Boxes{client: client, started: time.Now()}
}

// errOneBox refuses what only a host across discoboxes can do.
var errOneBox = errors.New("this host serves only the discobox it runs in; use `discobox admin ahp` to create, archive or rename discoboxes")

func (b *Boxes) List(ctx context.Context) (ahphost.Listing, error) {
	box, err := b.client.Box(ctx)
	if err != nil {
		return ahphost.Listing{}, err
	}
	out := ahphost.Box{
		ID:          box.SandboxID,
		Name:        boxName(box),
		State:       ahphost.BoxRunning,
		Harness:     box.Harness,
		HarnessName: box.HarnessName,
		Source:      box.Workdir,
		DiffBase:    box.DiffBase,
		Created:     b.started,
	}
	if box.Agent != nil {
		out.Agent = *box.Agent
	}
	return ahphost.Listing{Boxes: []ahphost.Box{out}}, nil
}

// boxName is what the session is titled. The discobox's own name is the
// control plane's and is not kept in it; the folder it works in is what VS
// Code's window is about.
func boxName(box localexec.Box) string {
	if box.Workdir != "" && box.Workdir != "/" {
		return filepath.Base(box.Workdir)
	}
	return box.SandboxID
}

func (b *Boxes) Harnesses(ctx context.Context) ([]ahphost.Harness, error) {
	box, err := b.client.Box(ctx)
	if err != nil || box.Harness == "" {
		return nil, err
	}
	return []ahphost.Harness{{Slug: box.Harness, Name: box.HarnessName}}, nil
}

func (b *Boxes) Create(context.Context, ahphost.CreateRequest, func(string)) (ahphost.Box, error) {
	return ahphost.Box{}, errOneBox
}

// Do starts the discobox, which is running, since this runs in it.
func (b *Boxes) Do(_ context.Context, verb ahphost.Verb, _ string) error {
	if verb == ahphost.VerbStart {
		return nil
	}
	return errOneBox
}

func (b *Boxes) Rename(context.Context, string, string) error { return errOneBox }

func (b *Boxes) Execs(ctx context.Context, _ string) ([]ahphost.Exec, error) {
	execs, err := b.client.Execs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ahphost.Exec, 0, len(execs))
	for _, exec := range execs {
		out = append(out, ahpExec(exec))
	}
	return out, nil
}

func ahpExec(exec localexec.Exec) ahphost.Exec {
	return ahphost.Exec{ID: exec.ID, Harness: exec.Harness, Service: exec.Service, Primary: exec.Primary, Live: exec.Live}
}

func (b *Boxes) OpenExec(ctx context.Context, _, execID string, cols, rows int) (ahphost.Stream, error) {
	term, err := b.client.Attach(ctx, execID)
	if err != nil {
		return nil, err
	}
	if err := term.Resize(cols, rows); err != nil {
		_ = term.Close()
		return nil, err
	}
	return term, nil
}

func (b *Boxes) NewShell(ctx context.Context, _ string, cols, rows int) (ahphost.Exec, ahphost.Stream, error) {
	return b.newExec(ctx, localexec.CreateRequest{Shell: true, Cols: cols, Rows: rows})
}

func (b *Boxes) NewTerminal(ctx context.Context, _ string, cols, rows int) (ahphost.Exec, ahphost.Stream, error) {
	return b.newExec(ctx, localexec.CreateRequest{Cols: cols, Rows: rows})
}

// newExec creates an exec, attaches, and only then starts it, so nothing it
// prints first is lost.
func (b *Boxes) newExec(ctx context.Context, req localexec.CreateRequest) (ahphost.Exec, ahphost.Stream, error) {
	exec, err := b.client.Create(ctx, req)
	if err != nil {
		return ahphost.Exec{}, nil, err
	}
	term, err := b.client.Attach(ctx, exec.ID)
	if err != nil {
		_ = b.client.Delete(context.WithoutCancel(ctx), exec.ID)
		return ahphost.Exec{}, nil, err
	}
	if err := b.client.Start(ctx, exec.ID); err != nil {
		_ = term.Close()
		_ = b.client.Delete(context.WithoutCancel(ctx), exec.ID)
		return ahphost.Exec{}, nil, err
	}
	exec.Live = true
	return ahpExec(exec), term, nil
}

func (b *Boxes) EndExec(ctx context.Context, _, execID string) error {
	return b.client.Delete(ctx, execID)
}

func (b *Boxes) SendInput(ctx context.Context, _, execID, text string) error {
	return b.client.Input(ctx, execID, text)
}

// PutImage writes a pasted image where the CLI's copy of one goes.
func (b *Boxes) PutImage(_ context.Context, _, name string, data []byte) (string, error) {
	dir := filepath.Join(os.TempDir(), "discobox-paste")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, filepath.Base(name))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}
