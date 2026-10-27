package terminal

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/discobox-ai/discobox/internal/filelock"
	"github.com/discobox-ai/discobox/pkg/fsdurable"
	hc "github.com/discobox-ai/discobox/pkg/harnesscapture"
	"github.com/discobox-ai/discobox/sandbox-agent/config"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
)

const CaptureStateDirectory = "/run/discobox/harness-capture"

var ErrCaptureConflict = errors.New("harness configuration candidate changed")
var ErrCaptureDefinitionChanged = errors.New("harness definition changed; deliver the current harness bundle and capture again")
var ErrCaptureUnavailable = errors.New("harness configuration capture unavailable")

// captureState contains secrets and is never part of an export or API preview.
// The acknowledgment is persisted before touching a credential file, so a
// restart retries the conditional handoff rather than losing the receipt.
type captureState struct {
	RecoveryOrder        []string                   `json:"recoveryOrder,omitempty"`
	RetiredFingerprints  []string                   `json:"retiredFingerprints,omitempty"`
	Sequence             uint64                     `json:"sequence"`
	Recovery             map[string]captureRecovery `json:"recovery,omitempty"`
	KnownSentinels       map[string][]string        `json:"knownSentinels,omitempty"`
	SentinelPredecessors map[string]string          `json:"sentinelPredecessors,omitempty"`
	ReceiptPredecessors  map[string]string          `json:"receiptPredecessors,omitempty"`
	Superseded           []retiredCaptureCredential `json:"superseded,omitempty"`
	WriteRevision        int64                      `json:"writeRevision"`
	SecretRevisions      map[string]string          `json:"secretRevisions,omitempty"`
	ObservedDigest       string                     `json:"observedDigest,omitempty"`
	Installed            bool                       `json:"installed"`
	LastAcknowledged     string                     `json:"lastAcknowledged,omitempty"`
	Frozen               map[string]bool            `json:"frozen,omitempty"`
	AcceptedFiles        []hc.File                  `json:"acceptedFiles,omitempty"`
	Sentinels            map[string]string          `json:"sentinels,omitempty"`
	Baseline             hc.Baseline                `json:"baseline"`
	Normalized           hc.Export                  `json:"normalized"`
	Observed             hc.Export                  `json:"observed"`
	Candidate            *hc.Candidate              `json:"candidate,omitempty"`
	History              map[string]hc.Candidate    `json:"history,omitempty"`
	Discarded            string                     `json:"discarded,omitempty"`
	Receipt              *hc.Acknowledgment         `json:"receipt,omitempty"`
	State                string                     `json:"state"`
}

type captureManager struct {
	published           atomic.Pointer[hc.Preview]
	execs               *execs.Manager
	mu                  sync.Mutex
	definition          hc.Definition
	state               captureState
	path, home, workdir string
	recoveryPath        string
	user                *execs.User
	secrets             func() map[string]string
	sandbox             map[string]any
	failed              bool
	run                 func(context.Context, []string, any, any) error
}

func newCaptureManager(s *Service, cfg ServiceConfig) (*captureManager, error) {
	if cfg.Harness.CaptureBaseline == nil {
		return nil, errors.New("capture requires delivered baseline")
	}
	if err := cfg.Harness.SaveBack.Validate(); err != nil {
		return nil, err
	}
	home, err := resolveHomeDir(cfg.ExecDefaults.HomeDirectory, s.defaultUser, s.installEnv())
	if err != nil {
		return nil, err
	}
	workdir, err := cfg.Execs.DefaultWorkdir()
	if err != nil {
		return nil, err
	}
	dir := cfg.CaptureStateDir
	if dir == "" {
		dir = CaptureStateDirectory
	}
	m := &captureManager{execs: cfg.Execs, definition: *cfg.Harness.SaveBack, path: filepath.Join(dir, "state.json"), home: home, workdir: workdir, user: s.defaultUser, secrets: cfg.SecretEnv, sandbox: cfg.SandboxConfig}
	if cfg.CaptureRecoveryDir != "" {
		m.recoveryPath = filepath.Join(cfg.CaptureRecoveryDir, "checkpoint.json")
	}
	m.run = m.command
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	m.state = captureState{Baseline: *cfg.Harness.CaptureBaseline, History: map[string]hc.Candidate{}, State: "clean"}
	if err := m.loadCaptureState(); err != nil {
		return nil, err
	}
	invalidated := false
	if m.state.Baseline.HarnessConfigID != cfg.Harness.CaptureBaseline.HarnessConfigID || m.state.Baseline.ManifestDigest != cfg.Harness.CaptureBaseline.ManifestDigest {
		m.state = captureState{Baseline: *cfg.Harness.CaptureBaseline, History: map[string]hc.Candidate{}, State: "clean"}
		invalidated = true
	}
	if !m.state.Installed && cfg.PrimaryState != nil {
		launched, err := cfg.PrimaryState.PrimaryTerminalLaunched(context.Background())
		if err != nil {
			return nil, err
		}
		m.state.Installed = launched
	}
	if m.state.History == nil {
		m.state.History = map[string]hc.Candidate{}
	}
	if m.state.WriteRevision == 0 {
		m.state.WriteRevision = m.state.Baseline.ConfigRevision
		m.state.SecretRevisions = maps.Clone(m.state.Baseline.SecretRevisions)
	}
	if invalidated {
		if err = m.persist(); err != nil {
			return nil, err
		}
	}
	m.publish()
	return m, nil
}

func readCaptureFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 8*hc.MaxBytes+1))
	if len(data) > 8*hc.MaxBytes {
		return nil, errors.New("private capture state exceeds limit")
	}
	return data, err
}

// command never enters exec audit/history: stdout and stdin carry real tokens.
// Both pipes are bounded, stderr is discarded and errors contain no output.
func (m *captureManager) command(ctx context.Context, argv []string, input, output any) error {
	data, err := json.Marshal(input)
	if err != nil || len(data) > hc.MaxBytes {
		return errors.New("capture input exceeds limit")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	// RunOnce resolves live secrets per invocation. Only HOME is an explicit
	// override; a copied install environment would pin old sentinels.
	out, err := m.execs.RunOnce(ctx, execs.OnceRequest{Command: argv, Stdin: data, Env: map[string]string{"HOME": m.home}, Dir: m.home, MaxOutput: hc.MaxBytes})
	if err != nil {
		return errors.New("harness capture command failed")
	}
	if err = json.Unmarshal(out, output); err != nil {
		return errors.New("invalid harness capture output")
	}

	return nil
}

func (m *captureManager) runtimeSentinels() map[string]string {
	out := map[string]string{}
	if m.secrets != nil {
		maps.Copy(out, m.secrets())
	}
	return out
}
func (m *captureManager) sentinels() map[string]string {
	out := m.runtimeSentinels()
	for name, accepted := range m.state.Sentinels {
		// Bridge only the precise pre-ack runtime delivery. Catch-up, a later
		// remint, or removal of that binding must regain authority immediately.
		if out[name] == m.state.SentinelPredecessors[name] && out[name] != accepted {
			out[name] = accepted
		}
	}
	return out
}
func (m *captureManager) refreshSentinels() error {
	live := m.runtimeSentinels()
	known := map[string][]string{}
	for name, values := range m.state.KnownSentinels {
		known[name] = slices.Clone(values)
	}
	changed := false
	add := func(name, value string) error {
		if value == "" || slices.Contains(known[name], value) {
			return nil
		}
		if len(known[name]) >= 32 || (len(known) >= 128 && known[name] == nil) {
			return errors.New("sentinel history exhausted; preserve local credentials")
		}
		known[name] = append(known[name], value)
		changed = true
		return nil
	}
	for name, value := range live {
		if err := add(name, value); err != nil {
			return err
		}
	}
	for name, value := range m.state.Sentinels {
		if err := add(name, value); err != nil {
			return err
		}
		if err := add(name, m.state.SentinelPredecessors[name]); err != nil {
			return err
		}
	}
	if m.state.Receipt != nil {
		for name, value := range m.state.Receipt.Sentinels {
			if err := add(name, value); err != nil {
				return err
			}
		}
	}
	for name, value := range m.state.Sentinels {
		if live[name] != m.state.SentinelPredecessors[name] || live[name] == value {
			delete(m.state.Sentinels, name)
			delete(m.state.SentinelPredecessors, name)
			changed = true
		}
	}
	m.state.KnownSentinels = known
	if changed {
		return m.persist()
	}
	return nil
}
func (m *captureManager) input(baselineOnly bool) hc.ExportInput {
	return hc.ExportInput{Version: hc.Version, Definition: m.definition, Baseline: m.state.Baseline, BaselineOnly: baselineOnly, Sentinels: m.sentinels(), KnownSentinels: m.state.KnownSentinels, WorkingDir: m.workdir}
}
func (m *captureManager) persist() error {
	if err := m.persistCheckpoint(); err != nil {
		return err
	}
	if err := writeCaptureSnapshot(m.path, m.state); err != nil {
		return err
	}
	return m.removeAbandonedWrites()
}
func (m *captureManager) lock(_ context.Context) (func(), error) {
	m.mu.Lock()
	root, err := os.OpenRoot(m.home)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	defer root.Close()
	if err = root.MkdirAll(".config/discobox", 0700); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	// Open and chown through descriptors confined to HOME, never a path that
	// a user can swap for a symlink between validation and root's write.
	for _, name := range []string{".config", ".config/discobox"} {
		dir, err := root.Open(name)
		if err != nil {
			m.mu.Unlock()
			return nil, err
		}
		err = chownCapture(dir, FileInstaller{User: m.user})
		_ = dir.Close()
		if err != nil {
			m.mu.Unlock()
			return nil, err
		}
	}
	file, err := root.OpenFile(".config/discobox/harness-capture.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	l, err := filelock.TryAcquireFile(file)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if err = chownCapture(file, FileInstaller{User: m.user}); err != nil {
		_ = l.Release()
		m.mu.Unlock()
		return nil, err
	}

	return func() { m.publish(); _ = l.Release(); m.mu.Unlock() }, nil
}
func chownCapture(file *os.File, i FileInstaller) error {
	uid, gid := -1, -1
	if i.uid() != nil {
		uid = int(*i.uid())
	}
	if i.gid() != nil {
		gid = int(*i.gid())
	}
	if uid == -1 && gid == -1 {
		return nil
	}
	return file.Chown(uid, gid)
}

// guard observes before any restoration. Failed exports fail closed: no local
// login or settings may be overwritten merely because its parser is unavailable.
func (m *captureManager) guard(ctx context.Context) (func(), error) {
	if m == nil {
		return func() {}, nil
	}
	release, err := m.lock(ctx)
	if err != nil {
		return nil, err
	}
	if err = m.observe(ctx); err != nil {
		m.failed = true
	}
	return release, nil
}
func (m *captureManager) protect(path string, install bool) bool {
	if m == nil {
		return false
	}
	watched := slices.Contains(m.definition.Watch, path) || slices.Contains(m.definition.CredentialFiles, path)
	if m.failed && watched {
		return true
	}
	if slices.Contains(m.state.Observed.ProtectedPaths, path) {
		return true
	}
	for _, c := range m.state.Observed.Credentials {
		if !c.Keep && slices.Contains(m.definition.CredentialFiles, path) {
			return true
		}
	}
	if install && watched {
		if m.state.Installed && !slices.Contains(m.definition.CredentialFiles, path) {
			return true
		}
		_, err := os.Lstat(filepath.Join(m.home, filepath.FromSlash(path)))
		return err == nil
	}
	return false
}
func (m *captureManager) observe(ctx context.Context) error {
	if err := m.refreshSentinels(); err != nil {
		return err
	}
	if err := m.publishSentinelContext(); err != nil {
		return err
	}
	if m.state.Receipt != nil {
		if err := m.handoff(ctx); err != nil {
			return err
		}
	}
	if !m.completeBaseline(m.state.Normalized) {
		// Recover an older persisted partial projection without treating its
		// missing settings as the delivered truth. Frozen in-flight snapshots
		// remain available for their receipt, but no invalid preview survives.
		if c := m.state.Candidate; c != nil && !m.state.Frozen[c.ID] {
			delete(m.state.History, c.ID)
		}
		m.state.Candidate = nil
		m.state.ObservedDigest = ""
		m.state.Normalized = hc.Export{}
		if err := m.persist(); err != nil {
			return err
		}
		input := m.input(true)
		input.Baseline.Files = slices.Clone(input.Baseline.Files)
		context := maps.Clone(m.sandbox)
		if context == nil {
			context = map[string]any{}
		}
		context["workingDir"] = m.workdir
		context["secrets"] = m.sentinels()
		for n := range input.Baseline.Files {
			f := &input.Baseline.Files[n]
			if !f.Template {
				continue
			}
			content, err := renderHarnessFileTemplate(f.Path, f.Content, context)
			if err != nil {
				return errors.New("render delivered capture baseline")
			}
			f.Content = content
			f.Template = false
		}
		var normalized hc.Export
		if err := m.run(ctx, m.definition.ExportCommand, input, &normalized); err != nil {
			return err
		}
		if !m.completeBaseline(normalized) {
			return ErrCaptureUnavailable
		}
		m.state.Normalized = publicObservation(normalized)
	}
	var observed hc.Export
	if err := m.run(ctx, m.definition.ExportCommand, m.input(false), &observed); err != nil {
		return err
	}
	if observed.Version != hc.Version {
		return ErrCaptureUnavailable
	}
	// Settings must pass the same allowlist the control plane uses. Diagnostics
	// and credentials remain private until projected by preview().
	if err := m.definition.ValidateFiles(observed.Files); err != nil {
		return errors.New("invalid harness settings export")
	}
	digest, err := exportDigest(observed)
	if err != nil {
		return err
	}
	if len(m.state.Superseded) > 0 || len(m.state.RetiredFingerprints) > 0 {
		if m.supersededObservation(observed) {
			m.state.Observed = publicObservation(observed)
			m.state.ObservedDigest = digest
			m.state.Candidate = nil
			m.state.State = "superseded"
			m.failed = false
			return m.persist()
		}
		m.state.Superseded = nil
		m.state.RetiredFingerprints = nil
		if m.state.State == "superseded" {
			m.state.State = "clean"
		}
	}
	if !m.failed && digest == m.state.ObservedDigest && m.state.State != "clean" {
		return nil
	}
	m.state.Observed = publicObservation(observed)
	m.state.ObservedDigest = digest
	if m.state.Discarded != "" && digest == m.state.Discarded {
		return nil
	}
	changes := captureDiff(m.state.Normalized.Files, observed.Files, m.state.Baseline.ProjectFiles)
	changes = slices.DeleteFunc(changes, func(f hc.FileChange) bool {
		if !f.Remove {
			return false
		}
		if slices.Contains(m.definition.CredentialFiles, f.Path) {
			return true
		}
		for _, d := range observed.Diagnostics {
			if d.Path == f.Path {
				return true
			}
		}
		return false
	})
	var creds []hc.Credential
	for _, c := range observed.Credentials {
		if !c.Keep {
			creds = append(creds, c)
		}
	}
	if len(changes) == 0 && len(creds) == 0 && len(observed.Diagnostics) == 0 {
		m.failed = false
		m.state.Candidate = nil
		m.state.State = "clean"
		return m.persist()
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return err
	}
	candidate := hc.Candidate{ID: hex.EncodeToString(id), Baseline: m.state.Baseline, WriteRevision: m.writeRevision(), SecretRevisions: m.writeSecretRevisions(), Files: changes, Credentials: observed.Credentials, Diagnostics: observed.Diagnostics}
	if err := m.definition.ValidateCandidate(candidate); err != nil {
		return errors.New("invalid harness capture candidate")
	}
	for key := range m.state.History {
		if !m.state.Frozen[key] && (m.state.Receipt == nil || key != m.state.Receipt.CandidateID) {
			delete(m.state.History, key)
		}
	}
	if len(m.state.History) >= 8 { // bounded private retention; never evict the active receipt.
		for key := range m.state.History {
			if !m.state.Frozen[key] && (m.state.Receipt == nil || key != m.state.Receipt.CandidateID) {
				delete(m.state.History, key)
				break
			}
		}
	}
	if len(m.state.History) >= 8 {
		return errors.New("capture receipts require reconciliation")
	}
	m.failed = false
	m.state.Candidate = &candidate
	if m.state.History == nil {
		m.state.History = map[string]hc.Candidate{}
	}
	m.state.History[candidate.ID] = candidate
	m.state.State = "pending"
	return m.persist()
}
func captureDiff(before, after []hc.File, project []string) []hc.FileChange {
	old := map[string]hc.File{}
	for _, f := range before {
		old[f.Path] = f
	}
	var changes []hc.FileChange
	for _, f := range after {
		if slices.Contains(project, f.Path) {
			delete(old, f.Path)
			continue
		}
		prior, ok := old[f.Path]
		if !ok || prior != f {
			file := f
			changes = append(changes, hc.FileChange{Path: f.Path, File: &file})
		}
		delete(old, f.Path)
	}
	for path := range old {
		if !slices.Contains(project, path) {
			changes = append(changes, hc.FileChange{Path: path, Remove: true})
		}
	}
	slices.SortFunc(changes, func(a, b hc.FileChange) int {
		if a.Path < b.Path {
			return -1
		}
		if a.Path > b.Path {
			return 1
		}
		return 0
	})
	return changes
}
func (m *captureManager) preview() hc.Preview {
	if m == nil {
		return hc.Preview{State: "unsupported"}
	}
	p := hc.Preview{HarnessConfigID: m.state.Baseline.HarnessConfigID, ConfigRevision: m.state.Baseline.ConfigRevision, State: m.state.State}
	if m.failed {
		p.State = "unavailable"
	}
	if m.state.Receipt != nil {
		p.State = "saved-handoff-pending"
	}
	if (len(m.state.Superseded) > 0 || len(m.state.RetiredFingerprints) > 0) || m.state.State == "superseded" {
		message := "Saved settings changed remotely. Make a new local edit; the previous candidate cannot be reconsidered."
		if len(m.state.Superseded) > 0 || len(m.state.RetiredFingerprints) > 0 {
			message = "Saved credentials changed remotely. A new local login is required; the previous candidate cannot be reconsidered."
		}
		p.Diagnostics = append(p.Diagnostics, hc.Diagnostic{Code: "superseded", Message: message})
	}
	if c := m.state.Candidate; c != nil {
		p.CandidateID = c.ID
		p.Files = c.Files
		// Exporter diagnostics have fixed messages, but project only code/path here:
		// a faulty bundle cannot leak a parse fragment or credential in Message.
		for _, d := range c.Diagnostics {
			path := d.Path
			if !slices.Contains(m.definition.Watch, path) && !slices.Contains(m.definition.CredentialFiles, path) {
				path = ""
			}
			code := d.Code
			if code != "partial" && code != "unsupported" {
				code = "invalid"
			}
			p.Diagnostics = append(p.Diagnostics, hc.Diagnostic{Path: path, Code: code, Message: "Harness configuration needs attention"})
		}
		for _, v := range c.Credentials {
			if !v.Keep {
				p.Credentials = append(p.Credentials, hc.CredentialPreview{Provider: v.Provider, EnvName: v.EnvName, Account: v.Account, Type: v.Type})
			}
		}
	}
	return p
}

// publish is called with mu held. Status readers use the last completed
// observation, rather than waiting for a native credential exporter (which can
// take tens of seconds). Candidate contents are immutable once published.
func (m *captureManager) publish() { p := m.preview(); m.published.Store(&p) }
func (s *Service) HarnessConfigStatus() hc.Preview {
	if s.capture == nil {
		return hc.Preview{State: "unsupported"}
	}
	if p := s.capture.published.Load(); p != nil {
		return *p
	}
	return hc.Preview{State: "initializing"}
}

func (s *Service) HarnessConfigChanges(ctx context.Context) (hc.Preview, error) {
	m := s.capture
	if m == nil {
		return hc.Preview{State: "unsupported"}, nil
	}
	release, err := m.guard(ctx)
	if err != nil {
		return hc.Preview{}, err
	}
	defer release()
	return m.preview(), nil
}
func (s *Service) CaptureHarnessConfig(ctx context.Context, id string) (hc.Candidate, error) {
	m := s.capture
	if m == nil {
		return hc.Candidate{}, ErrCaptureUnavailable
	}
	release, err := m.guard(ctx)
	if err != nil {
		return hc.Candidate{}, err
	}
	defer release()
	if m.failed {
		return hc.Candidate{}, ErrCaptureUnavailable
	}
	if m.state.Candidate == nil || m.state.Candidate.ID != id {
		return hc.Candidate{}, ErrCaptureConflict
	}
	if m.state.Frozen == nil {
		m.state.Frozen = map[string]bool{}
	}
	m.state.Frozen[id] = true
	if err := m.persist(); err != nil {
		return hc.Candidate{}, err
	}
	return *m.state.Candidate, nil
}
func (s *Service) DiscardHarnessConfig(ctx context.Context, id string) (hc.Preview, error) {
	m := s.capture
	if m == nil {
		return hc.Preview{}, ErrCaptureUnavailable
	}
	release, err := m.guard(ctx)
	if err != nil {
		return hc.Preview{}, err
	}
	defer release()
	if m.state.Candidate == nil || m.state.Candidate.ID != id {
		return hc.Preview{}, ErrCaptureConflict
	}
	m.purge(*m.state.Candidate)
	m.state.Discarded = m.state.ObservedDigest
	m.state.Candidate = nil
	m.state.State = "discarded"
	return m.preview(), m.persist()
}
func (s *Service) ReconsiderHarnessConfig(ctx context.Context, r hc.Reconsideration) (hc.Preview, error) {
	m := s.capture
	if m == nil {
		return hc.Preview{}, ErrCaptureUnavailable
	}
	release, err := m.lock(ctx)
	if err != nil {
		return hc.Preview{}, err
	}
	defer release()
	if r.Current.ManifestDigest != m.state.Baseline.ManifestDigest {
		return hc.Preview{}, ErrCaptureDefinitionChanged
	}
	if err = m.observe(ctx); err != nil {
		m.failed = true
		return hc.Preview{}, ErrCaptureUnavailable
	}
	if m.state.Candidate == nil || m.state.Candidate.ID != r.Selection.CandidateID {
		return hc.Preview{}, ErrCaptureConflict
	}
	c := *m.state.Candidate
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return hc.Preview{}, err
	}
	c.ID = hex.EncodeToString(id)
	c.WriteRevision = r.Current.ConfigRevision
	c.SecretRevisions = r.Current.SecretRevisions
	c.Files = nil
	for _, path := range r.Selection.KeepFiles {
		portable := false
		for _, setting := range m.definition.Settings {
			if setting.Path == path {
				portable = true
				break
			}
		}
		if !portable {
			return hc.Preview{}, ErrCaptureConflict
		}
	}
	for _, f := range m.state.Candidate.Files {
		if slices.Contains(r.Selection.KeepFiles, f.Path) {
			c.Files = append(c.Files, f)
		}
	}
	for _, name := range r.Selection.ReplaceCredentials {
		found := false
		for _, v := range m.state.Candidate.Credentials {
			if !v.Keep && v.EnvName == name {
				found = true
				break
			}
		}
		if !found {
			return hc.Preview{}, ErrCaptureConflict
		}
	}
	input := m.input(false)
	input.Reconsider = &hc.ReconsiderInput{Candidate: *m.state.Candidate, Current: r.Current, ReplaceCredentials: r.Selection.ReplaceCredentials}
	var projection hc.Export
	if err := m.run(ctx, m.definition.ExportCommand, input, &projection); err != nil {
		return hc.Preview{}, err
	}
	if projection.Version != hc.Version {
		return hc.Preview{}, ErrCaptureUnavailable
	}
	for _, f := range projection.Files {
		if !slices.Contains(m.definition.CredentialFiles, f.Path) {
			return hc.Preview{}, ErrCaptureUnavailable
		}
	}
	// The harness owns auth formats. KeepFiles selects only portable settings;
	// selected credentials and their projected delivery move as one change.
	changes := captureDiff(r.Current.Files, projection.Files, nil)
	for _, change := range changes {
		if !change.Remove {
			c.Files = append(c.Files, change)
		}
	}
	c.Credentials = projection.Credentials
	for _, v := range c.Credentials {
		if v.Keep {
			continue
		}
		if !slices.Contains(r.Selection.ReplaceCredentials, v.EnvName) {
			return hc.Preview{}, ErrCaptureUnavailable
		}
		valid := false
		for _, original := range m.state.Candidate.Credentials {
			if v.EnvName == original.EnvName && v.Generation == original.Generation && bytes.Equal(v.Value, original.Value) {
				valid = true
				break
			}
		}
		if !valid {
			return hc.Preview{}, ErrCaptureUnavailable
		}
	}
	for _, name := range r.Selection.ReplaceCredentials {
		found := false
		for _, v := range c.Credentials {
			if !v.Keep && v.EnvName == name {
				found = true
				break
			}
		}
		if !found {
			return hc.Preview{}, ErrCaptureUnavailable
		}
	}
	if err := m.definition.ValidateCandidate(c); err != nil {
		return hc.Preview{}, err
	}
	delete(m.state.Frozen, r.Selection.CandidateID)
	delete(m.state.History, r.Selection.CandidateID)
	m.state.Candidate = &c
	if m.state.History == nil {
		m.state.History = map[string]hc.Candidate{}
	}
	m.state.History[c.ID] = c
	m.state.State = "pending"
	return m.preview(), m.persist()
}
func (s *Service) AcknowledgeHarnessConfig(ctx context.Context, a hc.Acknowledgment) (hc.Preview, error) {
	m := s.capture
	if m == nil && a.Superseded {
		return hc.Preview{State: "unsupported"}, nil
	}
	if m == nil {
		return hc.Preview{}, ErrCaptureUnavailable
	}
	release, err := m.lock(ctx)
	if err != nil {
		return hc.Preview{}, err
	}
	defer release()
	if a.Superseded {
		return m.supersede(a)
	}
	if a.CandidateID == m.state.LastAcknowledged && m.state.Receipt == nil {
		return hc.Preview{State: "saved", CandidateID: a.CandidateID}, nil
	}
	if a.Baseline.ConfigRevision < m.writeRevision() {
		return hc.Preview{}, ErrCaptureConflict
	}
	if a.Baseline.HarnessConfigID != m.state.Baseline.HarnessConfigID {
		return hc.Preview{}, ErrCaptureConflict
	}
	if _, ok := m.state.History[a.CandidateID]; !ok {
		if _, recoverable := m.state.Recovery[a.CandidateID]; !recoverable {
			return hc.Preview{State: "saved", CandidateID: a.CandidateID}, m.finishUnrecoverableAcknowledgment(a)
		}
	}
	if m.state.Receipt == nil || m.state.Receipt.CandidateID != a.CandidateID {
		m.state.ReceiptPredecessors = m.runtimeSentinels()
	}
	m.state.Receipt = &a
	m.state.State = "saved-handoff-pending"
	if err := m.persist(); err != nil {
		return hc.Preview{}, err
	}
	m.publish()
	if err := m.handoff(ctx); err != nil {
		return m.preview(), nil
	}
	return m.preview(), nil
}
func (m *captureManager) handoff(ctx context.Context) error {
	a := m.state.Receipt
	if a == nil {
		return nil
	}
	c, ok := m.state.History[a.CandidateID]
	conflict := false
	if !ok {
		var err error
		c, conflict, err = m.recoverCandidate(ctx, *a)
		if err != nil {
			return err
		}
	}
	files := slices.Clone(a.Files)
	tmpl := maps.Clone(m.sandbox)
	if tmpl == nil {
		tmpl = map[string]any{}
	}
	tmpl["workingDir"] = m.workdir
	tmpl["secrets"] = a.Sentinels
	for n := range files {
		if files[n].Template {
			content, err := renderHarnessFileTemplate(files[n].Path, files[n].Content, tmpl)
			if err != nil {
				return errors.New("render accepted credential template")
			}
			files[n].Content = content
			files[n].Template = false
		}
	}
	var result hc.HandoffResult
	if conflict {
		result.State = "local-conflict"
	} else if err := m.run(ctx, m.definition.HandoffCommand, hc.Handoff{Version: hc.Version, CandidateID: c.ID, Credentials: c.Credentials, Files: files, Sentinels: a.Sentinels}, &result); err != nil {
		return err
	}
	if result.State != "applied" && result.State != "local-conflict" {
		return errors.New("credential handoff deferred")
	}
	m.state.LastAcknowledged = a.CandidateID
	delete(m.state.Recovery, a.CandidateID)
	delete(m.state.Frozen, a.CandidateID)
	m.state.AcceptedFiles = slices.Clone(a.Files)
	m.state.Sentinels = maps.Clone(a.Sentinels)
	m.state.SentinelPredecessors = maps.Clone(m.state.ReceiptPredecessors)
	m.state.ReceiptPredecessors = nil
	m.state.Observed = hc.Export{}
	m.state.ObservedDigest = ""
	// Only the accepted local patch was delivered. Remote-only settings in
	// the receipt remain remote, even though its revision now gates writes.
	m.state.Baseline.Files = applyCaptureFiles(m.state.Baseline.Files, c.Files)
	if a.Baseline.ConfigRevision >= m.writeRevision() {
		m.state.WriteRevision = a.Baseline.ConfigRevision
		m.state.SecretRevisions = maps.Clone(a.Baseline.SecretRevisions)
	}
	m.state.Normalized = hc.Export{}
	m.state.Discarded = ""
	m.state.Receipt = nil
	m.purge(c)
	m.state.State = "saved"
	if err := m.persist(); err != nil {
		return err
	}
	if err := m.refreshSentinels(); err != nil {
		return err
	}
	return m.publishSentinelContext()
}
func (s *Service) WatchHarnessConfig(ctx context.Context) {
	if s.capture == nil {
		return
	}
	watchHarnessConfig(ctx, s.capture.home, s.capture.definition.Watch, func() {
		_, _ = s.HarnessConfigChanges(ctx)
	}, time.Second, 30*time.Second)
}

