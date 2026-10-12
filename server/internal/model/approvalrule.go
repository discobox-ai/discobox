package model

import (
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/x/id"
)

// Approval rules (ADR 26-10-12-330): a project's standing answer to a
// credential request. A rule says a request like this, from a discobox like
// that, is approved without asking. It approves or says nothing: a request no
// rule covers waits for a person, and no rule denies anything.

// idPrefixApprovalRule is this package's own: nothing outside the server
// mints one.
const idPrefixApprovalRule = "rule"

// ApprovalRule is one rule. Only a person makes one; a discobox never makes,
// edits, or reads one.
//
// Its credential, hosts, purpose and lifetime cap are facts the server checks
// a request against. Its scope and uses are sentences the judge reads: Scope
// against what the control plane recorded about the asking discobox, Uses
// against the uses the request asked for.
type ApprovalRule struct {
	ID        string `gorm:"primaryKey;type:text" json:"id" doc:"Stable approval rule ID"`
	ProjectID string `gorm:"column:project_id;not null;type:text;index" json:"projectId" doc:"Project ID"`
	// Scope says which discoboxes the rule is tested for, as a sentence the
	// judge reads against facts recorded when a discobox was created. With no
	// sentence and no pin, the rule is for every discobox in the project,
	// including ones that do not exist yet.
	Scope string `gorm:"column:scope;not null;type:text;default:''" json:"scope,omitempty" doc:"Which discoboxes the rule is for, as a sentence the judge reads against what was recorded about a discobox when it was created; empty, with no pinned discobox, is every discobox in the project"`
	// SandboxID pins the rule to one discobox. A pin is compared, not read.
	SandboxID string `gorm:"column:sandbox_id;not null;type:text;default:'';index" json:"sandboxId,omitempty" doc:"The one discobox the rule is pinned to, when it is pinned"`
	// WellKnownID and SecretID name the rule's one credential: a well-known
	// ID, answered by the project secret marked for it, or a project secret.
	WellKnownID string `gorm:"column:well_known_id;not null;type:text;default:''" json:"wellKnownId,omitempty" doc:"The well-known credential the rule approves, when it names one by ID"`
	SecretID    string `gorm:"column:secret_id;not null;type:text;default:'';index" json:"secretId,omitempty" doc:"The project secret the rule approves, when it names one"`
	// Hosts are the hosts a grant the rule mints may name: a request is
	// covered only when every host it asks for is within them.
	Hosts []string `gorm:"column:hosts;type:text;serializer:json" json:"hosts" doc:"Hosts a grant minted under the rule may name; a request's hosts must all be within them"`
	// Purpose is the one thing the rule approves a credential for, as a grant
	// has one: using it, or delegating it.
	Purpose string `gorm:"column:purpose;not null;type:text;default:'use'" json:"purpose" doc:"What the rule approves a credential for: to use it, or to delegate it to other discoboxes" enum:"use,delegate"`
	// Uses are sentences saying what may be approved. They usually name a
	// class — any issue in a repository, a branch discobox/<slug> — and the
	// judge decides whether every use a request asks for is within them.
	Uses []string `gorm:"column:uses;type:text;serializer:json" json:"uses" doc:"What may be approved, as sentences the judge reads the uses a request asks for against"`
	// MaxGrantTTL caps the lifetime of every grant the rule mints, in seconds.
	// Zero is no cap of the rule's own; the secret's limit holds either way.
	MaxGrantTTL int64 `gorm:"column:max_grant_ttl_seconds;not null;default:0" json:"maxGrantTTLSeconds" doc:"Longest a grant minted under the rule may live, in seconds; 0 leaves it to the secret's own limit"`
	// ExpiresAt is when the rule stops approving. Grants it minted before
	// then are independent of it.
	ExpiresAt *time.Time `gorm:"column:expires_at" json:"expiresAt,omitempty" doc:"When the rule stops approving; empty never" format:"date-time"`
	CreatedBy string     `gorm:"column:created_by;not null;type:text;default:''" json:"createdBy,omitempty" doc:"Principal ID of the person who made the rule"`
	// Enabled carries no column default: a disabled rule is the zero value,
	// and a default would turn it back on at insert.
	Enabled   bool      `gorm:"column:enabled;not null" json:"enabled" doc:"Whether the rule approves anything; a disabled rule mints nothing and revokes nothing"`
	CreatedAt time.Time `json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt time.Time `json:"updatedAt" doc:"Last update timestamp" format:"date-time"`

	Project *Project `gorm:"foreignKey:ProjectID" json:"-"`
}

func (ApprovalRule) TableName() string { return "approval_rules" }

func (r *ApprovalRule) BeforeCreate(_ *gorm.DB) error {
	if r.ID == "" {
		var err error
		if r.ID, err = id.New(idPrefixApprovalRule); err != nil {
			return err
		}
	}
	return nil
}

// Live reports whether the rule approves anything at now: enabled, and not
// past its own expiry.
func (r *ApprovalRule) Live(now time.Time) bool {
	return r.Enabled && (r.ExpiresAt == nil || now.Before(*r.ExpiresAt))
}

// Credential is what the rule's credential is called where a person or the
// judge reads it: its well-known ID, else the secret's ID.
func (r *ApprovalRule) Credential() string {
	if r.WellKnownID != "" {
		return r.WellKnownID
	}
	return r.SecretID
}

// The stages a rule is tested in, in the order of what each costs
// (ADR 26-10-12-330 §4).
const (
	RuleTestStageFacts   = "facts"
	RuleTestStageScope   = "scope"
	RuleTestStageContent = "content"
)

// RuleTest is what came of testing one rule against one request, for the card
// a person reads: whether the rule covered it and, when it did not, which
// stage said so and why. It is read from the rule and the verdicts on record,
// never stored.
type RuleTest struct {
	RuleID  string `json:"ruleId" doc:"The rule tested"`
	Covered bool   `json:"covered" doc:"Whether the rule covers the request"`
	// Pending is a rule whose judge has not answered yet: neither covered nor
	// found not to.
	Pending bool   `json:"pending,omitempty" doc:"True while a judge has still to be asked about the rule"`
	Stage   string `json:"stage,omitempty" doc:"The stage that decided: facts the server checked, the rule's scope, or its content" enum:"facts,scope,content"`
	Reason  string `json:"reason,omitempty" doc:"Why the rule does not cover the request, or that it does"`
}
