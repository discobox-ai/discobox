package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/discobox-ai/discobox/pkg/sandboxmeta"
	"github.com/discobox-ai/discobox/proxy/egress"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/secrets"
)

type SandboxGetOption func(*sandboxGetOptions)

type sandboxGetOptions struct {
	generation *int64
	// bindingsDeleted is told, in DeleteSandbox's transaction, the bindings
	// the delete took with it.
	bindingsDeleted BindingsDeletedFunc
}

func WithGeneration(generation int64) SandboxGetOption {
	return func(opts *sandboxGetOptions) {
		opts.generation = &generation
	}
}

// OnBindingsDeleted has DeleteSandbox hand fn, in its transaction, the
// bindings it deleted with the sandbox: its own, and those other discoboxes
// were handed on under its delegate bindings, whose sentinels fn marks.
func OnBindingsDeleted(fn BindingsDeletedFunc) SandboxGetOption {
	return func(opts *sandboxGetOptions) {
		opts.bindingsDeleted = fn
	}
}

// SandboxOperationRef identifies one sandbox by its project scope.
type SandboxOperationRef struct {
	ProjectID string
	SandboxID string
}

// ListSandboxRefsNeedingReconcile returns refs of sandboxes whose generations
// disagree. It is the reconcile engine's lost-mark backstop (ADR 0017 §1).
//
// The generation comparison is the source of truth, so this needs no cutoff and
// no notion of an operation being in flight. The query it replaces asked which
// sandboxes had an operation recorded as pending or running, which is why it
// never re-drove a sandbox that had drifted but read `success` — the gap that
// let a whole pool's worth of dead containers report themselves as running.
func (s *Store) ListSandboxRefsNeedingReconcile(ctx context.Context) ([]SandboxOperationRef, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var rows []model.Sandbox
	if err := read.Model(&model.Sandbox{}).
		Select("project_id", "id").
		Where("generation > observed_generation").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	refs := make([]SandboxOperationRef, 0, len(rows))
	for _, row := range rows {
		refs = append(refs, SandboxOperationRef{ProjectID: row.ProjectID, SandboxID: row.ID})
	}
	return refs, nil
}

// ListArchivedSandboxRefsExpiredBefore returns refs of archived sandboxes whose
// retention has run out, judged against their own project's setting and falling
// back to defaultRetention where the project has not chosen one.
//
// It is the level-triggered backstop for retention (ADR 0022 §4). An archived
// sandbox's generations agree, so ListSandboxRefsNeedingReconcile is blind to it
// by design; the future-dated mark that normally wakes it is a mark like any
// other and can be lost. Without this, a lost mark means data kept forever.
//
// The deadline is derived rather than stored, for the same reason the Go-side
// helper derives it: nothing can drift out of agreement with a value that is
// never written down. It is computed here rather than in SQL because date
// arithmetic is not portable between SQLite and Postgres, and the archived set
// is small — a sandbox is only in it between an archive and its purge.
func (s *Store) ListArchivedSandboxRefsExpiredBefore(ctx context.Context, defaultRetention time.Duration, now time.Time) ([]SandboxOperationRef, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ProjectID               string
		ID                      string
		StateChangedAt          time.Time
		ArchiveRetentionSeconds int64
	}
	if err := read.Model(&model.Sandbox{}).
		Select("sandboxes.project_id, sandboxes.id, sandboxes.state_changed_at, projects.archive_retention_seconds").
		Joins("JOIN projects ON projects.id = sandboxes.project_id").
		Where("sandboxes.state = ?", model.SandboxStateArchived).
		Where("sandboxes.desired_state = ?", model.DesiredStateArchived).
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	refs := make([]SandboxOperationRef, 0, len(rows))
	for _, row := range rows {
		if row.StateChangedAt.IsZero() {
			continue
		}
		retention := defaultRetention
		if row.ArchiveRetentionSeconds > 0 {
			retention = time.Duration(row.ArchiveRetentionSeconds) * time.Second
		}
		if !now.Before(row.StateChangedAt.Add(retention)) {
			refs = append(refs, SandboxOperationRef{ProjectID: row.ProjectID, SandboxID: row.ID})
		}
	}
	return refs, nil
}

