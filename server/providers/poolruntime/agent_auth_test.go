package poolruntime

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"
	"time"

	"aidanwoods.dev/go-paseto"

	poolagentserver "github.com/obot-platform/discobox/pool-agent/server"
)

func newPoolAgentTestAuth(t *testing.T, projectID, poolID string) (string, string) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate pool-agent test key: %v", err)
	}
	secretKey, err := paseto.NewV4AsymmetricSecretKeyFromEd25519(privateKey)
	if err != nil {
		t.Fatalf("load pool-agent test secret key: %v", err)
	}
	now := time.Now()
	token := paseto.NewToken()
	token.SetAudience(poolagentserver.PoolAgentAudience)
	token.SetIssuedAt(now)
	token.SetNotBefore(now.Add(-time.Minute))
	token.SetExpiration(now.Add(time.Hour))
	token.SetString("project_id", projectID)
	token.SetString("pool_id", poolID)
	if err := token.Set("scopes", []string{poolagentserver.ScopeSandboxRead, poolagentserver.ScopeSandboxWrite}); err != nil {
		t.Fatalf("set pool-agent test scopes: %v", err)
	}
	return base64.StdEncoding.EncodeToString(publicKey), token.V4Sign(secretKey, nil)
}
