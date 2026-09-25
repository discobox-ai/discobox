package secrets

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// The other end of ADR 0132: a pool agent reports what an upstream made of a
// credential this control plane handed it, and this decides whether that needs
// a person.
//
// The judgment lives here rather than in the proxy because only this side can
// see the credential. The proxy knows a value was refused; whether that is
// recoverable depends on what kind of credential it is and whether it can be
// renewed, which are both facts it must never be told.

// Outcomes a pool agent may report. They mirror `proxy`'s vocabulary, which is
// deliberately not imported: this is a wire contract with an agent, not a
// shared Go type, and it is validated here like any other input.
const (
	// SecretRejectedOutcome is a 401 the proxy had nothing different to retry
	// with.
	SecretRejectedOutcome = "rejected"
	// SecretRejectedAfterRetryOutcome is a 401 on a second, different
	// credential too.
	SecretRejectedAfterRetryOutcome = "rejected-after-retry"
	// SecretAcceptedOutcome retracts a rejection: the credential worked.
	SecretAcceptedOutcome = "accepted"
)

var secretRejectionOutcomes = []string{
	SecretRejectedOutcome,
	SecretRejectedAfterRetryOutcome,
	SecretAcceptedOutcome,
}

// rejectionRefreshCooldown is how long a forced renewal stands for, before a
// further rejection of the same credential is allowed to spend another.
//
// It bounds two things that would otherwise run away, because a refresh token
// rotates on every use:
//
//   - A credential refused in every sandbox at once. A harness config's
//     credential fails in every box running that harness, and each of those is
//     a report; without this, a project with twenty boxes on a dead
//     subscription would spend twenty refresh tokens a minute against an
//     endpoint that has already said no.
//   - A renewal that renews happily and fixes nothing. A lapsed subscription
//     can hold a perfectly good refresh token: every rejection would rotate the
//     credential, read as recovered, record nothing, and be rejected again a
//     minute later — forever, and silently. A credential refused again inside
//     this window is one the renewal did not save, and that is what gets
//     recorded.
const rejectionRefreshCooldown = 10 * time.Minute

// rejectionJudgeCooldown is how long a standing rejection stands before the
// same credential is judged again. It bounds the work a refused credential
// costs while the rejection is still true, and is deliberately not equal to the
// period the proxy reports on: a window the same length as the interval being
// measured is decided by jitter rather than by the rule it states.
const rejectionJudgeCooldown = 5 * time.Minute

// rejectionStaleAfter is how long a rejection outlives the last report of it.
//
// It is what makes a clearance reliable. The proxy retracts a rejection when
// the credential works again, but only while the process that reported it still
// remembers doing so — restart the pool agent, or delete the discobox that hit
// the failure, and there is nobody left to send the retraction. Without this a
// credential fixed anywhere but in a secret write would leave a band nobody can
// dismiss, which is precisely the outcome this feature exists to avoid.
//
// A credential that is still refused and still being used re-reports every
// minute, so it never goes stale; one that has not been refused for half an
// hour is either fixed or unused, and either way the band is claiming something
// nothing has observed lately. It comes back within a minute of the next
// failure.
const rejectionStaleAfter = 30 * time.Minute