// ListStoppedSandboxes returns a project's sandboxes on one harness config,
// or on any when harnessConfigID is empty, that a change to their pins may be
// applied to unattended (ADR 0082 §2, ADR 0121, ADR 26-10-16-423 §7).
//
// Every condition here is a state question. Whether a sandbox is *behind* is
// not asked here at all: that rule is services.SandboxUpgradeTarget, the same
// one the read path reports `upgrade.available` from, and it also excludes
// config-mode sandboxes. Restating either in SQL would let this query and the
// reported state answer differently — and the config-mode half would answer
// differently at once, since the rule is `mode != "config"` while a column
// comparison would also drop a legacy row whose mode is empty.
//
// `runtime_state = 'stopped'` is an observation with exactly one writer
// (ADR 0034 §2), which is what makes it safe to act on without asking anyone.
// The empty runtime state means *not observed*, which is a different answer
// from stopped. A `ready` sandbox that nobody has reported on is in the brief
// window before its create's own report lands, and is excluded. A `failed` one
// is admitted (ADR 0121): that is the first create that failed before any
// container was seen, which has nothing running to restart and is exactly the
// sandbox a new image may fix.
//
// An empty harnessConfigID lists the project's stopped sandboxes whatever
// their harness: the platform's re-pin is every one's (ADR 26-10-16-423 §7).
func (s *Store) ListStoppedSandboxes(ctx context.Context, projectID, harnessConfigID string) ([]model.Sandbox, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	query := read.Where("project_id = ?", projectID)
	if harnessConfigID != "" {
		query = query.Where("harness_config_id = ?", harnessConfigID)
	}
	var sandboxes []model.Sandbox
	err = query.
		Where("desired_state = ?", model.DesiredStatePresent).
		// Settled, and nothing in flight against it: an unsettled row is
		// already being acted on. A settled failure is included (ADR 0121) —
		// the image it failed on is the likeliest cause, and the re-pin is new
		// intent that clears the latched error and retries on the new image.
		// Retries are bounded by the caller: it only re-pins a sandbox whose
		// pin differs from a digest that just moved.
		// Its rows choose which to re-pin; the intent is recorded on a fresh
		// read, so the skills' content is not loaded here (sandboxSkillColumns).
		Omit("skills").
		Where("generation = observed_generation").
		Where("(state = ? AND runtime_state = ?) OR (state = ? AND runtime_state IN ?)",
			model.SandboxStateReady, model.SandboxRuntimeStateStopped,
			model.SandboxStateFailed, []string{model.SandboxRuntimeStateStopped, ""}).
		Order("id ASC").
		Find(&sandboxes).Error
	return sandboxes, err
}

// noBundleList is a sandbox whose bundles column holds no list: one pinned
// before ADR 26-10-24-953, in its own columns, or to nothing.
const noBundleList = "(bundles IS NULL OR bundles IN ('', 'null', '[]'))"

// ListUndeliveredSandboxes lists every settled sandbox no container runs that
// is pinned to no platform bundle, in either form: one from before bundles,
// pinned to the runnable harness image it ran (ADR 26-10-16-423 §10). The stopped and failed
// ones ListStoppedSandboxes lists, of every project, and the archived ones,
// which a later unarchive builds a container for. A running one is not listed:
// its container stays until something rebuilds it, which pins it then.
func (s *Store) ListUndeliveredSandboxes(ctx context.Context) ([]model.Sandbox, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var sandboxes []model.Sandbox
	err = read.
		Where("platform_bundle_digest = ?", "").
		Where(noBundleList).
		Where("generation = observed_generation").
		Omit("skills").
		Where("(desired_state = ? AND ((state = ? AND runtime_state = ?) OR (state = ? AND runtime_state IN ?))) OR (desired_state = ? AND state = ?)",
			model.DesiredStatePresent,
			model.SandboxStateReady, model.SandboxRuntimeStateStopped,
			model.SandboxStateFailed, []string{model.SandboxRuntimeStateStopped, ""},
			model.DesiredStateArchived, model.SandboxStateArchived).
		Order("id ASC").
		Find(&sandboxes).Error
	return sandboxes, err
}

