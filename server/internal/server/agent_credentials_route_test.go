package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pool-agent/poolauth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"gorm.io/gorm"
)

// The agent credential broker routes, exercised through the real router: ogen
// routing, the pool authenticator, and the hand-written credential:broker
// scope check.
//
// The service tests call the service directly, so none of that layer runs
// there — and the scope check is the part most likely to be silently wrong,
// because a missing check fails open rather than loudly.

const (
	routeTestPoolID    = "pool-cred"
	routeTestSandboxID = "sbx-cred"
)

func TestAgentCredentialRoutesRequireTheBrokerScope(t *testing.T) {
	skipWithoutDocker(t)
	ctx := context.Background()
	db := newAppTestDB(ctx, t)
	router, _, _, _, err := NewApp(ctx, db.Write, db.Read)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	projectID, privateKey := seedCredentialRoutePool(ctx, t, db.Write, router)

	broker := signPoolAssertion(t, projectID, routeTestPoolID, privateKey, poolauth.ScopeCredentialBroker)
	// A perfectly valid pool assertion for the neighboring scope. Resolving a
	// sentinel and speaking on a sandbox's behalf are different authorities.
	resolveOnly := signPoolAssertion(t, projectID, routeTestPoolID, privateKey, poolauth.ScopeSecretResolve)
	unscoped := signPoolAssertion(t, projectID, routeTestPoolID, privateKey)

	listPath := "/api/pools/" + routeTestPoolID + "/sandbox-credentials?sandboxId=" + routeTestSandboxID
	requestBody := `{"sandboxId":"` + routeTestSandboxID + `","name":"github","envVar":"GITHUB_TOKEN",` +
		`"host":"api.github.com","uses":[{"description":"Open a PR"}]}`
	requestPath := "/api/pools/" + routeTestPoolID + "/sandbox-credential-requests"

	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   string
		token  string
		want   int
	}{
		{"list with the broker scope", http.MethodGet, listPath, "", broker, http.StatusOK},
		{"list with only the resolve scope", http.MethodGet, listPath, "", resolveOnly, http.StatusForbidden},
		{"list with no scopes", http.MethodGet, listPath, "", unscoped, http.StatusForbidden},
		{"list with no token at all", http.MethodGet, listPath, "", "", http.StatusUnauthorized},
		{"request with the broker scope", http.MethodPost, requestPath, requestBody, broker, http.StatusOK},
		{"request with only the resolve scope", http.MethodPost, requestPath, requestBody, resolveOnly, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := callRoute(t, router, tc.method, tc.path, tc.body, tc.token)
			if resp.Code != tc.want {
				t.Fatalf("status = %d, want %d; body = %s", resp.Code, tc.want, resp.Body.String())
			}
		})
	}
}

// A pool may speak only for the sandboxes it hosts. The body names the
// sandbox, so this is the check that stops one pool reading another's.
func TestAgentCredentialRoutesRefuseASandboxOnAnotherPool(t *testing.T) {
	skipWithoutDocker(t)
	ctx := context.Background()
	db := newAppTestDB(ctx, t)
	router, _, _, _, err := NewApp(ctx, db.Write, db.Read)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	projectID, _ := seedCredentialRoutePool(ctx, t, db.Write, router)

	// A second pool, with its own sandbox, in the same project.
	otherKey := seedPool(ctx, t, db.Write, projectID, "pool-other")
	if err := db.Write.WithContext(ctx).Create(&model.Sandbox{
		ID: "sbx-other", ProjectID: projectID, Name: "other", PoolID: "pool-other",
	}).Error; err != nil {
		t.Fatalf("create other sandbox: %v", err)
	}
	otherToken := signPoolAssertion(t, projectID, "pool-other", otherKey, poolauth.ScopeCredentialBroker)

	// pool-other asks its own route about a sandbox that lives on pool-cred.
	path := "/api/pools/pool-other/sandbox-credentials?sandboxId=" + routeTestSandboxID
	resp := callRoute(t, router, http.MethodGet, path, "", otherToken)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for another pool's sandbox; body = %s", resp.Code, resp.Body.String())
	}
}

