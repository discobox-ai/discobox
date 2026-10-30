package harnessconfigs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"

	"github.com/discobox-ai/discobox/pkg/harnesscapture"
	"github.com/discobox-ai/discobox/pkg/hostscope"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandboxagentclient"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/x/id"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func captureConflict() error {
	return apperrors.NewStatusError(http.StatusConflict, "harness configuration changed; reconsider the candidate before saving")
}

func captureDefinitionConflict() error {
	return apperrors.NewStatusError(http.StatusConflict, "harness definition changed; upgrade or recreate the sandbox and capture the local configuration again")
}

// captureRequest never includes the agent's error body in an error: capture is
// a credential-bearing transport and even a malformed response is private.
func (s *Service) captureRequest(ctx context.Context, projectID, sandboxID, method, suffix string, input, output any) error {
	if _, err := auth.UserID(ctx); err != nil {
		return apperrors.NewStatusError(http.StatusForbidden, "authenticated user is required")
	}
	if s.sandboxes == nil {
		return apperrors.NewStatusError(http.StatusServiceUnavailable, "sandbox runtime unavailable")
	}
	scope := harnesscapture.ScopeWrite
	if method == http.MethodGet {
		scope = harnesscapture.ScopeRead
	} else if suffix == "/harness-config-capture" {
		scope = harnesscapture.ScopeCapture
	}
	lease, box, err := s.sandboxes.AcquireSandboxHTTPClient(ctx, projectID, sandboxID, []string{scope}, services.SandboxAction("save harness configuration"))
	if err != nil {
		return err
	}
	defer lease.Release()
	return s.sendCaptureRequest(ctx, projectID, sandboxID, method, suffix, input, output, lease, box)
}

func (s *Service) sendCaptureRequest(ctx context.Context, projectID, sandboxID, method, suffix string, input, output any, lease *services.HTTPClientLease, box *model.Sandbox) error {
	payload, err := json.Marshal(input)
	if err != nil {
		return err
	}
	target, err := sandboxagentclient.TargetURL(lease.BaseURL, box.ProjectID, box.PoolID, box.ID, suffix)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := sandboxagentclient.HTTPClient(lease).Do(req)
	if err != nil {
		return apperrors.NewStatusError(http.StatusBadGateway, "harness capture transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusPreconditionFailed {
		return captureDefinitionConflict()
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return apperrors.NewStatusError(resp.StatusCode, "harness capture request was refused")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, harnesscapture.MaxBytes+1))
	if err != nil || len(data) > harnesscapture.MaxBytes {
		return apperrors.NewStatusError(http.StatusBadGateway, "invalid harness capture response")
	}
	if err := json.Unmarshal(data, output); err != nil {
		return apperrors.NewStatusError(http.StatusBadGateway, "invalid harness capture response")
	}
	if preview, ok := output.(*harnesscapture.Preview); ok {
		if err := s.validateCapturePreview(ctx, projectID, sandboxID, preview); err != nil {
			return err
		}
		return s.describeCapturePreview(ctx, projectID, sandboxID, preview)
	}
	return nil
}

