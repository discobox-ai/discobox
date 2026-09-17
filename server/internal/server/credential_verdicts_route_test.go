package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pool-agent/poolauth"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// The verdict trail end to end: written by a pool through the broker route,
// read back by a project member through list-credential-verdicts, after the
// sandbox it describes is gone. Every layer between runs for real — ogen's
// query decoding of the tri-state allow and the date-time since, the project
// authorizer, the handler, the store.
func TestCredentialVerdictsReadBackAfterTheirSandboxIsGone(t *testing.T) {
	skipWithoutDocker(t)
	ctx := context.Background()
	db := newAppTestDB(ctx, t)
	router := newTestApp(ctx, t, db)
	projectID, privateKey := seedCredentialRoutePool(ctx, t, db.Write, router)
	broker := signPoolAssertion(t, projectID, routeTestPoolID, privateKey, poolauth.ScopeCredentialBroker)
	before := time.Now().UTC().Add(-time.Minute)

	for _, body := range []string{
		`{"sandboxId":"` + routeTestSandboxID + `","useId":"use_issued","command":["gh","pr","create"],` +
			`"verdict":{"allow":true,"reason":"matches the approved use","role":"judge","prompt":"facts","latencyMs":400},"volunteered":false}`,
		`{"sandboxId":"` + routeTestSandboxID + `","useId":"use_issued","command":["gh","repo","delete"],` +
			`"verdict":{"allow":false,"reason":"not what was approved","role":"judge","prompt":"facts","latencyMs":350},"volunteered":true}`,
	} {
		resp := callRoute(t, router, http.MethodPost, "/api/pools/"+routeTestPoolID+"/sandbox-credential-verdicts", body, broker)
		if resp.Code != http.StatusNoContent {
			t.Fatalf("record status = %d, body = %s", resp.Code, resp.Body.String())
		}
	}

	// Purge the sandbox out from under its trail.
	if err := db.Write.WithContext(ctx).Delete(&model.Sandbox{ID: routeTestSandboxID}).Error; err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}

	list := func(t *testing.T, query url.Values) []model.CredentialVerdict {
		t.Helper()
		resp := callRoute(t, router, http.MethodGet, "/projects/"+projectID+"/credential-verdicts?"+query.Encode(), "", "")
		if resp.Code != http.StatusOK {
			t.Fatalf("list %v status = %d, body = %s", query, resp.Code, resp.Body.String())
		}
		var body struct {
			CredentialVerdicts []model.CredentialVerdict `json:"credentialVerdicts"`
		}
		if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body.CredentialVerdicts
	}

	all := list(t, url.Values{"sandboxId": {routeTestSandboxID}})
	if len(all) != 2 {
		t.Fatalf("got %d verdicts for the purged sandbox, want 2", len(all))
	}

	// Bounds are sent with offsets at the two ends of the range, the way a
	// client in another zone sends them. SQLite compares times as text, so a
	// bound is only read as an instant if the store puts it in the zone the
	// rows are in. Earliest-possible and latest-possible offsets make any
	// server zone disagree with them, so this fails without that step wherever
	// the test runs.
	farEast := time.FixedZone("UTC+14", 14*60*60)
	farWest := time.FixedZone("UTC-12", -12*60*60)
	denials := list(t, url.Values{"allow": {"false"}, "since": {before.In(farEast).Format(time.RFC3339)}})
	if len(denials) != 1 || denials[0].Allow || !denials[0].Volunteered || denials[0].Reason != "not what was approved" {
		t.Fatalf("denials = %+v, want the one reported denial", denials)
	}

	if future := list(t, url.Values{"since": {time.Now().Add(time.Hour).In(farWest).Format(time.RFC3339)}}); len(future) != 0 {
		t.Fatalf("since an hour ahead returned %d verdicts, want none", len(future))
	}
}

func TestCredentialVerdictsRejectsAnOutOfRangeLimit(t *testing.T) {
	skipWithoutDocker(t)
	ctx := context.Background()
	db := newAppTestDB(ctx, t)
	router := newTestApp(ctx, t, db)
	projectID := defaultProjectID(ctx, t, router)
	for _, limit := range []string{"0", "1001"} {
		resp := callRoute(t, router, http.MethodGet, "/projects/"+projectID+"/credential-verdicts?limit="+limit, "", "")
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("limit=%s status = %d, want 400", limit, resp.Code)
		}
	}
}
