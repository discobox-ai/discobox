package terminal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/discobox-ai/discobox/pkg/fsdurable"
	hc "github.com/discobox-ai/discobox/pkg/harnesscapture"
)

// Frozen candidates survive reboot only as identities and hashes. Their file
// contents and reusable credentials remain exclusively in the /run spool.
type captureRecovery struct {
	Files       []captureRecoveryFile `json:"files,omitempty"`
	Credentials []string              `json:"credentials,omitempty"`
}
type captureRecoveryFile struct {
	Path   string `json:"path"`
	Remove bool   `json:"remove,omitempty"`
}

// This explicit schema must never contain local observations or candidate
// snapshots. Files here are delivered/committed configuration and templates.
const maxCaptureRecovery = 128

type captureCheckpoint struct {
	RecoveryOrder        []string                   `json:"recoveryOrder,omitempty"`
	Superseded           []retiredCaptureCredential `json:"superseded,omitempty"`
	RetiredFingerprints  []string                   `json:"retiredFingerprints,omitempty"`
	Sequence             uint64                     `json:"sequence"`
	Baseline             hc.Baseline                `json:"baseline"`
	Installed            bool                       `json:"installed"`
	WriteRevision        int64                      `json:"writeRevision"`
	SecretRevisions      map[string]string          `json:"secretRevisions,omitempty"`
	LastAcknowledged     string                     `json:"lastAcknowledged,omitempty"`
	AcceptedFiles        []hc.File                  `json:"acceptedFiles,omitempty"`
	Sentinels            map[string]string          `json:"sentinels,omitempty"`
	KnownSentinels       map[string][]string        `json:"knownSentinels,omitempty"`
	SentinelPredecessors map[string]string          `json:"sentinelPredecessors,omitempty"`
	ReceiptPredecessors  map[string]string          `json:"receiptPredecessors,omitempty"`
	Receipt              *hc.Acknowledgment         `json:"receipt,omitempty"`
	Recovery             map[string]captureRecovery `json:"recovery,omitempty"`
}

