package workerpool

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"
	"time"

	"aidanwoods.dev/go-paseto"

	workeragentserver "github.com/obot-platform/discobox/worker-agent/server"
)

func newWorkerAgentTestAuth(t *testing.T, projectID, workerID string) (string, string) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate worker-agent test key: %v", err)
	}
	secretKey, err := paseto.NewV4AsymmetricSecretKeyFromEd25519(privateKey)
	if err != nil {
		t.Fatalf("load worker-agent test secret key: %v", err)
	}
	now := time.Now()
	token := paseto.NewToken()
	token.SetAudience(workeragentserver.WorkerAgentAudience)
	token.SetIssuedAt(now)
	token.SetNotBefore(now.Add(-time.Minute))
	token.SetExpiration(now.Add(time.Hour))
	token.SetString("project_id", projectID)
	token.SetString("worker_id", workerID)
	if err := token.Set("scopes", []string{workeragentserver.ScopeSandboxRead, workeragentserver.ScopeSandboxWrite}); err != nil {
		t.Fatalf("set worker-agent test scopes: %v", err)
	}
	return base64.StdEncoding.EncodeToString(publicKey), token.V4Sign(secretKey, nil)
}
