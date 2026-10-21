package secrets

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// RuleRequestResourceType is the reconciler that tests approval rules against
// a pending request (ADR 26-10-12-330 §4). Its id is a secret request's ID.
const RuleRequestResourceType = "ruleRequest"

// RuleRequests tests a project's approval rules against one pending request.
//
// Filing a request records it as pending and returns at once; the rules are
// tested here, off that path, because a test may wait on the judge. A request
// is marked when it is filed and when a rule is made or edited.
//
// There is no periodic scan. A run whose judge could not answer fails, and the
// engine tries it again with its own backoff for as long as the request is
// pending; a scan would re-mark every such request each minute and cancel that
// backoff, asking a judge that cannot answer once a minute per request. A mark
// that is lost costs what no rule costs: the request waits for a person.
//
// It is idempotent by the record: every question the judge answered has a
// verdict, and the same question is answered from it. A run that changes
// nothing asks nobody.
type RuleRequests struct {
	secrets *Service
}

// RuleRequests is the reconciler over this service's pending requests.
func (s *Service) RuleRequests() *RuleRequests { return &RuleRequests{secrets: s} }

// Reconcile tests the project's rules against one request, one at a time and
// each on its own — never several rules' uses together — in the order of what
// each step costs: the facts, the repository the discobox is of among them,
// then the content. The first rule
// that covers the request approves it and ends the run. A request no rule
// covers stays pending, for a person, exactly as it would with no rules.
//
// A judge that could not answer about some rule, when none covered, is an
// error: the request stays dirty and is tested again, since that rule might
// have covered it.
func (r *RuleRequests) Reconcile(ctx context.Context, requestID string) (reconcile.Result, error) {
	s := r.secrets
	req, err := s.store.GetSecretRequestByID(ctx, requestID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	if !ruleTestable(req) {
		return reconcile.Result{}, nil
	}
	rules, err := s.liveRules(ctx, req.ProjectID)
	if err != nil || len(rules) == 0 {
		return reconcile.Result{}, err
	}
	repository, found, err := s.requesterRepository(ctx, req)
	if err != nil || !found {
		return reconcile.Result{}, err
	}

	var unanswered error
	for i := range rules {
		rule := &rules[i]
		secret, _, reason, err := s.ruleFacts(ctx, rule, req, repository, nil, false)
		if err != nil {
			return reconcile.Result{}, err
		}
		if reason != "" {
			continue
		}
		covered, _, err := s.ruleUsesCover(ctx, rule, req)
		if err != nil {
			if unanswered == nil {
				unanswered = err
			}
			continue
		}
		if !covered {
			continue
		}
		switch err := s.approveUnderRule(ctx, req, rule, secret); {
		case err == nil:
			slog.InfoContext(ctx, "an approval rule approved a credential request",
				"projectId", req.ProjectID, "requestId", req.ID, "grantId", req.GrantID,
				"ruleId", rule.ID, "sandboxId", req.SandboxID)
			return reconcile.Result{}, nil
		case errors.Is(err, errRequestAnswered):
			// A person, or another node, answered it first.
			return reconcile.Result{}, nil
		default:
			// The rule covers the request and the approval was refused all the
			// same — a variable another grant still delivers, a rule edited
			// under the judge. It is said and the next rule is tried; the
			// request waits for a person if none goes through.
			slog.WarnContext(ctx, "an approval rule covers a request it could not approve",
				"projectId", req.ProjectID, "requestId", req.ID, "ruleId", rule.ID, "error", err)
		}
	}
	return reconcile.Result{}, unanswered
}

// errRequestAnswered is an approval that found its request no longer pending.
var errRequestAnswered = errors.New("the request was answered concurrently")

// errPolicyResolvedOtherwise is an approval whose policy's variables resolve,
// for the discobox that asked, to values other than the ones it was shown.
var errPolicyResolvedOtherwise = apperrors.NewStatusError(http.StatusConflict,
	"the policy's variables do not resolve to the values this approval was shown; read the request again and approve what it shows now")

// errRuleChanged is an approval whose rule no longer says what the judge read.
var errRuleChanged = apperrors.NewStatusError(http.StatusConflict,
	"the approval rule was disabled, lapsed, deleted or edited since the judge read it")

// approveUnderRule approves a request a rule covers, through the write a
// person's approval takes (commitApproval). The grant is what a person's
// approval would mint — sandbox-scoped, the uses as asked, so each discobox
// holds its own branch and not the class — and records the rule (§4).
func (s *Service) approveUnderRule(ctx context.Context, req *model.SecretRequest, rule *model.ApprovalRule, secret *model.Secret) error {
	uses, err := mintUseIDs(req.Uses)
	if err != nil {
		return err
	}
	purpose, err := grantPurpose(req.Purpose)
	if err != nil {
		return err
	}
	return s.commitApproval(ctx, &approvalWrite{
		req:      req,
		secret:   secret,
		scope:    model.SecretGrantScopeSandbox,
		scopeKey: req.SandboxID,
		hosts:    req.Hosts,
		uses:     uses,
		purpose:  purpose,
		rule:     rule,
	})
}

func logMarkFailure(ctx context.Context, resourceType, id string, err error) {
	slog.WarnContext(ctx, "an approval rule's test could not be marked for reconciling",
		"resourceType", resourceType, "id", id, "error", err)
}