// ListAgentlessSandboxes lists every sandbox pinned to a platform and to no
// agent bundle, in any state: one pinned before the agent was a bundle of its
// own (ADR 26-10-24-953 §2), whose pool placed the agent from its platform.
func (s *Store) ListAgentlessSandboxes(ctx context.Context) ([]model.Sandbox, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var sandboxes []model.Sandbox
	err = read.
		Select("id", "project_id", "platform_bundle_digest", "bundles", "agent_bundle_digest").
		Where("agent_bundle_digest = ?", "").
		Where("platform_bundle_digest <> '' OR NOT " + noBundleList).
		Order("id ASC").
		Find(&sandboxes).Error
	return sandboxes, err
}

// ListSandboxes lists a project's sandboxes. A non-empty sourceRoot restricts
// the result to sandboxes whose primary source resolves to that repository root.
// Non-empty originKeys restrict it to sandboxes filed under any one of those
// origin keys (ADR 0111). The two filters are independent: sourceRoot asks what
// a sandbox runs against, originKeys ask where on which client it belongs.
func (s *Store) ListSandboxes(ctx context.Context, projectID, sourceRoot string, originKeys []string) ([]model.Sandbox, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	query := read.
		Preload("Project").
		Preload("Pool").
		Preload("Pool.ProviderInstance").
		Preload("HarnessConfig", withBoundSecrets).
		Where("project_id = ?", projectID)
	if sourceRoot != "" {
		query = query.Where("source_root = ?", sourceRoot)
	}
	if len(originKeys) > 0 {
		query = query.Where("origin_key IN ?", originKeys)
	}
	// A listing leaves the skills' content unloaded and answers with their
	// names (sandboxSkillColumns).
	var sandboxes []model.Sandbox
	err = query.
		Omit("skills").
		Order("created_at ASC").
		Find(&sandboxes).Error
	return sandboxes, err
}

func (s *Store) CreateSandbox(ctx context.Context, sandbox *model.Sandbox) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	// The names listings read in place of the skills (sandboxSkillColumns).
	sandbox.SkillNames = nil
	if len(sandbox.Skills) > 0 {
		sandbox.SkillNames = sandbox.Skills.Names()
	}
	persisted, err := s.sealSandboxForWrite(ctx, sandbox)
	if err != nil {
		return err
	}
	return write.Create(persisted).Error
}

// GetSandboxByID looks a sandbox up by its globally unique ID alone, for
// callers that have not been told which project it belongs to.
//
// A pool agent is the case that needs it: it hosts sandboxes from one project
// but speaks in sandbox IDs, so the project is something the control plane
// derives rather than something the agent asserts. Every such caller must still
// check that the sandbox it got back is one it is allowed to speak for.
func (s *Store) GetSandboxByID(ctx context.Context, sandboxID string) (*model.Sandbox, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	return firstByID[model.Sandbox](read, "id", sandboxID)
}

func (s *Store) GetSandbox(ctx context.Context, projectID, sandboxID string, options ...SandboxGetOption) (*model.Sandbox, error) {
	var opts sandboxGetOptions
	for _, option := range options {
		if option != nil {
			option(&opts)
		}
	}

	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	query := read.
		Preload("Project").
		Preload("Pool").
		Preload("Pool.ProviderInstance").
		Preload("HarnessConfig", withBoundSecrets).
		Where("project_id = ?", projectID)
	if opts.generation != nil {
		sandbox, err := firstByID[model.Sandbox](query, "id", sandboxID)
		if err != nil {
			return nil, err
		}
		if sandbox.Generation != *opts.generation {
			return nil, ErrGenerationConflict
		}
		return sandbox, nil
	}
	return firstByID[model.Sandbox](query, "id", sandboxID)
}

