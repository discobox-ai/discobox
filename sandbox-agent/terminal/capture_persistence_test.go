package terminal

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	hc "github.com/discobox-ai/discobox/pkg/harnesscapture"
)

func enableCaptureCheckpoint(t *testing.T, s *Service) {
	t.Helper()
	s.capture.recoveryPath = filepath.Join(t.TempDir(), "checkpoint.json")
}
func restartCapture(t *testing.T, s *Service, reboot bool) {
	t.Helper()
	if reboot {
		if err := os.Remove(s.capture.path); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	s.capture.state = captureState{}
	if err := s.capture.loadCaptureState(); err != nil {
		t.Fatal(err)
	}
}
func frozenCapture(t *testing.T, s *Service) hc.Candidate {
	t.Helper()
	p, err := s.HarnessConfigChanges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.CaptureHarnessConfig(context.Background(), p.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func TestCaptureCheckpointContainsNoLocalSnapshots(t *testing.T) {
	s, observed, _ := captureFixture(t)
	enableCaptureCheckpoint(t, s)
	observed.Files[0].Content = `{"model":"pending-private-setting"}`
	observed.Credentials = []hc.Credential{{Provider: "test", EnvName: "TOKEN", Type: "token", LocalID: "private-local-id", Generation: "private-generation", Value: json.RawMessage(`{"token":"private-token"}`)}}
	c := frozenCapture(t, s)
	data, err := os.ReadFile(s.capture.recoveryPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"pending-private-setting", "private-token", "private-local-id", "private-generation", "normalized", "observed", "history"} {
		if bytes.Contains(data, []byte(marker)) {
			t.Fatalf("checkpoint retained %s", marker)
		}
	}
	restartCapture(t, s, false)
	if s.capture.state.Candidate == nil || s.capture.state.Candidate.ID != c.ID {
		t.Fatal("agent restart lost pending candidate")
	}
	restartCapture(t, s, true)
	if s.capture.state.Candidate != nil || len(s.capture.state.History) != 0 {
		t.Fatal("reboot restored raw candidate")
	}
	p, err := s.HarnessConfigChanges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.CandidateID == "" || p.CandidateID == c.ID {
		t.Fatal("reboot did not require fresh review")
	}
}
func TestCaptureRebootReceiptUsesHashesAndAcceptedPatch(t *testing.T) {
	for _, newer := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-login", true: "newer-login"}[newer], func(t *testing.T) {
			s, observed, _ := captureFixture(t)
			enableCaptureCheckpoint(t, s)
			observed.Credentials = []hc.Credential{{Provider: "test", EnvName: "TOKEN", Type: "token", LocalID: "auth.json", Generation: "one", Value: json.RawMessage(`{"token":"first"}`)}}
			c := frozenCapture(t, s)
			ack := hc.Acknowledgment{CandidateID: c.ID, Baseline: hc.Baseline{HarnessConfigID: "config", ConfigRevision: 3}, Files: append([]hc.File{}, observed.Files...)}
			ack.Files = append(ack.Files, hc.File{Path: "remote-only.json", Content: `{"remote":true}`})
			restartCapture(t, s, true)
			if newer {
				observed.Credentials[0].Generation = "two"
				observed.Credentials[0].Value = json.RawMessage(`{"token":"second"}`)
			}
			called := false
			run := s.capture.run
			s.capture.run = func(ctx context.Context, args []string, input, output any) error {
				if args[0] == "/handoff" {
					called = true
					h := input.(hc.Handoff)
					if len(h.Credentials) != 1 || string(h.Credentials[0].Value) != `{"token":"first"}` {
						t.Fatal("wrong recovered credential")
					}
				}
				return run(ctx, args, input, output)
			}
			p, err := s.AcknowledgeHarnessConfig(context.Background(), ack)
			if err != nil {
				t.Fatal(err)
			}
			if p.State != "saved" || called == newer {
				t.Fatalf("handoff: state=%s called=%v", p.State, called)
			}
			if len(s.capture.state.Baseline.Files) != 1 || s.capture.state.Baseline.Files[0].Content != observed.Files[0].Content || s.capture.writeRevision() != 3 {
				t.Fatal("recovered wrong baseline patch")
			}
			restartCapture(t, s, true)
			p, err = s.AcknowledgeHarnessConfig(context.Background(), ack)
			if err != nil || p.State != "saved" {
				t.Fatalf("duplicate ack: %v %s", err, p.State)
			}
			if newer {
				p, err = s.HarnessConfigChanges(context.Background())
				if err != nil || p.CandidateID == "" {
					t.Fatal("newer login was lost")
				}
			}
		})
	}
}
func TestCaptureRebootRetriesPersistedReceipt(t *testing.T) {
	s, observed, outcome := captureFixture(t)
	enableCaptureCheckpoint(t, s)
	c := frozenCapture(t, s)
	*outcome = "deferred"
	ack := hc.Acknowledgment{CandidateID: c.ID, Baseline: hc.Baseline{HarnessConfigID: "config", ConfigRevision: 2}, Files: observed.Files}
	p, err := s.AcknowledgeHarnessConfig(context.Background(), ack)
	if err != nil || p.State != "saved-handoff-pending" {
		t.Fatalf("ack %v %s", err, p.State)
	}
	restartCapture(t, s, true)
	if s.capture.state.Receipt == nil {
		t.Fatal("lost committed receipt")
	}
	*outcome = "applied"
	if _, err = s.HarnessConfigChanges(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.capture.state.Receipt != nil || s.capture.writeRevision() != 2 {
		t.Fatal("receipt not reconciled")
	}
}
func TestCaptureCheckpointWinsInterruptedSpoolWrite(t *testing.T) {
	s, _, _ := captureFixture(t)
	enableCaptureCheckpoint(t, s)
	_ = frozenCapture(t, s)
	s.capture.state.LastAcknowledged = "finished"
	s.capture.state.Candidate = nil
	s.capture.state.History = nil
	if err := s.capture.persistCheckpoint(); err != nil {
		t.Fatal(err)
	}
	restartCapture(t, s, false)
	if s.capture.state.Candidate != nil || s.capture.state.LastAcknowledged != "finished" {
		t.Fatal("stale spool overrode checkpoint")
	}
}
func TestCaptureMigratesLegacySpoolWithoutSecretBackup(t *testing.T) {
	s, observed, _ := captureFixture(t)
	observed.Credentials = []hc.Credential{{Provider: "test", EnvName: "TOKEN", Type: "token", LocalID: "auth.json", Generation: "one", Value: json.RawMessage(`{"token":"legacy-secret"}`)}}
	c := frozenCapture(t, s)
	legacy := s.capture.path
	s.capture.recoveryPath = filepath.Join(filepath.Dir(legacy), "checkpoint.json")
	s.capture.path = filepath.Join(t.TempDir(), "state.json")
	staging := filepath.Join(filepath.Dir(legacy), ".capture-interrupted")
	if err := os.WriteFile(staging, []byte("legacy-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	s.capture.state = captureState{}
	if err := s.capture.loadCaptureState(); err != nil {
		t.Fatal(err)
	}
	if s.capture.state.Candidate == nil || s.capture.state.Candidate.ID != c.ID {
		t.Fatal("migration lost live candidate")
	}
	for _, path := range []string{legacy, staging} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("legacy secret copy remains")
		}
	}
	data, err := os.ReadFile(s.capture.recoveryPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("legacy-secret")) {
		t.Fatal("migration retained durable token")
	}
}
func TestCaptureRebootSupersededCredentialRemainsSuppressed(t *testing.T) {
	s, observed, _ := captureFixture(t)
	enableCaptureCheckpoint(t, s)
	observed.Credentials = []hc.Credential{{Provider: "test", EnvName: "TOKEN", Type: "token", LocalID: "auth.json", Generation: "one", Value: json.RawMessage(`{"token":"first"}`)}}
	c := frozenCapture(t, s)
	restartCapture(t, s, true)
	if _, err := s.AcknowledgeHarnessConfig(context.Background(), hc.Acknowledgment{CandidateID: c.ID, Superseded: true}); err != nil {
		t.Fatal(err)
	}
	restartCapture(t, s, true)
	p, err := s.HarnessConfigChanges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.State != "superseded" || p.CandidateID != "" {
		t.Fatalf("stale login reoffered: %s", p.State)
	}
	observed.Credentials[0].Value = json.RawMessage(`{"token":"new-login"}`)
	p, err = s.HarnessConfigChanges(context.Background())
	if err != nil || p.CandidateID == "" {
		t.Fatal("new login suppressed")
	}
}

func TestCaptureBoundsAbandonedRecoveryAndAcknowledgesWithoutWriting(t *testing.T) {
	s, observed, _ := captureFixture(t)
	enableCaptureCheckpoint(t, s)
	first := frozenCapture(t, s)
	for n := 0; n < maxCaptureRecovery+1; n++ {
		restartCapture(t, s, true)
		_ = frozenCapture(t, s)
	}
	if len(s.capture.state.Recovery) > maxCaptureRecovery {
		t.Fatal("unbounded recovery descriptors")
	}
	if _, ok := s.capture.state.Recovery[first.ID]; ok {
		t.Fatal("old descriptor retained")
	}
	run := s.capture.run
	s.capture.run = func(ctx context.Context, args []string, input, output any) error {
		if args[0] == "/handoff" {
			t.Fatal("unknown generation overwrote local files")
		}
		return run(ctx, args, input, output)
	}
	ack := hc.Acknowledgment{CandidateID: first.ID, Baseline: hc.Baseline{HarnessConfigID: "config", ConfigRevision: 42}, Files: observed.Files}
	for range 2 {
		p, err := s.AcknowledgeHarnessConfig(context.Background(), ack)
		if err != nil || p.State != "saved" {
			t.Fatalf("old ack stuck: %v %s", err, p.State)
		}
	}
	if s.capture.writeRevision() != 42 || s.capture.state.Baseline.Files[0].Content != `{"model":"old"}` {
		t.Fatal("unknown acknowledgment changed local baseline")
	}
	p, err := s.HarnessConfigChanges(context.Background())
	if err != nil || p.CandidateID == "" {
		t.Fatal("remaining local change needs fresh review")
	}
}

func TestCapturePendingReceiptDoesNotRegressNewerWriteRevision(t *testing.T) {
	s, observed, outcome := captureFixture(t)
	enableCaptureCheckpoint(t, s)
	c := frozenCapture(t, s)
	*outcome = "deferred"
	ack := hc.Acknowledgment{CandidateID: c.ID, Baseline: hc.Baseline{HarnessConfigID: "config", ConfigRevision: 2}, Files: observed.Files}
	if _, err := s.AcknowledgeHarnessConfig(context.Background(), ack); err != nil {
		t.Fatal(err)
	}
	unknown := hc.Acknowledgment{CandidateID: "evicted", Baseline: hc.Baseline{HarnessConfigID: "config", ConfigRevision: 3}}
	if _, err := s.AcknowledgeHarnessConfig(context.Background(), unknown); err != nil {
		t.Fatal(err)
	}
	*outcome = "applied"
	if _, err := s.HarnessConfigChanges(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.capture.writeRevision() != 3 {
		t.Fatal("older pending receipt regressed server revision")
	}
}
