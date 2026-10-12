package handlers

import (
	"context"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
)

// Approval rules (ADR 26-10-12-330). The routes are a person's: the sandbox
// role lists none of them, and the service refuses a discobox that reached it
// another way.

func (h *Handler) ListApprovalRules(ctx context.Context, params serverapi.ListApprovalRulesParams) (serverapi.ListApprovalRulesRes, error) {
	rules, err := h.services.Secrets.ListApprovalRules(ctx, params.ProjectId)
	if err != nil {
		return apiError(err), nil
	}
	// The array is required, and a project with no rules has an empty one.
	if rules == nil {
		rules = []model.ApprovalRule{}
	}
	body, err := services.Convert[apimodel.ListApprovalRulesBody](struct {
		ApprovalRules any `json:"approvalRules"`
	}{ApprovalRules: rules})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) GetApprovalRule(ctx context.Context, params serverapi.GetApprovalRuleParams) (serverapi.GetApprovalRuleRes, error) {
	rule, err := h.services.Secrets.GetApprovalRule(ctx, params.ProjectId, params.RuleId)
	if err != nil {
		return apiError(err), nil
	}
	return approvalRuleBody(rule)
}

func (h *Handler) CreateApprovalRule(ctx context.Context, req *apimodel.CreateApprovalRuleBody, params serverapi.CreateApprovalRuleParams) (serverapi.CreateApprovalRuleRes, error) {
	rule, err := h.services.Secrets.CreateApprovalRule(ctx, params.ProjectId, *req)
	if err != nil {
		return apiError(err), nil
	}
	return approvalRuleBody(rule)
}

func (h *Handler) UpdateApprovalRule(ctx context.Context, req *apimodel.UpdateApprovalRuleBody, params serverapi.UpdateApprovalRuleParams) (serverapi.UpdateApprovalRuleRes, error) {
	rule, err := h.services.Secrets.UpdateApprovalRule(ctx, params.ProjectId, params.RuleId, *req)
	if err != nil {
		return apiError(err), nil
	}
	return approvalRuleBody(rule)
}

func (h *Handler) DeleteApprovalRule(ctx context.Context, params serverapi.DeleteApprovalRuleParams) (serverapi.DeleteApprovalRuleRes, error) {
	if err := h.services.Secrets.DeleteApprovalRule(ctx, params.ProjectId, params.RuleId); err != nil {
		return apiError(err), nil
	}
	return &serverapi.DeleteApprovalRuleNoContent{}, nil
}

func (h *Handler) ProposeApprovalRules(ctx context.Context, params serverapi.ProposeApprovalRulesParams) (serverapi.ProposeApprovalRulesRes, error) {
	proposals, err := h.services.Secrets.ProposeApprovalRules(ctx, params.ProjectId, params.RequestId)
	if err != nil {
		return apiError(err), nil
	}
	return proposals, nil
}

// approvalRuleBody is a rule as the API says it. The hosts and uses are
// required arrays, and a rule always has some of each.
func approvalRuleBody(rule *model.ApprovalRule) (*apimodel.ApprovalRule, error) {
	body, err := services.Convert[apimodel.ApprovalRule](rule)
	if err != nil {
		return nil, err
	}
	return &body, nil
}