// FindSandboxByIDPrefix resolves a sandbox ID or ID-prefix across all
// projects, unscoped by project ID. It exists for the SSH ingress's bare
// `sbx_...` username form (ADR 0024 §1), which names a sandbox before the
// connecting principal's project membership is known; every other caller
// should use the project-scoped GetSandbox.
func (s *Store) FindSandboxByIDPrefix(ctx context.Context, idOrPrefix string) (*model.Sandbox, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	query := read.
		Preload("Project").
		Preload("Pool").
		Preload("Pool.ProviderInstance").
		Preload("HarnessConfig", withBoundSecrets)
	return firstByID[model.Sandbox](query, "id", idOrPrefix)
}

// observedSandboxColumns are written by ApplySandboxStateReports and by nothing
// else (ADR 0034 §2).
//
// Every other sandbox write omits them. The reason is not tidiness: a caller
// that loads a sandbox, performs a slow operation, and saves it back would
// otherwise replay whatever these columns held before that operation, and the
// pool agent's observations land in exactly that window. That is how a sandbox
// observed `running` 1.5s into its create was pushed back to its pre-create
// value at +5s and left there until the next complete sync 60s later.
//
// Nothing is lost by omitting them. The reconciler has no observation to record
// — it never looked at the container — and the report path writes them from
// what the agent saw, in a transaction of its own.
var observedSandboxColumns = []string{
	"runtime_state",
	"runtime_state_changed_at",
	"state_reported_at",
	"state_report_boot",
	"state_report_seq",
	// Provisioning progress is observed on the same channel by the same agent
	// and written only by its report path (ADR 0039), so it belongs to the same
	// rule: a slow load-modify-save elsewhere must not replay a stale pull
	// percentage over a newer one.
	"provision_progress",
	"provision_progress_at",
	// Resource accounting is observed on its own channel by the same agent and
	// written only by its report path (ADR 0071, resource accounting), so it belongs to the same
	// rule: a slow load-modify-save elsewhere must not replay a stale CPU rate
	// over a newer one.
	"resources",
	"resources_observed_at",
}

// sandboxMetaColumns are the control plane's copy of the sandbox's meta
// (ADR 0136), written by create and by UpdateSandboxMeta and by nothing else.
// UpdateSandbox omits them for the reason it omits the observed columns above:
// a stale load-modify-save must not put back a tag the sandbox has since
// dropped. They are a list of their own rather than members of that one,
// because ApplySandboxStateReports writes every observed column back from the
// row it loaded, and a meta write that committed in between would be undone,
// its observation time with it.
var sandboxMetaColumns = []string{
	"meta_name",
	"description",
	"tags",
	"meta_observed_at",
}

// sandboxPlatformColumns is the sandbox's platform (ADR 0145 §1), written by
// the create or import that inserts the row and by placement, which settles it
// for a row that has none yet (SchedulablePoolForSandbox), and by nothing
// else. UpdateSandbox omits it for the reason it omits the columns above: the
// reconciler loads a sandbox, asks its provider to create it — which is where
// placement writes the platform — and saves the row it loaded, which would put
// the empty platform back over the one just settled.
var sandboxPlatformColumns = []string{"platform"}

// sandboxSkillColumns are the skills a sandbox was created with and their
// names, written by the insert and by nothing else: skills are fixed at create
// (ADR 26-10-09-395 §2). UpdateSandbox omits them so that a row loaded by
// ListSandboxes, which leaves the skills unloaded, cannot put an empty set
// back over them when it is saved.
//
// Only the reads of one sandbox load the skills' content; every scan of many
// leaves "skills" unloaded — a listing, a pool's state sync, the upgrade
// sweep — since up to a megabyte a sandbox is too much to read for rows that
// only need their state.
var sandboxSkillColumns = []string{"skills", "skill_names"}

// sandboxEgressColumns are the sandbox's own egress level, which only
// SetSandboxEgress writes (see egressColumn).
var sandboxEgressColumns = []string{egressColumn}