func (m *captureManager) files(files []config.HarnessFile) []config.HarnessFile {
	if m == nil || len(m.state.AcceptedFiles) == 0 {
		return files
	}
	result := slices.Clone(files)
	for _, f := range m.state.AcceptedFiles {
		next := config.HarnessFile{Path: f.Path, Content: f.Content, Template: f.Template, CreateOnly: f.CreateOnly}
		found := false
		for n := range result {
			if result[n].Path == f.Path {
				result[n] = next
				found = true
				break
			}
		}
		if !found {
			result = append(result, next)
		}
	}
	return result
}

func (m *captureManager) writeRevision() int64 {
	if m.state.WriteRevision != 0 {
		return m.state.WriteRevision
	}
	return m.state.Baseline.ConfigRevision
}
func (m *captureManager) writeSecretRevisions() map[string]string {
	if m.state.WriteRevision != 0 {
		return m.state.SecretRevisions
	}
	return m.state.Baseline.SecretRevisions
}
func exportDigest(e hc.Export) (string, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// Observation metadata protects local files but never retains reusable material.
func publicObservation(e hc.Export) hc.Export {
	e.Credentials = slices.Clone(e.Credentials)
	for i := range e.Credentials {
		e.Credentials[i].Value = nil
	}
	return e
}
func applyCaptureFiles(files []hc.File, changes []hc.FileChange) []hc.File {
	result := slices.Clone(files)
	for _, change := range changes {
		result = slices.DeleteFunc(result, func(f hc.File) bool { return f.Path == change.Path })
		if !change.Remove && change.File != nil {
			result = append(result, *change.File)
		}
	}
	return result
}

// Erase the completed candidate and any superseded snapshots carrying the same
// reusable material. Newer local logins remain independently pending.
func (m *captureManager) purge(completed hc.Candidate) {
	matches := func(v hc.Credential) bool {
		if v.Keep || len(v.Value) == 0 {
			return false
		}
		for _, done := range completed.Credentials {
			if !done.Keep && len(done.Value) > 0 && v.EnvName == done.EnvName && bytes.Equal(v.Value, done.Value) {
				return true
			}
		}
		return false
	}
	for id, c := range m.state.History {
		if id == completed.ID {
			delete(m.state.Recovery, id)
			delete(m.state.History, id)
			delete(m.state.Frozen, id)
			continue
		}
		matched := false
		c.Credentials = slices.Clone(c.Credentials)
		for i := range c.Credentials {
			if matches(c.Credentials[i]) {
				matched = true
				c.Credentials[i].Value = nil
			}
		}
		if !matched {
			continue
		}
		if m.state.Frozen[id] {
			// Another save already received this immutable snapshot. Its ack still
			// needs local identity/generation, but not the material now acknowledged.
			m.state.History[id] = c
		} else {
			delete(m.state.History, id)
		}
	}
	if m.state.Candidate != nil {
		invalidate := m.state.Candidate.ID == completed.ID
		for _, v := range m.state.Candidate.Credentials {
			invalidate = invalidate || matches(v)
		}
		if invalidate {
			m.state.Candidate = nil
		}
	}
	m.state.Observed = publicObservation(m.state.Observed)
}

// Interrupted atomic writes are private pending state too. Once a new snapshot
// is durable, obsolete staging files must not retain an acknowledged secret.
func (m *captureManager) removeAbandonedWrites() error {
	entries, err := os.ReadDir(filepath.Dir(m.path))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".capture-") && !entry.IsDir() {
			if err = os.Remove(filepath.Join(filepath.Dir(m.path), entry.Name())); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

// A terminal supersession retains only non-reusable identities. A settings edit
// must not accidentally offer the same now-stale credential for save again.
type retiredCaptureCredential struct {
	EnvName    string `json:"envName"`
	Generation string `json:"generation,omitempty"`
	Digest     string `json:"digest,omitempty"`
}

func credentialDigest(c hc.Credential) string {
	if len(c.Value) == 0 {
		return ""
	}
	sum := sha256.Sum256(c.Value)
	return hex.EncodeToString(sum[:])
}
func (m *captureManager) supersededObservation(observed hc.Export) bool {
	for _, v := range observed.Credentials {
		if v.Keep {
			continue
		}
		if slices.Contains(m.state.RetiredFingerprints, captureCredentialFingerprint(v)) {
			return true
		}
		for _, old := range m.state.Superseded {
			if old.EnvName == v.EnvName && ((old.Generation != "" && old.Generation == v.Generation) || (old.Digest != "" && old.Digest == credentialDigest(v))) {
				return true
			}
		}
	}
	return false
}
func (m *captureManager) supersede(a hc.Acknowledgment) (hc.Preview, error) {
	c, ok := m.state.History[a.CandidateID]
	if !ok {
		m.state.RetiredFingerprints = append(m.state.RetiredFingerprints, m.state.Recovery[a.CandidateID].Credentials...)
		delete(m.state.Recovery, a.CandidateID)
		m.state.LastAcknowledged = a.CandidateID
		if m.state.Receipt != nil && m.state.Receipt.CandidateID == a.CandidateID {
			m.state.Receipt = nil
			m.state.ReceiptPredecessors = nil
		}
		return hc.Preview{State: "superseded", CandidateID: a.CandidateID}, m.persist()
	}
	for _, v := range c.Credentials {
		if !v.Keep {
			m.state.RetiredFingerprints = append(m.state.RetiredFingerprints, captureCredentialFingerprint(v))
			m.state.Superseded = append(m.state.Superseded, retiredCaptureCredential{EnvName: v.EnvName, Generation: v.Generation, Digest: credentialDigest(v)})
		}
	}
	m.purge(c)
	if m.state.Receipt != nil && m.state.Receipt.CandidateID == a.CandidateID {
		m.state.Receipt = nil
		m.state.ReceiptPredecessors = nil
	}
	m.state.LastAcknowledged = a.CandidateID
	if m.state.Candidate == nil {
		m.state.State = "superseded"
		m.state.Discarded = m.state.ObservedDigest
	}
	// No file handoff, sentinel change, or remote baseline adoption is safe.
	return m.preview(), m.persist()
}

// A newly delivered harness without capture support cannot retain an older
// harness's reusable spool. The explicit path comes from the server config;
// direct test/service constructors with no durable path never touch global state.
func invalidateCaptureState(dir string) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() && (entry.Name() == "state.json" || strings.HasPrefix(entry.Name(), ".capture-")) {
			if err = os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return fsdurable.SyncDir(dir)
}

// Native launchers need delivered aliases to distinguish an old DB sentinel
// from a fresh login. This user-readable context contains no real credentials;
// exporters never consume it as authority for capture or server validation.
func (m *captureManager) publishSentinelContext() error {
	data, err := json.Marshal(struct {
		Sentinels      map[string]string   `json:"sentinels"`
		KnownSentinels map[string][]string `json:"knownSentinels"`
	}{m.sentinels(), m.state.KnownSentinels})
	if err != nil {
		return err
	}
	if len(data) > 256<<10 {
		return errors.New("sentinel context exceeds limit")
	}
	root, err := os.OpenRoot(m.home)
	if err != nil {
		return err
	}
	defer root.Close()
	random := make([]byte, 16)
	if _, err = rand.Read(random); err != nil {
		return err
	}
	name := ".config/discobox/.sentinels-" + hex.EncodeToString(random)
	file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(name) }()
	if err = chownCapture(file, FileInstaller{User: m.user}); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return root.Rename(name, ".config/discobox/harness-sentinels.json")
}

// Versioned output can still be incomplete: bundle startup or a parser failure
// reports partial diagnostics while omitting files. Never cache that omission
// as delivered provenance; retry from the original delivered Files instead.
func (m *captureManager) completeBaseline(normalized hc.Export) bool {
	if normalized.Version != hc.Version || m.definition.ValidateFiles(normalized.Files) != nil {
		return false
	}
	for _, diagnostic := range normalized.Diagnostics {
		if diagnostic.Code != "unsupported" {
			return false
		}
	}
	return true
}
