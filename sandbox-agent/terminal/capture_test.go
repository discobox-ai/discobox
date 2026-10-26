package terminal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	hc "github.com/discobox-ai/discobox/pkg/harnesscapture"
	"github.com/discobox-ai/discobox/sandbox-agent/config"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
)

func captureFixture(t *testing.T) (*Service, *hc.Export, *string) {
	t.Helper()
	home := t.TempDir()
	state := t.TempDir()
	d := hc.Definition{Version: 1, Watch: []string{"settings.json", "auth.json"}, CredentialFiles: []string{"auth.json"}, ExportCommand: []string{"/export"}, HandoffCommand: []string{"/handoff"}, Settings: []hc.Setting{{Path: "settings.json", Format: "json", Fields: map[string]string{"model": "string"}}}, Providers: []hc.Provider{{ID: "test", EnvName: "TOKEN", Types: []string{"token"}}}}
	baseline := hc.File{Path: "settings.json", Content: `{"model":"old"}`}
	observed := &hc.Export{Version: 1, Files: []hc.File{{Path: "settings.json", Content: `{"model":"new"}`}}}
	outcome := "applied"
	m := &captureManager{definition: d, path: filepath.Join(state, "state.json"), home: home, state: captureState{Baseline: hc.Baseline{HarnessConfigID: "config", ConfigRevision: 1, Files: []hc.File{baseline}}, History: map[string]hc.Candidate{}, State: "clean"}}
	m.run = func(_ context.Context, args []string, input, output any) error {
		if args[0] == "/handoff" {
			output.(*hc.HandoffResult).State = outcome
			return nil
		}
		target := output.(*hc.Export)
		if input.(hc.ExportInput).Reconsider != nil {
			*target = hc.Export{Version: 1}
			return nil
		}
		if input.(hc.ExportInput).BaselineOnly {
			*target = hc.Export{Version: 1, Files: m.state.Baseline.Files}
		} else {
			data, err := json.Marshal(observed)
			if err != nil {
				return err
			}
			if err = json.Unmarshal(data, target); err != nil {
				return err
			}
		}
		return nil
	}
	return &Service{capture: m}, observed, &outcome
}
func TestCaptureUsesDeliveredBaselineAndPersistsCandidate(t *testing.T) {
	s, _, _ := captureFixture(t)
	ctx := context.Background()
	p, err := s.HarnessConfigChanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != "pending" || p.CandidateID == "" || len(p.Files) != 1 {
		t.Fatalf("preview: %+v", p)
	}
	c, err := s.CaptureHarnessConfig(ctx, p.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Baseline.ConfigRevision != 1 || c.Files[0].File.Content != `{"model":"new"}` {
		t.Fatalf("candidate: %+v", c)
	}
	var restored captureState
	data, err := os.ReadFile(s.capture.path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Candidate.ID != p.CandidateID {
		t.Fatal("candidate changed across persistence")
	}
	info, _ := os.Stat(s.capture.path)
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("private state mode %v", info.Mode())
	}
	p2, _ := s.HarnessConfigChanges(ctx)
	if p2.CandidateID != p.CandidateID {
		t.Fatal("unchanged content minted another candidate")
	}
}
func TestCapturePreservesCredentialBeforeAckAndRestartsHandoff(t *testing.T) {
	s, observed, outcome := captureFixture(t)
	ctx := context.Background()
	observed.Credentials = []hc.Credential{{Provider: "test", EnvName: "TOKEN", Type: "token", LocalID: "auth.json", Generation: "v1", Value: json.RawMessage(`{"token":"new-secret"}`)}}
	p, err := s.HarnessConfigChanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !s.capture.protect("auth.json", false) {
		t.Fatal("pending credential is not protected")
	}
	wire, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "new-secret") || strings.Contains(string(wire), "v1") {
		t.Fatal("preview leaked private material")
	}
	*outcome = "retryable-failure"
	ack := hc.Acknowledgment{CandidateID: p.CandidateID, Baseline: hc.Baseline{HarnessConfigID: "config", ConfigRevision: 2, Files: observed.Files}, Files: []hc.File{{Path: "auth.json", Template: true, Content: `{"token":{{json .secrets.TOKEN}}}`}}, Sentinels: map[string]string{"TOKEN": "sentinel-new"}}
	p, err = s.AcknowledgeHarnessConfig(ctx, ack)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != "saved-handoff-pending" {
		t.Fatalf("state %s", p.State)
	}
	data, err := os.ReadFile(s.capture.path)
	if err != nil {
		t.Fatal(err)
	}
	s.capture.state = captureState{}
	if err = json.Unmarshal(data, &s.capture.state); err != nil {
		t.Fatal(err)
	}
	if s.capture.state.Receipt == nil {
		t.Fatal("lost receipt on restart")
	}
	*outcome = "applied"
	observed.Credentials = []hc.Credential{{Provider: "test", EnvName: "TOKEN", Type: "token", Keep: true}}
	p, err = s.HarnessConfigChanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.capture.state.Receipt != nil || s.capture.writeRevision() != 2 {
		t.Fatal("receipt was not retried")
	}
	if s.capture.sentinels()["TOKEN"] != "sentinel-new" {
		t.Fatal("accepted sentinel not retained")
	}
	if p.State != "clean" {
		t.Fatalf("delivery reoffered as edit: %+v", p)
	}
}
func TestCaptureAcknowledgmentPreservesNewerLocalLogin(t *testing.T) {
	s, observed, outcome := captureFixture(t)
	ctx := context.Background()
	observed.Credentials = []hc.Credential{{Provider: "test", EnvName: "TOKEN", Type: "token", LocalID: "auth.json", Generation: "v1", Value: json.RawMessage(`{"token":"first"}`)}}
	first, err := s.HarnessConfigChanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CaptureHarnessConfig(ctx, first.CandidateID); err != nil {
		t.Fatal(err)
	}
	observed.Credentials[0].Generation = "v2"
	observed.Credentials[0].Value = json.RawMessage(`{"token":"second"}`)
	newer, err := s.HarnessConfigChanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if newer.CandidateID == first.CandidateID {
		t.Fatal("changed login retained candidate id")
	}
	*outcome = "local-conflict"
	_, err = s.AcknowledgeHarnessConfig(ctx, hc.Acknowledgment{CandidateID: first.CandidateID, Baseline: hc.Baseline{HarnessConfigID: "config", ConfigRevision: 2, Files: observed.Files}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.HarnessConfigChanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != "pending" || len(p.Credentials) != 1 || !s.capture.protect("auth.json", false) {
		t.Fatalf("new login lost: %+v", p)
	}
}
func TestDiscardDoesNotDestroyLocalLogin(t *testing.T) {
	s, observed, _ := captureFixture(t)
	ctx := context.Background()
	observed.Credentials = []hc.Credential{{Provider: "test", EnvName: "TOKEN", Type: "token", Value: json.RawMessage(`{"token":"secret"}`)}}
	p, err := s.HarnessConfigChanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DiscardHarnessConfig(ctx, p.CandidateID); err != nil {
		t.Fatal(err)
	}
	p, _ = s.HarnessConfigChanges(ctx)
	if p.State != "discarded" || p.CandidateID != "" {
		t.Fatalf("discard: %+v", p)
	}
	if !s.capture.protect("auth.json", false) {
		t.Fatal("discard permits login destruction")
	}
}
func TestFailedExporterPreventsInstallationOverwrite(t *testing.T) {
	s, _, _ := captureFixture(t)
	s.capture.run = func(context.Context, []string, any, any) error { return errors.New("invalid login") }
	file := filepath.Join(s.capture.home, "auth.json")
	if err := os.WriteFile(file, []byte("partial-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	i := FileInstaller{Capture: s.capture, HomeDirectory: s.capture.home}
	err := i.EnsureInstalled(context.Background(), config.Harness{Files: []config.HarnessFile{{Path: "auth.json", Content: "sentinel"}}}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(file)
	if string(data) != "partial-secret" {
		t.Fatal("failed export allowed credential overwrite")
	}
}
func TestCaptureProjectOwnedSettingsNeverOffered(t *testing.T) {
	s, _, _ := captureFixture(t)
	s.capture.state.Baseline.ProjectFiles = []string{"settings.json"}
	p, err := s.HarnessConfigChanges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.State != "clean" || len(p.Files) != 0 {
		t.Fatalf("project file offered: %+v", p)
	}
}

func TestCaptureLockCannotFollowSymlinkOutsideHome(t *testing.T) {
	s, _, _ := captureFixture(t)
	outside := t.TempDir()
	target := filepath.Join(outside, "victim")
	if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.capture.home, ".config", "discobox")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "harness-capture.lock")); err != nil {
		t.Skip(err)
	}
	if release, err := s.capture.lock(context.Background()); err == nil {
		release()
		t.Fatal("lock followed escaping symlink")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "unchanged" {
		t.Fatal("outside file modified")
	}
}
func TestInvalidCredentialDoesNotBecomeSaveableOnSecondObservation(t *testing.T) {
	s, observed, _ := captureFixture(t)
	ctx := context.Background()
	observed.Credentials = []hc.Credential{{Provider: "test", EnvName: "TOKEN", Type: "token", Value: json.RawMessage(`{"token":"first"}`)}}
	first, err := s.HarnessConfigChanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	observed.Credentials[0].Value = json.RawMessage(`{}`)
	for range 2 {
		p, err := s.HarnessConfigChanges(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if p.State != "unavailable" {
			t.Fatalf("invalid export state: %s", p.State)
		}
		if _, err = s.CaptureHarnessConfig(ctx, first.CandidateID); !errors.Is(err, ErrCaptureUnavailable) {
			t.Fatalf("invalid generation allowed old save: %v", err)
		}
	}
}

func TestPartialSettingsAreNotRemovedFromSavedConfig(t *testing.T) {
	s, observed, _ := captureFixture(t)
	observed.Files = nil
	observed.Diagnostics = []hc.Diagnostic{{Path: "settings.json", Code: "partial", Message: "secret input must not leak"}}
	p, err := s.HarnessConfigChanges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Files) != 0 {
		t.Fatal("partial file offered as removal")
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret input") {
		t.Fatal("diagnostic leaked parser detail")
	}
}

func TestTerminalRecreationPreservesDeletedSettings(t *testing.T) {
	s, observed, _ := captureFixture(t)
	s.capture.state.Installed = true
	observed.Files = nil
	i := FileInstaller{Capture: s.capture, HomeDirectory: s.capture.home}
	if err := i.EnsureInstalled(context.Background(), config.Harness{Files: []config.HarnessFile{{Path: "settings.json", Content: `{"model":"old"}`}}}, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.capture.home, "settings.json")); !os.IsNotExist(err) {
		t.Fatal("deleted setting was recreated")
	}
	p := s.HarnessConfigStatus()
	if len(p.Files) != 1 || !p.Files[0].Remove {
		t.Fatalf("deletion lost: %+v", p)
	}
}

func TestCaptureStatusDoesNotWaitForExporter(t *testing.T) {
	s, _, _ := captureFixture(t)
	ctx := context.Background()
	first, err := s.HarnessConfigChanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	started, finish, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	previous := s.capture.run
	s.capture.run = func(ctx context.Context, args []string, input, output any) error {
		close(started)
		<-finish
		return previous(ctx, args, input, output)
	}
	go func() { defer close(done); _, _ = s.HarnessConfigChanges(ctx) }()
	<-started
	status := make(chan hc.Preview, 1)
	go func() { status <- s.HarnessConfigStatus() }()
	select {
	case got := <-status:
		if got.CandidateID != first.CandidateID {
			t.Error("status did not retain last completed snapshot")
		}
	case <-time.After(time.Second):
		t.Error("status blocked behind exporter")
	}
	close(finish)
	<-done
}

func TestReconsiderAcknowledgmentDoesNotAdoptUndeliveredRemoteSettings(t *testing.T) {
	s, observed, _ := captureFixture(t)
	ctx := t.Context()
	s.capture.definition.Settings = append(s.capture.definition.Settings, hc.Setting{Path: "other.json", Format: "json", Fields: map[string]string{"model": "string"}})
	other := hc.File{Path: "other.json", Content: `{"model":"original"}`}
	s.capture.state.Baseline.Files = append(s.capture.state.Baseline.Files, other)
	observed.Files = append(observed.Files, other)
	first, err := s.HarnessConfigChanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	remote := hc.Baseline{HarnessConfigID: "config", ConfigRevision: 2, Files: []hc.File{{Path: "settings.json", Content: `{"model":"old"}`}, {Path: "other.json", Content: `{"model":"remote"}`}}}
	reconsidered, err := s.ReconsiderHarnessConfig(ctx, hc.Reconsideration{Selection: hc.Selection{CandidateID: first.CandidateID, KeepFiles: []string{"settings.json"}}, Current: remote})
	if err != nil {
		t.Fatal(err)
	}
	remote.ConfigRevision = 3
	remote.Files[0] = observed.Files[0]
	if _, err = s.AcknowledgeHarnessConfig(ctx, hc.Acknowledgment{CandidateID: reconsidered.CandidateID, Baseline: remote}); err != nil {
		t.Fatal(err)
	}
	var restored captureState
	data, err := os.ReadFile(s.capture.path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	s.capture.state = restored
	after, err := s.HarnessConfigChanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "clean" || len(after.Files) != 0 {
		t.Fatalf("undelivered remote setting reoffered: %+v", after)
	}
	if s.capture.state.Baseline.ConfigRevision != 1 || s.capture.writeRevision() != 3 {
		t.Fatal("delivered provenance and write revision were conflated")
	}
}
func TestCompletedCapturePurgesReusableSecretsAcrossRestart(t *testing.T) {
	for _, action := range []string{"acknowledge", "discard"} {
		t.Run(action, func(t *testing.T) {
			s, observed, _ := captureFixture(t)
			ctx := t.Context()
			observed.Credentials = []hc.Credential{{Provider: "test", EnvName: "TOKEN", Type: "token", Value: json.RawMessage(`{"token":"purge-this-reusable-token"}`)}}
			first, err := s.HarnessConfigChanges(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.CaptureHarnessConfig(ctx, first.CandidateID); err != nil {
				t.Fatal(err)
			}
			orphan := filepath.Join(filepath.Dir(s.capture.path), ".capture-abandoned")
			if err = os.WriteFile(orphan, []byte("purge-this-reusable-token"), 0600); err != nil {
				t.Fatal(err)
			}
			if action == "discard" {
				_, err = s.DiscardHarnessConfig(ctx, first.CandidateID)
			} else {
				_, err = s.AcknowledgeHarnessConfig(ctx, hc.Acknowledgment{CandidateID: first.CandidateID, Baseline: hc.Baseline{HarnessConfigID: "config", ConfigRevision: 2, Files: observed.Files}})
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(s.capture.path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "purge-this-reusable-token") {
				t.Fatal("completed candidate left reusable material in spool")
			}
			if _, err = os.Stat(orphan); !os.IsNotExist(err) {
				t.Fatal("orphan staging file survived completion")
			}
			var restored captureState
			if err = json.Unmarshal(data, &restored); err != nil {
				t.Fatal(err)
			}
			s.capture.state = restored
			if action == "acknowledge" {
				observed.Credentials = []hc.Credential{{Provider: "test", EnvName: "TOKEN", Type: "token", Keep: true}}
			}
			p, err := s.HarnessConfigChanges(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if p.CandidateID != "" {
				t.Fatal("completed credential resurrected after restart")
			}
			data, err = os.ReadFile(s.capture.path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "purge-this-reusable-token") {
				t.Fatal("restart retained discarded material")
			}
			if action == "discard" && !s.capture.protect("auth.json", false) {
				t.Fatal("discard stopped protecting local login")
			}
		})
	}
}
func TestReconsiderCouplesCredentialAndDeliveryWithoutKeepFile(t *testing.T) {
	s, observed, _ := captureFixture(t)
	ctx := t.Context()
	s.capture.definition.CredentialSettings = []hc.Setting{{Path: "auth.json", Format: "json", Fields: map[string]string{"token": "secret"}}}
	auth := hc.File{Path: "auth.json", Template: true, Content: `{"token":"{{ .secrets.TOKEN }}"}`}
	observed.Files = append(observed.Files, auth)
	credential := hc.Credential{Provider: "test", EnvName: "TOKEN", Type: "token", Generation: "captured", Value: json.RawMessage(`{"token":"immutable-login"}`)}
	observed.Credentials = []hc.Credential{credential}
	previous := s.capture.run
	s.capture.run = func(ctx context.Context, args []string, input, output any) error {
		in, ok := input.(hc.ExportInput)
		if ok && in.Reconsider != nil {
			if len(in.Reconsider.Candidate.Credentials) != 1 || string(in.Reconsider.Candidate.Credentials[0].Value) != string(credential.Value) {
				t.Error("projection did not receive immutable credential")
			}
			*output.(*hc.Export) = hc.Export{Version: 1, Files: []hc.File{auth}, Credentials: []hc.Credential{credential}}
			return nil
		}
		return previous(ctx, args, input, output)
	}
	first, err := s.HarnessConfigChanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.ReconsiderHarnessConfig(ctx, hc.Reconsideration{Selection: hc.Selection{CandidateID: first.CandidateID, ReplaceCredentials: []string{"TOKEN"}}, Current: hc.Baseline{HarnessConfigID: "config", ConfigRevision: 2}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.CaptureHarnessConfig(ctx, p.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Files) != 1 || c.Files[0].Path != "auth.json" || len(c.Credentials) != 1 {
		t.Fatalf("credential delivery separated from credential: %+v", c)
	}
	if _, err = s.ReconsiderHarnessConfig(ctx, hc.Reconsideration{Selection: hc.Selection{CandidateID: p.CandidateID, KeepFiles: []string{"auth.json"}}, Current: hc.Baseline{HarnessConfigID: "config", ConfigRevision: 2}}); !errors.Is(err, ErrCaptureConflict) {
		t.Fatal("KeepFiles accepted credential template independently")
	}
}

func TestAcknowledgmentRetainsConcurrentFrozenHandoffWithoutAcceptedSecret(t *testing.T) {
	s, _, _ := captureFixture(t)
	shared := hc.Credential{Provider: "test", EnvName: "TOKEN", Type: "token", LocalID: "shared", Generation: "g1", Value: json.RawMessage(`{"token":"accepted-material"}`)}
	newer := hc.Credential{Provider: "other", EnvName: "OTHER", Type: "token", LocalID: "newer", Generation: "g2", Value: json.RawMessage(`{"token":"still-pending-material"}`)}
	first := hc.Candidate{ID: "first", Credentials: []hc.Credential{shared}}
	second := hc.Candidate{ID: "second", Credentials: []hc.Credential{shared, newer}}
	s.capture.state.History = map[string]hc.Candidate{"first": first, "second": second}
	s.capture.state.Frozen = map[string]bool{"first": true, "second": true}
	s.capture.state.Candidate = &second
	s.capture.purge(first)
	if err := s.capture.persist(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.capture.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "accepted-material") {
		t.Fatal("accepted shared material survived")
	}
	var restored captureState
	if err = json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	retained, ok := restored.History[second.ID]
	if !ok {
		t.Fatal("concurrent frozen handoff lost")
	}
	if retained.Credentials[0].Generation != "g1" || retained.Credentials[0].LocalID != "shared" || len(retained.Credentials[0].Value) != 0 {
		t.Fatal("accepted handoff metadata not retained safely")
	}
	if string(retained.Credentials[1].Value) != string(newer.Value) {
		t.Fatal("distinct pending credential was purged")
	}
	s.capture.state = restored
	if _, err = s.AcknowledgeHarnessConfig(t.Context(), hc.Acknowledgment{CandidateID: second.ID, Baseline: hc.Baseline{HarnessConfigID: "config", ConfigRevision: 2}}); err != nil {
		t.Fatalf("concurrent handoff cannot complete: %v", err)
	}
}

func TestReconsiderUsesRealHarnessCredentialProjection(t *testing.T) {
	for _, inherited := range []bool{false, true} {
		name := "changed-template"
		if inherited {
			name = "unchanged-delivered-template"
		}
		t.Run(name, func(t *testing.T) { testReconsiderUsesRealHarnessCredentialProjection(t, inherited) })
	}
}
func testReconsiderUsesRealHarnessCredentialProjection(t *testing.T, inherited bool) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the bundle protocol regression")
	}
	s, observed, _ := captureFixture(t)
	ctx := t.Context()
	root, err := filepath.Abs("../../harness")
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := os.ReadFile(filepath.Join(root, "pi", "image.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Harness struct {
			SaveBack hc.Definition `json:"saveBack"`
		} `json:"harness"`
	}
	if err = json.Unmarshal(metadata, &manifest); err != nil {
		t.Fatal(err)
	}
	s.capture.definition = manifest.Harness.SaveBack
	s.capture.state.Baseline.Files = nil
	observed.Files = []hc.File{{Path: ".pi/agent/auth.json", Template: true, Content: `{"anthropic":{"type":"api_key","key":"{{ .secrets.PI_ANTHROPIC_CREDENTIAL }}"},"openai":{"type":"api_key","key":"{{ .secrets.PI_OPENAI_CREDENTIAL }}"}}`}}
	if inherited {
		s.capture.state.Baseline.Files = slices.Clone(observed.Files)
	}
	observed.Credentials = []hc.Credential{{Provider: "anthropic", EnvName: "PI_ANTHROPIC_CREDENTIAL", Type: "token", Generation: "skip", Value: json.RawMessage(`{"token":"rejected-login"}`)}, {Provider: "openai", EnvName: "PI_OPENAI_CREDENTIAL", Type: "token", LocalID: "openai", Generation: "immutable", Value: json.RawMessage(`{"token":"selected-login"}`)}}
	previous := s.capture.run
	s.capture.run = func(ctx context.Context, _ []string, input, output any) error {
		in, ok := input.(hc.ExportInput)
		if !ok || in.Reconsider == nil {
			return previous(ctx, []string{"/export"}, input, output)
		}
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		script := `const fs=require('fs'),p=require('path'),root=process.argv[1]; const runtime=require(p.join(root,'capture/runtime.cjs')),normalizer=require(p.join(root,'pi/pi-normalize.cjs')),definition=require(p.join(root,'pi/image.json')).harness.saveBack; process.stdout.write(JSON.stringify(runtime.capture('pi',definition,normalizer,JSON.parse(fs.readFileSync(0,'utf8')))));`
		cmd := exec.CommandContext(ctx, node, "-e", script, root)
		cmd.Stdin = bytes.NewReader(data)
		cmd.Env = append(os.Environ(), "HOME="+s.capture.home)
		result, err := cmd.Output()
		if err != nil {
			return errors.New("harness projection failed")
		}
		return json.Unmarshal(result, output)
	}
	p, err := s.HarnessConfigChanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if inherited && len(p.Files) != 0 {
		t.Fatal("unchanged credential template should not appear in candidate diff")
	}
	if p.State != "pending" {
		t.Fatalf("capture state %s", p.State)
	}
	current := hc.Baseline{HarnessConfigID: "config", ConfigRevision: 2, Files: []hc.File{{Path: ".pi/agent/auth.json", Template: true, Content: `{"google":{"type":"api_key","key":"{{ .secrets.PI_GOOGLE_CREDENTIAL }}"}}`}}}
	next, err := s.ReconsiderHarnessConfig(ctx, hc.Reconsideration{Current: current, Selection: hc.Selection{CandidateID: p.CandidateID, ReplaceCredentials: []string{"PI_OPENAI_CREDENTIAL"}}})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := s.CaptureHarnessConfig(ctx, next.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidate.Files) != 1 || candidate.Files[0].File == nil {
		t.Fatal("selected login has no delivery")
	}
	content := candidate.Files[0].File.Content
	if strings.Contains(content, "ANTHROPIC") || !strings.Contains(content, "OPENAI") || !strings.Contains(content, "GOOGLE") {
		t.Fatalf("projection failed to couple selected login and current binding: %s", content)
	}
	if len(candidate.Credentials) != 2 {
		t.Fatalf("unexpected projected credentials: %d", len(candidate.Credentials))
	}
	for _, credential := range candidate.Credentials {
		if credential.EnvName == "PI_OPENAI_CREDENTIAL" && (credential.Generation != "immutable" || string(credential.Value) != `{"token":"selected-login"}`) {
			t.Fatal("selected snapshot changed during projection")
		}
	}
}

func TestReconsiderRefusesChangedDefinitionBeforeExport(t *testing.T) {
	s, _, _ := captureFixture(t)
	s.capture.state.Baseline.ManifestDigest = "installed"
	s.capture.run = func(context.Context, []string, any, any) error {
		t.Error("old exporter used for changed definition")
		return nil
	}
	_, err := s.ReconsiderHarnessConfig(t.Context(), hc.Reconsideration{Current: hc.Baseline{ManifestDigest: "changed"}})
	if !errors.Is(err, ErrCaptureDefinitionChanged) {
		t.Fatalf("definition mismatch: %v", err)
	}
}
func TestSupersededReceiptPurgesSpoolWithoutOverwritingLogin(t *testing.T) {
	s, observed, _ := captureFixture(t)
	ctx := t.Context()
	credential := hc.Credential{Provider: "test", EnvName: "TOKEN", Type: "token", LocalID: "auth", Generation: "old", Value: json.RawMessage(`{"token":"superseded-material"}`)}
	observed.Credentials = []hc.Credential{credential}
	local := filepath.Join(s.capture.home, "auth.json")
	if err := os.WriteFile(local, []byte("local-login"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := s.HarnessConfigChanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CaptureHarnessConfig(ctx, first.CandidateID); err != nil {
		t.Fatal(err)
	}
	previous := s.capture.run
	s.capture.run = func(ctx context.Context, args []string, input, output any) error {
		if args[0] == "/handoff" {
			t.Fatal("supersession attempted credential handoff")
		}
		return previous(ctx, args, input, output)
	}
	p, err := s.AcknowledgeHarnessConfig(ctx, hc.Acknowledgment{CandidateID: first.CandidateID, Superseded: true, Baseline: hc.Baseline{HarnessConfigID: "another-config", ConfigRevision: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if p.State != "superseded" || p.CandidateID != "" || len(p.Diagnostics) == 0 {
		t.Fatalf("terminal outcome: %+v", p)
	}
	data, err := os.ReadFile(s.capture.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "superseded-material") {
		t.Fatal("terminal outcome retained accepted raw material")
	}
	var restored captureState
	if err = json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	s.capture.state = restored
	observed.Files[0].Content = `{"model":"another-local-edit"}`
	p, err = s.HarnessConfigChanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != "superseded" || p.CandidateID != "" {
		t.Fatal("settings edit replayed stale accepted credential")
	}
	disk, err := os.ReadFile(local)
	if err != nil {
		t.Fatal(err)
	}
	if string(disk) != "local-login" {
		t.Fatal("supersession overwrote local login")
	}
	if !s.capture.protect("auth.json", false) {
		t.Fatal("superseded local login lost protection")
	}
	if s.capture.writeRevision() != 1 {
		t.Fatal("supersession adopted remote baseline")
	}
	observed.Credentials[0].Generation = "new"
	observed.Credentials[0].Value = json.RawMessage(`{"token":"fresh-login"}`)
	p, err = s.HarnessConfigChanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != "pending" || p.CandidateID == first.CandidateID || p.CandidateID == "" {
		t.Fatalf("new login cannot be captured: %+v", p)
	}
}
func TestSupersededReceiptPreservesDistinctNewerPendingLogin(t *testing.T) {
	s, observed, _ := captureFixture(t)
	ctx := t.Context()
	observed.Credentials = []hc.Credential{{Provider: "test", EnvName: "TOKEN", Type: "token", Generation: "old", Value: json.RawMessage(`{"token":"old-login"}`)}}
	first, err := s.HarnessConfigChanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CaptureHarnessConfig(ctx, first.CandidateID); err != nil {
		t.Fatal(err)
	}
	observed.Credentials[0].Generation = "new"
	observed.Credentials[0].Value = json.RawMessage(`{"token":"new-login"}`)
	next, err := s.HarnessConfigChanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AcknowledgeHarnessConfig(ctx, hc.Acknowledgment{CandidateID: first.CandidateID, Superseded: true}); err != nil {
		t.Fatal(err)
	}
	current, err := s.CaptureHarnessConfig(ctx, next.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	if string(current.Credentials[0].Value) != `{"token":"new-login"}` {
		t.Fatal("newer login lost to old supersession")
	}
	data, err := os.ReadFile(s.capture.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "old-login") {
		t.Fatal("old material survived supersession")
	}
}
func TestInvalidatingUnsupportedCaptureRemovesOnlyPrivateState(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"state.json", ".capture-interrupted", "unrelated"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("material"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := invalidateCaptureState(dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"state.json", ".capture-interrupted"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("private %s survived invalidation", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "unrelated")); err != nil {
		t.Fatal("invalidation removed unrelated file")
	}
}

func TestAcknowledgedSentinelYieldsToNewRuntimeDelivery(t *testing.T) {
	s, observed, _ := captureFixture(t)
	ctx := t.Context()
	live := map[string]string{"TOKEN": "before"}
	s.capture.secrets = func() map[string]string { return live }
	observed.Credentials = []hc.Credential{{Provider: "test", EnvName: "TOKEN", Type: "token", Keep: true}}
	first, err := s.HarnessConfigChanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	auth := hc.File{Path: "auth.json", Content: `{"token":"{{ .secrets.TOKEN }}"}`, Template: true}
	_, err = s.AcknowledgeHarnessConfig(ctx, hc.Acknowledgment{CandidateID: first.CandidateID, Baseline: hc.Baseline{HarnessConfigID: "config", ConfigRevision: 2}, Files: []hc.File{auth}, Sentinels: map[string]string{"TOKEN": "accepted"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.capture.sentinels()["TOKEN"]; got != "accepted" {
		t.Fatalf("ack bridge = %s", got)
	}
	local := filepath.Join(s.capture.home, "auth.json")
	if err = os.WriteFile(local, []byte(`{"token":"accepted"}`), 0600); err != nil {
		t.Fatal(err)
	}
	live["TOKEN"] = "accepted"
	if _, err = s.HarnessConfigChanges(ctx); err != nil {
		t.Fatal(err)
	}
	if len(s.capture.state.Sentinels) != 0 {
		t.Fatal("runtime catch-up retained ack override")
	}
	live["TOKEN"] = "reminted"
	installer := FileInstaller{Capture: s.capture, HomeDirectory: s.capture.home, Secrets: s.capture.secrets}
	h := config.Harness{Files: []config.HarnessFile{{Path: "auth.json", Content: auth.Content, Template: true}}}
	if _, err = installer.RestoreSecretFiles(ctx, h, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(local)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "reminted") {
		t.Fatalf("runtime remint shadowed: %s", data)
	}
	for _, expected := range []string{"before", "accepted", "reminted"} {
		if !slices.Contains(s.capture.state.KnownSentinels["TOKEN"], expected) {
			t.Fatalf("lost delivered alias %s", expected)
		}
	}
	observed.Credentials = []hc.Credential{{Provider: "test", EnvName: "TOKEN", Type: "token", Generation: "new-login", Value: json.RawMessage(`{"token":"real-local-login"}`)}}
	if err = os.WriteFile(local, []byte("real-local-login"), 0600); err != nil {
		t.Fatal(err)
	}
	live["TOKEN"] = "later-remint"
	if _, err = installer.RestoreSecretFiles(ctx, h, nil); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(local)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "real-local-login" {
		t.Fatal("remint overwrote new local login")
	}
	contextData, err := os.ReadFile(filepath.Join(s.capture.home, ".config/discobox/harness-sentinels.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contextData), "real-local-login") || !strings.Contains(string(contextData), "later-remint") {
		t.Fatal("launcher alias context contains secrets or is stale")
	}
}
func TestSentinelHistoryExhaustionFailsProtected(t *testing.T) {
	s, _, _ := captureFixture(t)
	s.capture.state.KnownSentinels = map[string][]string{"TOKEN": {}}
	for n := range 32 {
		s.capture.state.KnownSentinels["TOKEN"] = append(s.capture.state.KnownSentinels["TOKEN"], fmt.Sprintf("known-%d", n))
	}
	s.capture.secrets = func() map[string]string { return map[string]string{"TOKEN": "overflow"} }
	p, err := s.HarnessConfigChanges(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if p.State != "unavailable" || !s.capture.protect("auth.json", false) {
		t.Fatal("history overflow allowed unsafe capture/restoration")
	}
	if len(s.capture.state.KnownSentinels["TOKEN"]) != 32 {
		t.Fatal("history overflow discarded a known alias")
	}
}

func TestCaptureCommandUsesLiveSecretEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("capture manifests use POSIX executable paths and the sandbox's Unix process runner")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the capture process regression")
	}
	home := t.TempDir()
	live := map[string]string{"ROTATING_TOKEN": "first", "FILE_TOKEN": "file-sentinel"}
	manager, err := execs.NewManagerWithConfig(execs.ManagerConfig{WorkingRoot: home, RuntimeDir: filepath.Join(home, "runtime"), Env: map[string]string{"HOME": home, "PATH": os.Getenv("PATH")}, SecretEnv: func() map[string]string { return live }, FileSecrets: []string{"FILE_TOKEN"}, Units: &fakeUnits{}})
	if err != nil {
		t.Fatal(err)
	}
	fixture, _, _ := captureFixture(t)
	definition := fixture.capture.definition
	definition.ExportCommand = []string{node}
	definition.HandoffCommand = []string{node}
	service, err := NewService(ServiceConfig{Execs: manager, WorkingRoot: home, RuntimeDir: filepath.Join(home, "runtime"), CaptureStateDir: t.TempDir(), ExecDefaults: config.ExecDefaults{HomeDirectory: home}, SecretEnv: func() map[string]string { return live }, Harness: config.Harness{SaveBack: &definition, CaptureBaseline: &hc.Baseline{HarnessConfigID: "config", ConfigRevision: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	script := `process.stdout.write(JSON.stringify({token:process.env.ROTATING_TOKEN||'',file:process.env.FILE_TOKEN||'',home:process.env.HOME}))`
	for _, want := range []string{"first", "refreshed", ""} {
		if want == "" {
			delete(live, "ROTATING_TOKEN")
		} else {
			live["ROTATING_TOKEN"] = want
		}
		var got struct{ Token, File, Home string }
		if err = service.capture.command(t.Context(), []string{node, "-e", script}, struct{}{}, &got); err != nil {
			t.Fatal(err)
		}
		if got.Token != want || got.File != "" || got.Home != home {
			t.Fatalf("capture environment token=%q file=%q home=%q", got.Token, got.File, got.Home)
		}
	}
	if len(manager.List()) != 0 {
		t.Fatal("capture helper created an execution record")
	}
}

func TestPartialBaselineNormalizationIsRetriedAcrossRestart(t *testing.T) {
	s, observed, _ := captureFixture(t)
	observed.Files = slices.Clone(s.capture.state.Baseline.Files)
	previous := s.capture.run
	partial := true
	baselineCalls := 0
	s.capture.run = func(ctx context.Context, args []string, input, output any) error {
		if in, ok := input.(hc.ExportInput); ok && in.BaselineOnly {
			baselineCalls++
			if partial {
				*output.(*hc.Export) = hc.Export{Version: 1, Diagnostics: []hc.Diagnostic{{Path: "settings.json", Code: "partial"}}}
				return nil
			}
		}
		return previous(ctx, args, input, output)
	}
	p, err := s.HarnessConfigChanges(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if p.State != "unavailable" || p.CandidateID != "" || s.capture.state.Normalized.Version != 0 {
		t.Fatal("partial normalization became delivered truth")
	}
	data, err := os.ReadFile(s.capture.path)
	if err != nil {
		t.Fatal(err)
	}
	var restored captureState
	if err = json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	s.capture.state = restored
	partial = false
	p, err = s.HarnessConfigChanges(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if p.State != "clean" || len(p.Files) != 0 || baselineCalls != 2 {
		t.Fatalf("complete baseline not retried: %+v calls=%d", p, baselineCalls)
	}
	if s.capture.state.Baseline.Files[0].Content != observed.Files[0].Content {
		t.Fatal("original delivery was modified during recovery")
	}
}
func TestRecoversPersistedPartialBaselineWithoutFalseCandidate(t *testing.T) {
	s, observed, _ := captureFixture(t)
	observed.Files = slices.Clone(s.capture.state.Baseline.Files)
	// This is the runtime failure: baseline Files contain valid delivered JSON,
	// but an earlier exporter returned version1 plus partial and omitted it.
	s.capture.state.Normalized = hc.Export{Version: 1, Diagnostics: []hc.Diagnostic{{Path: "settings.json", Code: "partial"}}}
	s.capture.state.Observed = publicObservation(*observed)
	digest, err := exportDigest(*observed)
	if err != nil {
		t.Fatal(err)
	}
	s.capture.state.ObservedDigest = digest
	file := observed.Files[0]
	c := hc.Candidate{ID: "false-candidate", Baseline: s.capture.state.Baseline, WriteRevision: 1, Files: []hc.FileChange{{Path: file.Path, File: &file}}}
	s.capture.state.Candidate = &c
	s.capture.state.History[c.ID] = c
	s.capture.state.State = "pending"
	if err = s.capture.persist(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.capture.path)
	if err != nil {
		t.Fatal(err)
	}
	var restored captureState
	if err = json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	s.capture.state = restored
	p, err := s.HarnessConfigChanges(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if p.State != "clean" || p.CandidateID != "" || len(p.Files) != 0 {
		t.Fatalf("persisted partial baseline still offers unchanged delivery: %+v", p)
	}
	if _, ok := s.capture.state.History[c.ID]; ok {
		t.Fatal("invalid unfrozen snapshot survived recovery")
	}
	if len(s.capture.state.Normalized.Files) != 1 || s.capture.state.Normalized.Files[0].Content != file.Content {
		t.Fatal("recovery did not normalize original delivered files")
	}
}