// sandboxWakeColumns are the sandbox's scheduled start and its guard, which
// two writers share — the power reconcile sets and claims the wake, and every
// outside power instruction clears it — so only the guarded methods below
// write them (ADR 26-10-16-337 §6). A whole-row save of a stale struct would
// put back a wake a stop had canceled.
var sandboxWakeColumns = []string{"wake_at", "wake_epoch"}

// sandboxDiscoboxAPIColumn is whether the sandbox may call the discobox API
// (ADR 26-10-27-880 §9), which create and SetSandboxDiscoboxAPI alone write: a
// person changes it while a reconcile holds a copy of the row, and a
// whole-row save of that copy would put the old answer back.
const sandboxDiscoboxAPIColumn = "discobox_api"

// sandboxAttachedHarnessColumn is the harness configs a sandbox was given
// beside its own, which AttachSandboxHarnessConfig alone writes.
const sandboxAttachedHarnessColumn = "attached_harness_config_ids"

// sandboxOmittedColumns is what UpdateSandbox never writes.
var sandboxOmittedColumns = append(append(append(append(append(append(append([]string(nil), observedSandboxColumns...), sandboxMetaColumns...), sandboxPlatformColumns...), sandboxSkillColumns...), sandboxEgressColumns...), sandboxWakeColumns...), sandboxDiscoboxAPIColumn, sandboxAttachedHarnessColumn)

// CancelSandboxWakeTx is what every power instruction from outside writes: no
// scheduled start, and a new epoch, whether or not a wake was set — the race
// it closes is usually one where the power reconcile has not written the wake
// yet (ADR 26-10-16-337 §6). It runs in the caller's transaction, beside the
// dirty mark that has the power reconcile drop its timer.
func (s *Store) CancelSandboxWakeTx(ctx context.Context, tx *gorm.DB, projectID, sandboxID string) error {
	return tx.WithContext(ctx).Model(&model.Sandbox{}).
		Where("project_id = ? AND id = ?", projectID, sandboxID).
		UpdateColumns(map[string]any{"wake_at": nil, "wake_epoch": gorm.Expr("wake_epoch + 1")}).Error
}

// SetSandboxWakeIfEpoch records the wake a confirmed power-down carried, unless
// an outside power instruction has moved the epoch since the caller read it.
// It reports whether it wrote. A nil at clears the wake under the same guard,
// which is how the reconcile spends one.
func (s *Store) SetSandboxWakeIfEpoch(ctx context.Context, projectID, sandboxID string, epoch int64, at *time.Time) (bool, error) {
	write, err := s.getWrite(ctx)
	if err != nil {
		return false, err
	}
	result := write.WithContext(ctx).Model(&model.Sandbox{}).
		Where("project_id = ? AND id = ? AND wake_epoch = ?", projectID, sandboxID, epoch).
		UpdateColumn("wake_at", at)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// SetSandboxEgress writes a sandbox's own egress level, and nothing else.
func (s *Store) SetSandboxEgress(ctx context.Context, projectID, sandboxID string, level egress.Policy) error {
	return s.setEgress(ctx, &model.Sandbox{}, projectID, sandboxID, level)
}

// AttachSandboxHarnessConfig gives a sandbox a harness config's bindings
// beside its own harness config's, in the caller's transaction, reporting
// whether it was not attached already. Its own harness config is never
// attached: it applies already.
func (s *Store) AttachSandboxHarnessConfig(ctx context.Context, projectID, sandboxID, configID string) (bool, error) {
	attached := false
	err := s.Transaction(ctx, func(_ *Store, tx *gorm.DB) error {
		sandbox, err := firstByID[model.Sandbox](tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("project_id = ?", projectID), "id", sandboxID)
		if err != nil {
			return err
		}
		if (sandbox.HarnessConfigID != nil && *sandbox.HarnessConfigID == configID) || slices.Contains(sandbox.AttachedHarnessConfigIDs, configID) {
			return nil
		}
		attached = true
		return tx.Model(sandbox).Select(sandboxAttachedHarnessColumn).
			Updates(&model.Sandbox{AttachedHarnessConfigIDs: append(slices.Clone(sandbox.AttachedHarnessConfigIDs), configID)}).Error
	})
	return attached, err
}

// SetSandboxDiscoboxAPI writes whether a sandbox may call the discobox API,
// and nothing else.
func (s *Store) SetSandboxDiscoboxAPI(ctx context.Context, projectID, sandboxID string, allowed bool) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	result := write.WithContext(ctx).Model(&model.Sandbox{}).
		Where("project_id = ? AND id = ?", projectID, sandboxID).
		Update(sandboxDiscoboxAPIColumn, allowed)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) UpdateSandbox(ctx context.Context, sandbox *model.Sandbox, options ...SandboxGetOption) error {
	var opts sandboxGetOptions
	for _, option := range options {
		if option != nil {
			option(&opts)
		}
	}

	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	persisted, err := s.sealSandboxForWrite(ctx, sandbox)
	if err != nil {
		return err
	}
	if opts.generation == nil {
		return write.Omit(sandboxOmittedColumns...).Save(persisted).Error
	}

	result := write.Model(&model.Sandbox{}).
		Where("project_id = ? AND id = ? AND generation = ?", sandbox.ProjectID, sandbox.ID, *opts.generation).
		Select("*").
		Omit(sandboxOmittedColumns...).
		Updates(persisted)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrGenerationConflict
	}
	return nil
}

