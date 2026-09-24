package model

import (
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/x/id"
)

// Host trust (ADR 0149): a host whose certificate the pool's egress refuses is
// reachable by one sandbox once a person pins one certificate from the chain
// the pool observed. A trust is a sandbox's alone — its rows carry the sandbox
// and go when it does — and it always lapses.

// ID prefixes for the two host trust resources. They are this package's own:
// nothing outside the server mints either.
const (
	idPrefixHostTrustRequest = "treq"
	idPrefixHostTrust        = "trust"
)

// Pin kinds. A pin names one certificate, never a setting that turns
// verification off.
const (
	// TrustPinKindCA pins a CA the host's chain must verify to.
	TrustPinKindCA = "ca"
	// TrustPinKindLeafSPKI pins the host certificate's public key, for a
	// chain that carries no CA.
	TrustPinKindLeafSPKI = "leaf-spki"
)

// HostTrustRequest statuses. They are a secret request's: an approval is a
// person's, whatever it approves.
const (
	HostTrustRequestStatusPending  = SecretRequestStatusPending
	HostTrustRequestStatusApproved = SecretRequestStatusApproved
	HostTrustRequestStatusDenied   = SecretRequestStatusDenied
)

// MaxHostTrustTTLSeconds is the longest a trust may last: the agent
// credentials protocol's own ceiling on an ask, thirty days.
const MaxHostTrustTTLSeconds = 30 * 24 * 60 * 60

// DefaultHostTrustTTLSeconds is a trust's lifetime when neither the agent nor
// the approver names one.
const DefaultHostTrustTTLSeconds = 60 * 60

// ObservedCertificate is one certificate a host presented to the pool's
// egress. Public material only.
type ObservedCertificate struct {
	Subject    string    `json:"subject" doc:"Subject distinguished name"`
	Issuer     string    `json:"issuer" doc:"Issuer distinguished name"`
	DNSNames   []string  `json:"dnsNames,omitempty" doc:"DNS names the certificate is valid for"`
	IPs        []string  `json:"ips,omitempty" doc:"IP addresses the certificate is valid for"`
	NotBefore  time.Time `json:"notBefore" doc:"Start of the validity window" format:"date-time"`
	NotAfter   time.Time `json:"notAfter" doc:"End of the validity window" format:"date-time"`
	SHA256     string    `json:"sha256" doc:"Hex SHA-256 of the certificate's DER"`
	SPKISHA256 string    `json:"spkiSha256" doc:"Hex SHA-256 of its SubjectPublicKeyInfo"`
	IsCA       bool      `json:"isCA,omitempty" doc:"Whether the certificate is a CA"`
	SelfSigned bool      `json:"selfSigned,omitempty" doc:"Whether the certificate is signed by its own key"`
	PEM        string    `json:"pem" doc:"The certificate, PEM-encoded"`
}

// TrustPin names the certificate a host trust verifies the host against.
type TrustPin struct {
	Kind   string `gorm:"column:kind;not null;type:text" json:"kind" doc:"What is pinned" enum:"ca,leaf-spki"`
	SHA256 string `gorm:"column:sha256;not null;type:text" json:"sha256" doc:"Hex SHA-256 of the pinned CA's DER, or of the leaf's SubjectPublicKeyInfo"`
}

