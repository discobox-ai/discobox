package model

import (
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/policy"
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
// Its repositories, credential, hosts, purpose and lifetime cap are facts the
// server checks a request against. Its policy is what a request's policy must
// fall within (ADR 26-10-22-689),
// compared by code, with the judge asked only about conditions code cannot
// compare.
type ApprovalRule struct {
	ID        string `gorm:"primaryKey;type:text" json:"id" doc:"Stable approval rule ID"`
	ProjectID string `gorm:"column:project_id;not null;type:text;index" json:"projectId" doc:"Project ID"`
	// Repositories say which discoboxes the rule is for: those whose primary
	// source is one of them, each stored as host/path and compared exactly,
	// by the server. With none — which a rule has only because a person said
	// every discobox, never by saying nothing — it is for every discobox in the project,
	// including ones that do not exist yet.
	Repositories []string `gorm:"column:repositories;type:text;serializer:json" json:"repositories,omitempty" doc:"The repositories the rule is for, each as host/path: it answers a discobox whose primary source is one of them. Empty is every discobox in the project"`
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
	// Policy says what may be approved. Its statements usually name a class
	// — any issue in $primary.repo, a branch "discobox/*" — and a request is
	// covered when every allow it asks for falls within one of them and it
	// carries every deny. Its variables bind to the discobox that asked, at
	// each test, so it records no resolved values of its own.
	Policy *policy.Policy `gorm:"column:policy;type:text;serializer:json" json:"policy" doc:"What may be approved: a request is covered when its policy falls within this one"`
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
	Stage   string `json:"stage,omitempty" doc:"The stage that decided: facts the server checked, the rule's repositories among them, or its content, which the judge read" enum:"facts,content"`
	Reason  string `json:"reason,omitempty" doc:"Why the rule does not cover the request, or that it does"`
}