// UpdateSandboxAgentStatus writes only the two agent-status columns, pushed
// periodically by the hosting pool-agent (ADR 0030). It deliberately does not
// go through UpdateSandbox/WithGeneration: this is telemetry, not part of the
// desired/observed generation contract, so gating it on generation would make
// a benign polling write spuriously fail against unrelated concurrent
// desired-state reconciliation, and a whole-row Save from a stale in-memory
// struct would risk clobbering a column written concurrently by that same
// reconciliation. UpdateColumns rather than Updates, so it does not stamp
// updated_at either: this is a heartbeat on every running sandbox every poll
// interval, and nothing about it is a change to the resource.
// lastActiveAt, when non-nil, is the newest client access the report carried
// (an attach, a keystroke, or a client attached at observation); it only ever
// moves last_active_at forward, so a late or re-delivered report cannot walk
// real activity back.
func (s *Store) UpdateSandboxAgentStatus(ctx context.Context, projectID, sandboxID string, status model.RawJSON, observedAt time.Time, lastActiveAt *time.Time) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	columns := map[string]any{
		"agent_status":             status,
		"agent_status_observed_at": observedAt,
	}
	if lastActiveAt != nil {
		columns["last_active_at"] = gorm.Expr(
			"CASE WHEN last_active_at IS NULL OR last_active_at < ? THEN ? ELSE last_active_at END",
			*lastActiveAt, *lastActiveAt)
	}
	result := write.WithContext(ctx).Model(&model.Sandbox{}).
		Where("project_id = ? AND id = ?", projectID, sandboxID).
		UpdateColumns(columns)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateSandboxMeta records the name, description and tags a sandbox reported
// holding at observedAt, on the sandbox's own clock (ADR 0136). It writes only
// when observedAt is newer than what is recorded, because two paths report
// them — the periodic status and the answer to a meta write — and a poll that
// read the file before a write must not undo that write by arriving after it.
// A report that loses that race is not an error: something newer is already
// recorded.
//
// Like UpdateSandboxAgentStatus it is an observation, so it neither goes
// through the generation contract nor stamps updated_at.
func (s *Store) UpdateSandboxMeta(ctx context.Context, projectID, sandboxID string, meta sandboxmeta.Meta, observedAt time.Time) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	tags := meta.Tags
	if tags == nil {
		tags = map[string]string{}
	}
	// Encoded here rather than left to the field's serializer, which a
	// column-map update does not apply.
	encoded, err := json.Marshal(tags)
	if err != nil {
		return err
	}
	var name, description any
	if meta.Name != "" {
		name = meta.Name
	}
	if meta.Description != "" {
		description = meta.Description
	}
	observedAt = observedAt.UTC()
	return write.WithContext(ctx).Model(&model.Sandbox{}).
		Where("project_id = ? AND id = ?", projectID, sandboxID).
		Where("meta_observed_at IS NULL OR meta_observed_at < ?", observedAt).
		UpdateColumns(map[string]any{
			"meta_name":        name,
			"description":      description,
			"tags":             string(encoded),
			"meta_observed_at": observedAt,
		}).Error
}