// HostTrustRequest is an agent's ask to trust a host for its own sandbox. It
// is an approval-inbox item beside SecretRequest; a person resolves it by
// approving a pin, which mints a HostTrust, or by denying it.
type HostTrustRequest struct {
	ID            string      `gorm:"primaryKey;type:text" json:"id" doc:"Stable request ID"`
	ProjectID     string      `gorm:"column:project_id;not null;type:text;index" json:"projectId" doc:"Project ID"`
	SandboxID     string      `gorm:"column:sandbox_id;not null;type:text;index" json:"sandboxId" doc:"Sandbox the trust is asked for"`
	RequestedBy   string      `gorm:"column:requested_by;not null;type:text" json:"requestedBy" doc:"Principal ID of the requestor"`
	Host          string      `gorm:"column:host;not null;type:text" json:"host" doc:"The endpoint to trust, host:port"`
	Justification string      `gorm:"column:justification;not null;type:text;default:''" json:"justification,omitempty" doc:"Why the agent says it needs to reach the host"`
	Uses          []SecretUse `gorm:"column:uses;type:text;serializer:json" json:"uses,omitempty" doc:"What the agent says it will send the host"`
	// GrantTTL is how long the agent asked the trust to last, in seconds. It is
	// the approval's default, never its term; zero asked for nothing in
	// particular.
	GrantTTL int64 `gorm:"column:grant_ttl_seconds;not null;default:0" json:"grantTTLSeconds,omitempty" doc:"How long the agent asked the trust to last, in seconds"`
	// ObservedChain is what the pool's egress was shown, leaf first. The
	// sandbox's word about the host is never recorded: only the pool's.
	ObservedChain []ObservedCertificate `gorm:"column:observed_chain;type:text;serializer:json" json:"observedChain,omitempty" doc:"The chain the pool's egress was shown, leaf first"`
	// SuppliedCA is a CA the agent offered as the pin, already checked by the
	// pool to be one the observed chain verifies against.
	SuppliedCA *ObservedCertificate `gorm:"column:supplied_ca;type:text;serializer:json" json:"suppliedCA,omitempty" doc:"A CA the agent offered, which the observed chain verifies against"`
	Status     string               `gorm:"column:status;not null;type:text;default:'pending'" json:"status" doc:"Request status" enum:"pending,approved,denied"`
	TrustID    string               `gorm:"column:trust_id;not null;type:text;default:''" json:"trustId,omitempty" doc:"Host trust that satisfied this request; set when approved"`
	CreatedAt  time.Time            `json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt  time.Time            `json:"updatedAt" doc:"Last update timestamp" format:"date-time"`

	Project *Project `gorm:"foreignKey:ProjectID" json:"-"`
}

func (HostTrustRequest) TableName() string { return "host_trust_requests" }

func (r *HostTrustRequest) BeforeCreate(_ *gorm.DB) error {
	if r.ID == "" {
		var err error
		if r.ID, err = id.New(idPrefixHostTrustRequest); err != nil {
			return err
		}
	}
	return nil
}

// HostTrust is one sandbox's pin for one endpoint. Every request the sandbox
// sends the host is judged against Uses.
type HostTrust struct {
	ID        string   `gorm:"primaryKey;type:text" json:"id" doc:"Stable host trust ID"`
	ProjectID string   `gorm:"column:project_id;not null;type:text;index" json:"projectId" doc:"Project ID"`
	SandboxID string   `gorm:"column:sandbox_id;not null;type:text;index" json:"sandboxId" doc:"The one sandbox that trusts the host"`
	Host      string   `gorm:"column:host;not null;type:text" json:"host" doc:"The trusted endpoint, host:port"`
	Pin       TrustPin `gorm:"embedded;embeddedPrefix:pin_" json:"pin" doc:"The pinned certificate"`
	// PinPEM is the pinned CA itself, which the proxy builds its root set
	// from. A leaf-spki pin has none: the key's hash is the whole pin.
	PinPEM    string      `gorm:"column:pin_pem;not null;type:text;default:''" json:"pinPem,omitempty" doc:"The pinned CA, PEM-encoded; absent for a leaf-spki pin"`
	Uses      []SecretUse `gorm:"column:uses;type:text;serializer:json" json:"uses,omitempty" doc:"Approved uses, with the IDs the judge is given"`
	ExpiresAt time.Time   `gorm:"column:expires_at;not null" json:"expiresAt" doc:"When the trust lapses" format:"date-time"`
	GrantedBy string      `gorm:"column:granted_by;not null;type:text" json:"grantedBy" doc:"Principal ID of the approver"`
	RequestID string      `gorm:"column:request_id;not null;type:text;default:''" json:"requestId,omitempty" doc:"Trust request the trust was approved from"`
	CreatedAt time.Time   `json:"createdAt" doc:"Creation timestamp" format:"date-time"`

	Project *Project `gorm:"foreignKey:ProjectID" json:"-"`
}

func (HostTrust) TableName() string { return "host_trusts" }

func (t *HostTrust) BeforeCreate(_ *gorm.DB) error {
	if t.ID == "" {
		var err error
		if t.ID, err = id.New(idPrefixHostTrust); err != nil {
			return err
		}
	}
	return nil
}

// Live reports whether the trust is still in force at now.
func (t *HostTrust) Live(now time.Time) bool { return now.Before(t.ExpiresAt) }