// RecordSandboxSecretRejection takes a pool agent's report about a credential
// it swapped and the upstream refused.
//
// The sentinel is resolved exactly as ResolveSandboxSecret resolves it, and the
// calling pool must own the sandbox it belongs to: a report is about a
// credential, and naming one is the same act of trust as asking for its value.
func (s *Service) RecordSandboxSecretRejection(ctx context.Context, poolID, sandboxID, sentinel, host, outcome, useID string) error {
	outcome = strings.TrimSpace(outcome)
	if !slices.Contains(secretRejectionOutcomes, outcome) {
		return apperrors.NewStatusError(http.StatusBadRequest, "unknown rejection outcome")
	}
	assignment, err := s.store.GetSandboxSecretBySentinel(ctx, sandboxID, sentinel)
	if err != nil {
		return apperrors.NotFound(err, "sandbox secret not found")
	}
	sandbox, err := s.store.GetSandbox(ctx, assignment.ProjectID, assignment.SandboxID)
	if err != nil {
		return apperrors.NotFound(err, "sandbox not found")
	}
	if strings.TrimSpace(sandbox.PoolID) != strings.TrimSpace(poolID) {
		return apperrors.NewStatusError(http.StatusNotFound, "sandbox secret not found")
	}
	secret, err := s.store.GetSecret(ctx, assignment.ProjectID, assignment.SecretID)
	if err != nil {
		return apperrors.NotFound(err, "secret not found")
	}
	host = normalizeHost(host)

	if outcome == SecretAcceptedOutcome {
		return s.store.ClearSecretRejection(ctx, assignment.ProjectID, secret.ID, host)
	}
	// A token a person's client renews is not a dead end: its value is stale
	// from now, and the client is asked for another (ADR 26-09-25-122 §3).
	if secret.Renewable() {
		return s.renewableRejection(ctx, secret, sandboxID)
	}

	standing, err := s.store.GetSecretRejection(ctx, assignment.ProjectID, secret.ID, host)
	if err != nil {
		return err
	}
	if standing != nil && time.Since(standing.LastSeenAt) < rejectionJudgeCooldown {
		// Already judged, recently. Record that it is still happening and do
		// not spend another renewal on an answer we have.
		return s.recordRejection(ctx, assignment.ProjectID, secret.ID, host, standing.Reason, sandboxID, useID)
	}

	verdict := s.judgeRejection(ctx, secret)
	switch verdict {
	case verdictRecovered:
		// The credential renewed. Whatever was recorded about it is about a
		// value that no longer exists, and the next request carries the new one
		// — the proxy dropped its cached copy on the way here.
		return s.store.ClearSecretRejections(ctx, assignment.ProjectID, secret.ID)
	case verdictUnknown:
		// Nothing was *learned*: the renewal could not be attempted or could
		// not be judged. Inventing a reason here would name a fault that may be
		// ours, and — through the standing-row cooldown — suppress the attempt
		// that would have settled it.
		//
		// But something was still observed. A credential already standing as
		// refused is being refused again, right now, and the row has to say so
		// or it ages out (rejectionStaleAfter) while the failure is still
		// happening every minute — which is the one state this feature exists
		// to prevent. So the sighting is recorded under the reason already
		// standing, and only a report with nothing standing behind it records
		// nothing at all.
		if standing != nil {
			return s.recordRejection(ctx, assignment.ProjectID, secret.ID, host, standing.Reason, sandboxID, useID)
		}
		return nil
	}
	return s.recordRejection(ctx, assignment.ProjectID, secret.ID, host, string(verdict), sandboxID, useID)
}

// verdict is what a rejection came to. The two that are not a reason are named
// rather than smuggled through an error or an empty string: "it renewed" and
// "we do not know" are both outcomes the caller has to act on differently, and
// both were silently collapsed into a reason before.
type verdict string

const (
	verdictRecovered verdict = "recovered"
	verdictUnknown   verdict = "unknown"
)

// judgeRejection decides what a refused credential means, renewing it first
// where renewing is possible.
func (s *Service) judgeRejection(ctx context.Context, secret *model.Secret) verdict {
	if secret.Type != model.SecretTypeOAuth {
		// An API key is what was stored, and what was stored was refused.
		return verdict(model.SecretRejectionReasonUnrefreshable)
	}
	if s.renewedRecently(secret.ID) {
		// This credential was renewed a moment ago and is being refused again.
		// The renewal is not the answer, whatever the token endpoint says.
		return verdict(model.SecretRejectionReasonRejectedAfterRefresh)
	}
	switch s.forceRefreshOAuth(ctx, secret) {
	case renewalRotated:
		s.noteRenewal(secret.ID)
		return verdictRecovered
	case renewalRefused:
		// The endpoint refused to renew: the refresh token is spent, revoked,
		// or belongs to a session somebody ended elsewhere. A person has to
		// sign in again, and this is the reason that says so.
		return verdict(model.SecretRejectionReasonRefreshFailed)
	case renewalUnchanged:
		// It answered, and handed back the token that was just refused.
		return verdict(model.SecretRejectionReasonRejectedAfterRefresh)
	case renewalImpossible:
		// Nothing to renew with — the same dead end an API key is in.
		return verdict(model.SecretRejectionReasonUnrefreshable)
	}
	return verdictUnknown
}