// UpdateSandboxResources writes only the two resource columns, pushed
// periodically by the hosting pool agent (ADR 0071, resource accounting).
//
// Like UpdateSandboxAgentStatus above it deliberately bypasses
// UpdateSandbox/WithGeneration: this is telemetry, not part of the
// desired/observed generation contract, so gating it on generation would make
// a routine report fail against unrelated concurrent reconciliation, and a
// whole-row Save from a stale struct would risk clobbering a column that
// reconciliation is writing at the same moment.
//
// It deliberately does not touch last_active_at. Resource consumption is not
// evidence that a person touched the sandbox — a build or a runaway test loop
// burns CPU with nobody watching — and letting it move that column would keep
// an abandoned sandbox looking active for as long as something was spinning
// inside it.
func (s *Store) UpdateSandboxResources(ctx context.Context, projectID, sandboxID string, resources model.RawJSON, observedAt time.Time) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	result := write.WithContext(ctx).Model(&model.Sandbox{}).
		Where("project_id = ? AND id = ?", projectID, sandboxID).
		UpdateColumns(map[string]any{
			"resources":             resources,
			"resources_observed_at": observedAt.UTC(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteSandbox(ctx context.Context, projectID, sandboxID string, options ...SandboxGetOption) error {
	var opts sandboxGetOptions
	for _, option := range options {
		if option != nil {
			option(&opts)
		}
	}

	return s.Transaction(ctx, func(txStore *Store, tx *gorm.DB) error {
		// Locked before its bindings are deleted, so a binding being written
		// for it (CreateSecretBinding) either finds it gone or is deleted with
		// it.
		query := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("project_id = ?", projectID)
		if opts.generation != nil {
			query = query.Where("generation = ?", *opts.generation)
		}
		sandbox, err := firstByID[model.Sandbox](query, "id", sandboxID)
		if err != nil {
			if opts.generation != nil && errors.Is(err, ErrNotFound) {
				return ErrGenerationConflict
			}
			return err
		}
		deleted, err := DeleteSecretBindingsForTargetTx(tx, projectID, model.SecretBindingTargetSandbox, sandbox.ID)
		if err != nil {
			return err
		}
		if opts.bindingsDeleted != nil {
			if err := opts.bindingsDeleted(txStore, tx, deleted); err != nil {
				return err
			}
		}
		if err := deleteSandboxSecretsTx(tx, projectID, sandboxID); err != nil {
			return err
		}
		if err := deleteSandboxHostTrustsTx(tx, sandbox.ID); err != nil {
			return err
		}
		if err := tx.Where("sandbox_id = ?", sandbox.ID).Delete(&model.SandboxInventory{}).Error; err != nil {
			return err
		}
		if err := denySandboxSecretRequestsTx(tx, projectID, sandbox.ID); err != nil {
			return err
		}
		return tx.Delete(sandbox).Error
	})
}

// deleteSandboxSecretsTx removes a sandbox's secret assignments and the anonymous
// secrets that were created for them. Referenced (non-anonymous) secrets are left
// untouched.
func deleteSandboxSecretsTx(tx *gorm.DB, projectID, sandboxID string) error {
	var assignments []model.SandboxSecret
	if err := tx.Where("sandbox_id = ?", sandboxID).Find(&assignments).Error; err != nil {
		return err
	}
	if len(assignments) == 0 {
		return nil
	}
	secretIDs := make([]string, 0, len(assignments))
	for _, assignment := range assignments {
		secretIDs = append(secretIDs, assignment.SecretID)
	}
	if err := tx.Where("sandbox_id = ?", sandboxID).Delete(&model.SandboxSecret{}).Error; err != nil {
		return err
	}
	// Null the anonymous secrets' ciphertext before deleting them, as
	// DeleteSecret does, so the value is overwritten rather than only unlinked
	// with its row.
	if err := tx.Model(&model.Secret{}).
		Where("project_id = ? AND anonymous = ? AND id IN ?", projectID, true, secretIDs).
		Update("encrypted_value", nil).Error; err != nil {
		return err
	}
	return tx.Where("project_id = ? AND anonymous = ? AND id IN ?", projectID, true, secretIDs).
		Delete(&model.Secret{}).Error
}

func (s *Store) sealSandboxForWrite(ctx context.Context, sandbox *model.Sandbox) (*model.Sandbox, error) {
	if sandbox.ID == "" {
		if err := sandbox.BeforeCreate(nil); err != nil {
			return nil, err
		}
	}
	persisted := *sandbox
	ciphertext, err := secrets.SealIfUnsealed(ctx, s.sealer, "sandboxes.secret_state", sandboxSecretResourceID(sandbox), sandbox.SecretState)
	if err != nil {
		return nil, fmt.Errorf("encrypt sandbox secret state: %w", err)
	}
	persisted.SecretState = ciphertext
	return &persisted, nil
}

// OpenSandboxSecretState decrypts a sandbox's secret state for consumers that
// need to use it. It returns a copy and does not modify the model.
func (s *Store) OpenSandboxSecretState(ctx context.Context, sandbox *model.Sandbox) ([]byte, error) {
	if sandbox == nil || len(sandbox.SecretState) == 0 {
		return nil, nil
	}
	if s.sealer == nil || !secrets.IsSealed(sandbox.SecretState) {
		return append([]byte(nil), sandbox.SecretState...), nil
	}
	plaintext, err := secrets.Open(ctx, s.sealer, "sandboxes.secret_state", sandboxSecretResourceID(sandbox), sandbox.SecretState)
	if err != nil {
		return nil, fmt.Errorf("decrypt sandbox secret state: %w", err)
	}
	return plaintext, nil
}

func sandboxSecretResourceID(sandbox *model.Sandbox) string {
	return sandbox.ProjectID + "/" + sandbox.ID
}

// GetSandboxByName reads a project's sandbox by its name, which is unique in
// the project.
func (s *Store) GetSandboxByName(ctx context.Context, projectID, name string) (*model.Sandbox, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var rows []model.Sandbox
	if err := read.Where("project_id = ? AND name = ?", projectID, name).Limit(1).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	return &rows[0], nil
}

// SandboxNameTaken reports whether a sandbox already holds name in the project.
// It backs the friendly conflict the API returns before the
// idx_sandbox_project_name unique index would reject the insert with a driver
// error. The index remains the authority — two concurrent creates can both pass
// this check — but the race is narrow and the loser still fails safely.
func (s *Store) SandboxNameTaken(ctx context.Context, projectID, name string) (bool, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return false, err
	}
	var count int64
	if err := read.Model(&model.Sandbox{}).
		Where("project_id = ? AND name = ?", projectID, name).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// ListPresentSandboxIDs lists the sandboxes of a project meant to exist —
// neither archived nor deleted, nor on the way to either — optionally narrowed
// to one pool and one harness config. These are the sandboxes an egress level
// change has to reach (ADR 26-10-01-032 §5); one archived since picks the
// policy up when it is recreated.
func (s *Store) ListPresentSandboxIDs(ctx context.Context, projectID, poolID, harnessConfigID string) ([]string, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	query := read.Model(&model.Sandbox{}).
		Where("project_id = ? AND desired_state = ?", projectID, model.DesiredStatePresent)
	if poolID != "" {
		query = query.Where("pool_id = ?", poolID)
	}
	if harnessConfigID != "" {
		query = query.Where("harness_config_id = ?", harnessConfigID)
	}
	var out []string
	err = query.Order("id ASC").Pluck("id", &out).Error
	return out, err
}