// The request/poll pair over HTTP, which is the shape the pool agent actually
// drives: an ask returns an id, and polling it reports a status the protocol
// understands.
func TestAgentCredentialRequestAndPollOverHTTP(t *testing.T) {
	skipWithoutDocker(t)
	ctx := context.Background()
	db := newAppTestDB(ctx, t)
	router, _, _, _, err := NewApp(ctx, db.Write, db.Read)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	projectID, privateKey := seedCredentialRoutePool(ctx, t, db.Write, router)
	token := signPoolAssertion(t, projectID, routeTestPoolID, privateKey, poolauth.ScopeCredentialBroker)

	body := `{"sandboxId":"` + routeTestSandboxID + `","name":"github","envVar":"GITHUB_TOKEN",` +
		`"host":"api.github.com","justification":"the task asks me to open a PR",` +
		`"uses":[{"description":"Open a PR against the current repo"}]}`
	resp := callRoute(t, router, http.MethodPost, "/api/pools/"+routeTestPoolID+"/sandbox-credential-requests", body, token)
	if resp.Code != http.StatusOK {
		t.Fatalf("request status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var created struct {
		RequestID string `json:"requestId"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if created.Status != "pending" || !strings.HasPrefix(created.RequestID, "sreq_") {
		t.Fatalf("created = %#v, want a pending request id", created)
	}

	poll := callRoute(t, router,
		http.MethodGet,
		"/api/pools/"+routeTestPoolID+"/sandbox-credential-requests/"+created.RequestID+"?sandboxId="+routeTestSandboxID,
		"", token)
	if poll.Code != http.StatusOK {
		t.Fatalf("poll status = %d, body = %s", poll.Code, poll.Body.String())
	}
	var polled struct {
		RequestID string `json:"requestId"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(poll.Body.Bytes(), &polled); err != nil {
		t.Fatalf("decode poll: %v", err)
	}
	if polled.RequestID != created.RequestID || polled.Status != "pending" {
		t.Fatalf("polled = %#v, want the same request still pending", polled)
	}

	// A hostless ask must be refused at the edge, since approving it could only
	// produce a wildcard grant.
	hostless := `{"sandboxId":"` + routeTestSandboxID + `","name":"github","envVar":"GITHUB_TOKEN",` +
		`"uses":[{"description":"Open a PR"}]}`
	bad := callRoute(t, router, http.MethodPost, "/api/pools/"+routeTestPoolID+"/sandbox-credential-requests", hostless, token)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("hostless request status = %d, want 400; body = %s", bad.Code, bad.Body.String())
	}
}

func callRoute(t *testing.T, router http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp
}

func signPoolAssertion(t *testing.T, projectID, poolID string, privateKey ed25519.PrivateKey, scopes ...string) string {
	t.Helper()
	token, err := poolauth.CreateToken(privateKey, poolauth.Claims{
		ProjectID: projectID,
		PoolID:    poolID,
		Scopes:    scopes,
	})
	if err != nil {
		t.Fatalf("sign pool assertion: %v", err)
	}
	return token
}

// seedCredentialRoutePool creates the pool these tests speak for, registers an
// agent keypair for it, and gives it one sandbox.
func seedCredentialRoutePool(ctx context.Context, t *testing.T, write *gorm.DB, router http.Handler) (string, ed25519.PrivateKey) {
	t.Helper()
	projectID := defaultProjectID(ctx, t, router)
	if err := write.WithContext(ctx).Create(&model.SandboxProviderInstance{
		ID: "provider-cred", ProjectID: projectID, Type: "docker", Name: "docker",
	}).Error; err != nil {
		t.Fatalf("create provider: %v", err)
	}
	key := seedPool(ctx, t, write, projectID, routeTestPoolID)
	if err := write.WithContext(ctx).Create(&model.Sandbox{
		ID: routeTestSandboxID, ProjectID: projectID, Name: "cred", PoolID: routeTestPoolID,
	}).Error; err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	return projectID, key
}

// seedPool creates a pool and stores an agent public key for it, which is what
// the pool authenticator verifies assertions against.
func seedPool(ctx context.Context, t *testing.T, write *gorm.DB, projectID, poolID string) ed25519.PrivateKey {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	encoded, err := poolauth.EncodePublicKey(publicKey)
	if err != nil {
		t.Fatalf("encode key: %v", err)
	}
	registered := time.Now().UTC()
	pool := &model.Pool{
		ID:           poolID,
		ProjectID:    projectID,
		PoolManifest: model.PoolManifest{Name: poolID, ProviderInstanceID: "provider-cred"},
		PublicKey:    encoded,
		KeyType:      poolauth.KeyType,
		RegisteredAt: &registered,
	}
	if err := write.WithContext(ctx).Create(pool).Error; err != nil {
		t.Fatalf("create pool %s: %v", poolID, err)
	}
	return privateKey
}

func defaultProjectID(ctx context.Context, t *testing.T, router http.Handler) string {
	t.Helper()
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, httptest.NewRequestWithContext(ctx, http.MethodGet, "/projects", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("list projects status = %d", resp.Code)
	}
	var body struct {
		Projects []model.Project `json:"projects"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode projects: %v", err)
	}
	if len(body.Projects) == 0 {
		t.Fatal("no default project")
	}
	return body.Projects[0].ID
}