func captureCredentialFingerprint(c hc.Credential) string {
	// Length-prefix every field, including the unparsed value, so even malformed
	// exporter values produce an unambiguous fingerprint without serialization.
	var data []byte
	for _, field := range []string{c.Provider, c.EnvName, c.Account, c.LocalID, c.Generation, c.Type, strconv.FormatBool(c.Keep), string(c.Value)} {
		data = strconv.AppendInt(data, int64(len(field)), 10)
		data = append(data, ':')
		data = append(data, field...)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (m *captureManager) checkpoint() captureCheckpoint {
	if m.state.Recovery == nil {
		m.state.Recovery = map[string]captureRecovery{}
	}
	for id, c := range m.state.History {
		if !m.state.Frozen[id] && (m.state.Receipt == nil || m.state.Receipt.CandidateID != id) {
			continue
		}
		// Do not replace the original fingerprint after purge has erased a value.
		if _, ok := m.state.Recovery[id]; ok {
			continue
		}
		r := captureRecovery{}
		for _, f := range c.Files {
			r.Files = append(r.Files, captureRecoveryFile{Path: f.Path, Remove: f.Remove})
		}
		for _, c := range c.Credentials {
			if !c.Keep {
				r.Credentials = append(r.Credentials, captureCredentialFingerprint(c))
			}
		}
		m.state.Recovery[id] = r
		m.state.RecoveryOrder = append(m.state.RecoveryOrder, id)
	}
	m.state.RecoveryOrder = slices.DeleteFunc(m.state.RecoveryOrder, func(id string) bool { _, ok := m.state.Recovery[id]; return !ok })
	for len(m.state.RecoveryOrder) > maxCaptureRecovery {
		n := 0
		if m.state.Receipt != nil && m.state.RecoveryOrder[n] == m.state.Receipt.CandidateID {
			n++
		}
		delete(m.state.Recovery, m.state.RecoveryOrder[n])
		m.state.RecoveryOrder = slices.Delete(m.state.RecoveryOrder, n, n+1)
	}
	retired := make([]retiredCaptureCredential, 0, len(m.state.Superseded))
	for _, r := range m.state.Superseded {
		if r.Digest != "" {
			retired = append(retired, retiredCaptureCredential{EnvName: r.EnvName, Digest: r.Digest})
		}
	}
	return captureCheckpoint{
		RecoveryOrder: m.state.RecoveryOrder, Superseded: retired, RetiredFingerprints: m.state.RetiredFingerprints, Sequence: m.state.Sequence, Baseline: m.state.Baseline, Installed: m.state.Installed,
		WriteRevision: m.state.WriteRevision, SecretRevisions: m.state.SecretRevisions,
		LastAcknowledged: m.state.LastAcknowledged, AcceptedFiles: m.state.AcceptedFiles,
		Sentinels: m.state.Sentinels, KnownSentinels: m.state.KnownSentinels,
		SentinelPredecessors: m.state.SentinelPredecessors, ReceiptPredecessors: m.state.ReceiptPredecessors,
		Receipt: m.state.Receipt, Recovery: m.state.Recovery,
	}
}
func (c captureCheckpoint) state() captureState {
	state := "clean"
	if c.Receipt != nil {
		state = "saved-handoff-pending"
	}
	return captureState{
		RecoveryOrder: c.RecoveryOrder, Superseded: c.Superseded, RetiredFingerprints: c.RetiredFingerprints, Sequence: c.Sequence, Baseline: c.Baseline, Installed: c.Installed,
		WriteRevision: c.WriteRevision, SecretRevisions: c.SecretRevisions,
		LastAcknowledged: c.LastAcknowledged, AcceptedFiles: c.AcceptedFiles,
		Sentinels: c.Sentinels, KnownSentinels: c.KnownSentinels,
		SentinelPredecessors: c.SentinelPredecessors, ReceiptPredecessors: c.ReceiptPredecessors,
		Receipt: c.Receipt, Recovery: c.Recovery, State: state, History: map[string]hc.Candidate{},
	}
}
func (m *captureManager) persistCheckpoint() error {
	m.state.Sequence++
	checkpoint := m.checkpoint()
	if m.recoveryPath == "" {
		return nil
	}
	// The checkpoint commits first: a crash between writes discards an older
	// ephemeral snapshot rather than replaying a finished handoff.
	return writeCaptureSnapshot(m.recoveryPath, checkpoint)
}
func writeCaptureSnapshot(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > 8*hc.MaxBytes {
		return errors.New("capture state exceeds limit")
	}
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".capture-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	return fsdurable.SyncDir(dir)
}

func (m *captureManager) loadCaptureState() error {
	var checkpoint *captureCheckpoint
	if m.recoveryPath != "" {
		data, err := readCaptureFile(m.recoveryPath)
		if err == nil {
			checkpoint = new(captureCheckpoint)
			if json.Unmarshal(data, checkpoint) != nil {
				return errors.New("invalid capture checkpoint")
			}
			m.state = checkpoint.state()
		} else if !os.IsNotExist(err) {
			return err
		}
		// Upgrade the previous durable spool in place without a secret-bearing
		// backup. Transfer its live spool into /run, then remove old staging files.
		legacy := filepath.Join(filepath.Dir(m.recoveryPath), "state.json")
		data, err = readCaptureFile(legacy)
		if err == nil && checkpoint == nil {
			if json.Unmarshal(data, &m.state) != nil {
				return errors.New("invalid legacy capture state")
			}
			if err = m.persist(); err != nil {
				return err
			}
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := invalidateCaptureState(filepath.Dir(m.recoveryPath)); err != nil {
			return err
		}
	}
	data, err := readCaptureFile(m.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var state captureState
	if json.Unmarshal(data, &state) != nil {
		return errors.New("invalid private capture state")
	}
	if checkpoint == nil || state.Sequence == checkpoint.Sequence {
		m.state = state
	}
	return nil
}

// A reboot destroyed the reviewed snapshot. A server receipt can still finish
// its conditional handoff, but only with matching freshly exported credentials.
// A later local login is preserved and will receive a new review candidate.
func (m *captureManager) recoverCandidate(ctx context.Context, a hc.Acknowledgment) (hc.Candidate, bool, error) {
	r, ok := m.state.Recovery[a.CandidateID]
	if !ok {
		return hc.Candidate{}, false, ErrCaptureConflict
	}
	c := hc.Candidate{ID: a.CandidateID}
	for _, f := range r.Files {
		change := hc.FileChange{Path: f.Path, Remove: f.Remove}
		if !f.Remove {
			n := slices.IndexFunc(a.Files, func(v hc.File) bool { return v.Path == f.Path })
			if n < 0 {
				return c, false, ErrCaptureConflict
			}
			file := a.Files[n]
			change.File = &file
		}
		c.Files = append(c.Files, change)
	}
	if len(r.Credentials) == 0 {
		return c, false, nil
	}
	var observed hc.Export
	if err := m.run(ctx, m.definition.ExportCommand, m.input(false), &observed); err != nil {
		return c, false, err
	}
	if observed.Version != hc.Version {
		return c, false, ErrCaptureUnavailable
	}
	for _, hash := range r.Credentials {
		n := slices.IndexFunc(observed.Credentials, func(v hc.Credential) bool { return !v.Keep && captureCredentialFingerprint(v) == hash })
		if n < 0 {
			return c, true, nil
		}
		c.Credentials = append(c.Credentials, observed.Credentials[n])
	}
	return c, false, nil
}

// An old receipt may outlive the bounded recovery identities. Never guess its
// local patch or login generation: accept only its server write revision, leave
// every local file untouched, and freshly review the differences still present.
func (m *captureManager) finishUnrecoverableAcknowledgment(a hc.Acknowledgment) error {
	m.state.LastAcknowledged = a.CandidateID
	m.state.WriteRevision = a.Baseline.ConfigRevision
	m.state.SecretRevisions = a.Baseline.SecretRevisions
	m.state.Candidate = nil
	m.state.ObservedDigest = ""
	m.state.State = "saved"
	if m.state.Receipt != nil && m.state.Receipt.CandidateID == a.CandidateID {
		m.state.Receipt = nil
		m.state.ReceiptPredecessors = nil
	}
	return m.persist()
}