// describeCapturePreview sets each change's before and after from the stored
// configuration, so a client can show what a save changes rather than only
// what it writes. Previous is the file a save replaces: the configured file
// over the bundle's, at the current revision, whatever the sandbox was
// delivered. A removal drops the configured file, so its after is the bundle's
// own file at that path, when it ships one. Nothing here is applied.
func (s *Service) describeCapturePreview(ctx context.Context, projectID, sandboxID string, p *harnesscapture.Preview) error {
	if len(p.Files) == 0 {
		return nil
	}
	box, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return err
	}
	if box.HarnessConfigID == nil {
		return captureConflict()
	}
	cfg, err := s.store.GetHarnessConfig(ctx, projectID, *box.HarnessConfigID)
	if err != nil {
		return err
	}
	current, err := s.store.CaptureBaseline(ctx, projectID, cfg.ID)
	if err != nil {
		return err
	}
	for i := range p.Files {
		change := &p.Files[i]
		change.Previous = nil
		if n := slices.IndexFunc(current.Files, func(f harnesscapture.File) bool { return f.Path == change.Path }); n >= 0 {
			previous := current.Files[n]
			change.Previous = &previous
		}
		if !change.Remove {
			continue
		}
		if n := slices.IndexFunc(cfg.Files, func(f model.HarnessConfigFile) bool { return f.Path == change.Path }); n >= 0 {
			f := cfg.Files[n]
			change.File = &harnesscapture.File{Path: f.Path, Content: f.Content, CreateOnly: f.CreateOnly, Template: f.Template}
		}
	}
	return nil
}
func (s *Service) GetSandboxHarnessConfigChanges(ctx context.Context, p, b string) (harnesscapture.Preview, error) {
	var out harnesscapture.Preview
	err := s.captureRequest(ctx, p, b, http.MethodGet, "/harness-config-changes", nil, &out)
	return out, err
}
func (s *Service) DiscardSandboxHarnessConfigChanges(ctx context.Context, p, b string, r harnesscapture.CandidateRequest) (harnesscapture.Preview, error) {
	var out harnesscapture.Preview
	err := s.captureRequest(ctx, p, b, http.MethodPost, "/harness-config-changes/discard", r, &out)
	return out, err
}
func (s *Service) ReconsiderSandboxHarnessConfigChanges(ctx context.Context, p, b string, r harnesscapture.Selection) (harnesscapture.Preview, error) {
	var out harnesscapture.Preview
	box, err := s.store.GetSandbox(ctx, p, b)
	if err != nil {
		return out, err
	}
	if box.HarnessConfigID == nil {
		return out, captureConflict()
	}
	baseline, err := s.store.CaptureBaseline(ctx, p, *box.HarnessConfigID)
	if err != nil {
		return out, err
	}
	err = s.captureRequest(ctx, p, b, http.MethodPost, "/harness-config-reconsider", harnesscapture.Reconsideration{Selection: r, Current: baseline}, &out)
	return out, err
}
func (s *Service) SaveSandboxHarnessConfigChanges(ctx context.Context, p, b string, r harnesscapture.CandidateRequest) (harnesscapture.SaveResult, error) {
	if _, err := auth.UserID(ctx); err != nil {
		return harnesscapture.SaveResult{}, apperrors.NewStatusError(http.StatusForbidden, "authenticated user is required")
	}
	box, err := s.store.GetSandbox(ctx, p, b)
	if err != nil {
		return harnesscapture.SaveResult{}, err
	}
	if box.HarnessConfigID == nil {
		return harnesscapture.SaveResult{}, captureConflict()
	}
	configID := *box.HarnessConfigID
	receipt, err := s.store.GetHarnessCaptureReceipt(ctx, p, b, configID, r.CandidateID)
	if errors.Is(err, store.ErrNotFound) {
		var candidate harnesscapture.Candidate
		if err = s.captureRequest(ctx, p, b, http.MethodPost, "/harness-config-capture", harnesscapture.CandidateRequest{CandidateID: r.CandidateID}, &candidate); err != nil {
			return harnesscapture.SaveResult{}, err
		}
		if candidate.ID != r.CandidateID || candidate.ID == "" {
			return harnesscapture.SaveResult{}, captureConflict()
		}
		receipt, err = s.commitCapture(ctx, p, b, configID, candidate, r.Force)
	}
	if err != nil {
		return harnesscapture.SaveResult{}, err
	}
	result := harnesscapture.SaveResult{CandidateID: r.CandidateID, HarnessConfigID: configID, ConfigRevision: receipt.Baseline.ConfigRevision, State: "saved"}
	state, ackErr := s.deliverCaptureAcknowledgment(ctx, receipt)
	if ackErr != nil {
		result.State = "saved-handoff-pending"
	} else {
		result.State = state
	}

	return result, nil
}