// renewedRecently reports whether a rejection already spent a renewal on this
// credential inside the cooldown.
//
// It is in memory rather than on the row, because the case it exists for is the
// one where no row was written: a renewal that succeeded, cleared everything,
// and fixed nothing. A restart forgets it and costs one further refresh, which
// is the right way for this to fail.
func (s *Service) renewedRecently(secretID string) bool {
	s.renewalsMu.Lock()
	defer s.renewalsMu.Unlock()
	at, ok := s.renewals[secretID]
	return ok && time.Since(at) < rejectionRefreshCooldown
}

func (s *Service) noteRenewal(secretID string) {
	s.renewalsMu.Lock()
	defer s.renewalsMu.Unlock()
	if s.renewals == nil {
		s.renewals = map[string]time.Time{}
	}
	s.renewals[secretID] = time.Now()
	// Nothing sweeps this: a credential that was renewed once and never
	// rejected again leaves one timestamp behind, and the set is bounded by the
	// project's secrets.
}

func (s *Service) recordRejection(ctx context.Context, projectID, secretID, host, reason, sandboxID, useID string) error {
	return s.store.RecordSecretRejection(ctx, &model.SecretRejection{
		ProjectID: projectID,
		SecretID:  secretID,
		Host:      host,
		Reason:    reason,
		SandboxID: strings.TrimSpace(sandboxID),
		UseID:     strings.TrimSpace(useID),
	})
}

// dropStaleRejections removes and filters out the rejections nothing has
// observed lately (see rejectionStaleAfter).
//
// On the read rather than on a sweep of its own, because this is the only thing
// that asks and the answer is only wrong while somebody is looking at it. The
// package already mints a binding on a read path for the same kind of reason: a
// reconciler chasing rows nobody is reading would be more machinery for a
// smaller guarantee.
func (s *Service) dropStaleRejections(ctx context.Context, rejections []model.SecretRejection) ([]model.SecretRejection, error) {
	live := rejections[:0]
	for _, rejection := range rejections {
		if time.Since(rejection.LastSeenAt) < rejectionStaleAfter {
			live = append(live, rejection)
			continue
		}
		if err := s.store.ClearSecretRejection(ctx, rejection.ProjectID, rejection.SecretID, rejection.Host); err != nil {
			return nil, err
		}
	}
	return live, nil
}

// ListSecretRejections returns the credentials this project is known to be
// unable to use, with what a caller needs to act on each.
//
// The harness is derived here rather than stored: a credential belongs to a
// harness config when that config's configure flow created it, and the same
// answer would otherwise have to be written — and cleared — by every path that
// touches either side.
func (s *Service) ListSecretRejections(ctx context.Context, projectID string) ([]model.SecretRejection, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	rejections, err := s.store.ListSecretRejections(ctx, projectID)
	if err != nil {
		return nil, err
	}
	rejections, err = s.dropStaleRejections(ctx, rejections)
	if err != nil {
		return nil, err
	}
	if len(rejections) == 0 {
		return rejections, nil
	}
	configs, err := s.store.ListHarnessConfigs(ctx, projectID)
	if err != nil {
		return nil, err
	}
	// Which harness config owns a secret, and what it calls it. A secret the
	// configure flow created is named by that config; the variable it is
	// delivered in comes from the binding, which is what a person recognizes it
	// by on the harness's own card.
	type owner struct {
		id, name, envName string
	}
	owners := map[string]owner{}
	for _, cfg := range configs {
		for _, secretID := range cfg.ConfiguredSecretIDs {
			owners[secretID] = owner{id: cfg.ID, name: cfg.Name}
		}
		bindings, err := s.store.ListHarnessConfigSecretBindings(ctx, projectID, cfg.ID)
		if err != nil {
			return nil, err
		}
		for _, binding := range bindings {
			found, ok := owners[binding.SecretID]
			if !ok {
				continue
			}
			if found.id == cfg.ID && found.envName == "" {
				found.envName = binding.EnvName
				owners[binding.SecretID] = found
			}
		}
	}
	for i := range rejections {
		rejection := &rejections[i]
		secret, err := s.store.GetSecret(ctx, projectID, rejection.SecretID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return nil, err
		}
		rejection.SecretName = secret.Name
		rejection.SecretType = secret.Type
		if found, ok := owners[rejection.SecretID]; ok {
			rejection.HarnessConfigID = found.id
			rejection.HarnessConfigName = found.name
			rejection.EnvName = found.envName
		}
	}
	return rejections, nil
}
