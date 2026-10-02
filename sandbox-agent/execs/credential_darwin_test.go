package execs

import (
	"testing"

	"github.com/discobox-ai/discobox/sandboxuser"
)

// A macOS exec inherits the agent's own account, so even a resolved user
// yields no credential to switch to (ADR 0145 §5).
func TestUserCredentialGivesNoneOnTheOneAccount(t *testing.T) {
	credential, ok, err := userCredential(&User{Name: "dev", HomeDirectory: "/Users/dev"})
	if err != nil || ok || credential != nil {
		t.Fatalf("userCredential = %v, %v, %v; want no credential", credential, ok, err)
	}
	attr, err := agentSysProcAttr(&User{Name: "dev", UID: sandboxuser.ID(501)})
	if err != nil || attr == nil || attr.Credential != nil || !attr.Setsid {
		t.Fatalf("agentSysProcAttr = %+v, %v; want a new session and no credential", attr, err)
	}
}