func (s *Service) commitCapture(ctx context.Context, p, b, configID string, c harnesscapture.Candidate, force bool) (*model.HarnessCaptureReceipt, error) {
	var receipt *model.HarnessCaptureReceipt
	err := s.store.Transaction(ctx, func(tx *store.Store, db *gorm.DB) error {
		// Serialize config mutations and lock credential rows before checking their
		// revisions; a proxy refresh completing first conflicts unless the user forces the save.
		var locked model.HarnessConfig
		if err := db.Clauses(clause.Locking{Strength: "UPDATE"}).Where("project_id = ? AND id = ?", p, configID).First(&locked).Error; err != nil {
			return err
		}
		if previous, err := tx.GetHarnessCaptureReceipt(ctx, p, b, configID, c.ID); err == nil {
			receipt = previous
			return nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		cfg, err := tx.GetHarnessConfig(ctx, p, configID)
		if err != nil {
			return err
		}
		box, err := tx.GetSandbox(ctx, p, b)
		if err != nil {
			return err
		}
		if box.HarnessConfigID == nil || *box.HarnessConfigID != configID {
			return captureConflict()
		}
		bindings, err := tx.ListTargetUseBindings(ctx, p, model.SecretBindingTargetHarnessConfig, configID)
		if err != nil {
			return err
		}
		for _, binding := range bindings {
			var sec model.Secret
			if err := db.Clauses(clause.Locking{Strength: "UPDATE"}).Where("project_id = ? AND id = ?", p, binding.SecretID).First(&sec).Error; err != nil {
				return err
			}
		}
		baseline, err := tx.CaptureBaseline(ctx, p, configID)
		if err != nil {
			return err
		}
		if c.Baseline.ManifestDigest != cfg.ManifestDigest {
			return captureDefinitionConflict()
		}
		if c.Baseline.HarnessConfigID != configID {
			return captureConflict()
		}
		if !force && (c.WriteRevision != cfg.Revision || !maps.Equal(c.SecretRevisions, baseline.SecretRevisions)) {
			return apperrors.StatusError{Status: http.StatusConflict, Message: "harness configuration revision changed; force the save to overwrite conflicting changes, or reconsider the candidate", Cause: harnesscapture.ErrRevisionConflict}
		}
		if cfg.SaveBack == nil {
			return apperrors.NewStatusError(http.StatusBadRequest, "harness does not support configuration capture")
		}
		if err := cfg.SaveBack.ValidateCandidate(c); err != nil {
			return apperrors.NewStatusError(http.StatusBadRequest, "invalid harness configuration candidate")
		}
		files := make([]harnesscapture.File, 0, len(c.Files))
		for _, change := range c.Files {
			if change.File != nil {
				files = append(files, *change.File)
			}
		}
		references, err := cfg.SaveBack.ReferencedSecrets(files)
		if err != nil {
			return apperrors.NewStatusError(http.StatusBadRequest, "invalid credential template")
		}
		available := map[string]bool{}
		for _, binding := range bindings {
			available[binding.EnvName] = true
		}
		for _, credential := range c.Credentials {
			if !credential.Keep {
				available[credential.EnvName] = true
			}
		}
		for _, env := range references {
			if !available[env] {
				return apperrors.NewStatusError(http.StatusBadRequest, "credential template references an unbound secret")
			}
		}
		// The config and secret rows remain locked. Every write below rolls back
		// together if validation or delivery preparation fails.
		for _, change := range c.Files {
			index := slices.IndexFunc(cfg.ConfiguredFiles, func(f model.HarnessConfigFile) bool { return f.Path == change.Path })
			if change.Remove {
				if index >= 0 {
					cfg.ConfiguredFiles = slices.Delete(cfg.ConfiguredFiles, index, index+1)
				}
				continue
			}
			f := change.File
			file := model.HarnessConfigFile{Path: f.Path, Content: f.Content, CreateOnly: f.CreateOnly, Template: f.Template}
			if index >= 0 {
				cfg.ConfiguredFiles[index] = file
			} else {
				cfg.ConfiguredFiles = append(cfg.ConfiguredFiles, file)
			}
		}
		for _, credential := range c.Credentials {
			if err := s.applyCapturedCredential(ctx, tx, db, cfg, bindings, credential); err != nil {
				return err
			}
		}
		if err := tx.UpdateHarnessConfig(ctx, cfg); err != nil {
			return err
		}
		baseline, err = tx.CaptureBaseline(ctx, p, configID)
		if err != nil {
			return err
		}
		// The sandboxes running the config were marked by each binding
		// written; the one that saved is given what they deliver now, so its
		// acknowledgment carries their sentinels.
		if s.sandboxes != nil {
			if err := s.sandboxes.MaterializeSandboxSecretsTx(ctx, tx, p, b); err != nil {
				return err
			}
		}
		receipt = &model.HarnessCaptureReceipt{ProjectID: p, SandboxID: b, HarnessConfigID: configID, CandidateID: c.ID, Baseline: baseline}
		if err := tx.CreateHarnessCaptureReceipt(ctx, receipt); err != nil {
			return err
		}
		if s.dirtier == nil {
			return errors.New("harness capture requires the reconcile engine")
		}
		return s.dirtier.MarkDirtyTx(ctx, db, HarnessConfigResourceType, configID)
	})
	return receipt, err
}

func (s *Service) applyCapturedCredential(ctx context.Context, tx *store.Store, db *gorm.DB, cfg *model.HarnessConfig, bindings []model.SecretBinding, c harnesscapture.Credential) error {
	provider, ok := cfg.SaveBack.Provider(c.Provider, c.EnvName)
	if !ok {
		return captureConflict()
	}
	var existingID string
	for _, binding := range bindings {
		if binding.EnvName == c.EnvName {
			existingID = binding.SecretID
		}
	}
	if c.Keep {
		if existingID == "" {
			return captureConflict()
		}
		return nil
	}
	var value model.SecretValue
	if err := json.Unmarshal(c.Value, &value); err != nil {
		return apperrors.NewStatusError(http.StatusBadRequest, "invalid credential value")
	}
	// Sentinels are deliberately indistinguishable from real tokens by shape.
	// Reject exact placeholders issued anywhere in this project, including a
	// copied sentinel that the originating sandbox does not own.
	var placeholders []model.SandboxSecret
	if err := db.Select("sentinel").Where("project_id = ?", cfg.ProjectID).Find(&placeholders).Error; err != nil {
		return err
	}
	for _, placeholder := range placeholders {
		if placeholder.Sentinel != "" && (value.Token == placeholder.Sentinel || value.RefreshToken == placeholder.Sentinel) {
			return apperrors.NewStatusError(http.StatusBadRequest, "credential is a sentinel placeholder")
		}
	}
	if c.Type == "oauth" {
		if provider.OAuth == nil {
			return captureConflict()
		}
		value.TokenURL = provider.OAuth.TokenURL
		value.ClientID = provider.OAuth.ClientID
		value.TokenRequestEncoding = provider.OAuth.Encoding
	}
	//nolint:gosec // Credential bytes go directly to the sealing store, never to clients or logs.
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	owned := slices.Contains(cfg.ConfiguredSecretIDs, existingID)
	scoped := *s
	scoped.store = tx
	var secret *model.Secret
	if owned {
		secret, err = tx.GetSecret(ctx, cfg.ProjectID, existingID)
		if err != nil {
			return err
		}
	} else {
		newID, err := id.New(id.PrefixSecret)
		if err != nil {
			return err
		}
		name, err := scoped.freeSecretName(ctx, cfg, c.EnvName)
		if err != nil {
			return err
		}
		secret = &model.Secret{ID: newID, ProjectID: cfg.ProjectID, Name: name, MaxBindingTTL: 0}
	}
	secret.Type = c.Type
	secret.EncryptedValue = raw
	// Where it may go: the routes the provider registers. A provider that
	// registers none leaves an existing secret's routes as they are — a
	// person may have narrowed them — and gives a new one its host, else
	// anywhere (ADR 26-10-27-939 §2).
	routes := make([]model.SecretRoute, 0, len(provider.Routes))
	for _, route := range provider.Routes {
		routes = append(routes, model.SecretRoute(route))
	}
	if len(routes) == 0 && !owned {
		routes = configuredRoutes(&model.HarnessConfig{}, "", hostscope.Normalize(provider.Host))
	}
	routesChanged := false
	if len(routes) > 0 {
		if routesChanged, err = secret.ReplaceRoutes(routes); err != nil {
			return apperrors.NewStatusError(http.StatusBadRequest, "invalid credential routes")
		}
	}
	if owned {
		if err := tx.UpdateSecret(ctx, secret); err != nil {
			return err
		}
		if routesChanged && s.sandboxes != nil {
			if err := s.sandboxes.MarkSecretHoldersDirtyTx(ctx, tx, db, cfg.ProjectID, secret.ID); err != nil {
				return err
			}
		}
	} else {
		if err := tx.CreateSecret(ctx, secret); err != nil {
			return err
		}
		cfg.ConfiguredSecretIDs = append(cfg.ConfiguredSecretIDs, secret.ID)
	}
	return s.bindHarnessSecret(ctx, tx, db, cfg, c.EnvName, secret.ID)
}

// A preview crosses the same untrusted boundary as a candidate. Validate file
// contents before returning them and replace free-form exporter diagnostics;
// an exporter must not turn its error text into a credential disclosure.
func (s *Service) validateCapturePreview(ctx context.Context, projectID, sandboxID string, p *harnesscapture.Preview) error {
	box, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return err
	}
	if box.HarnessConfigID == nil {
		return captureConflict()
	}
	cfg, err := s.store.GetHarnessConfig(ctx, projectID, *box.HarnessConfigID)
	if err != nil {
		return err
	}
	invalid := func() error {
		return apperrors.NewStatusError(http.StatusBadGateway, "invalid harness configuration preview")
	}
	if cfg.SaveBack == nil {
		if len(p.Files) > 0 || len(p.Credentials) > 0 {
			return invalid()
		}
	} else {
		files := []harnesscapture.File{}
		for _, change := range p.Files {
			if change.File != nil {
				if change.File.Path != change.Path || change.Remove {
					return invalid()
				}
				files = append(files, *change.File)
			} else if !change.Remove {
				return invalid()
			}
		}
		if err := cfg.SaveBack.ValidateFiles(files); err != nil {
			return invalid()
		}
		for _, credential := range p.Credentials {
			provider, ok := cfg.SaveBack.Provider(credential.Provider, credential.EnvName)
			if !ok || !slices.Contains(provider.Types, credential.Type) {
				return invalid()
			}
		}

	}
	if p.State == "superseded" {
		p.Diagnostics = []harnesscapture.Diagnostic{{Code: "capture-superseded", Message: "Saved credentials or harness delivery changed remotely; refresh the sandbox delivery and log in again."}}
		return nil
	}
	for i := range p.Diagnostics {
		original := p.Diagnostics[i]
		diagnostic := harnesscapture.Diagnostic{Code: "capture-warning", Message: "The harness reported unsupported local configuration; inspect it locally."}
		// Only registered paths and fixed messages cross this boundary. A
		// harness's free-form error or invented path may contain a credential.
		if cfg.SaveBack != nil && (slices.Contains(cfg.SaveBack.Watch, original.Path) || slices.Contains(cfg.SaveBack.CredentialFiles, original.Path)) {
			diagnostic.Path = original.Path
		}
		switch original.Code {
		case "partial":
			diagnostic.Code = "capture-incomplete"
			diagnostic.Message = "Configuration is incomplete, unreadable, or invalid. Check the local file; it has been retained unchanged."
		case "unsupported":
			diagnostic.Code = "capture-unsupported"
			diagnostic.Message = "The harness cannot export this configuration or login format. Check the local file; it has been retained unchanged."
		}
		p.Diagnostics[i] = diagnostic
	}
	return nil
}
