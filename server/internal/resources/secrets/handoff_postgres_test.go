package secrets_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	apigen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestPostgresHandoffLockRaces(t *testing.T) {
	for _, scenario := range []string{"expiry", "revoke", "revision"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			svc, st, admin := postgresSchemaService(t, "handoff_"+scenario)
			secret := createBearerSecret(testPrincipalContext(), t, svc)
			parent := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: leadID, Delegate: true})
			var req *model.SecretRequest
			if scenario == "revision" {
				req = createAgentRequest(testPrincipalContext(), t, svc)
			}
			locked, release := make(chan struct{}), make(chan struct{})
			blocker := make(chan error, 1)
			go func() {
				blocker <- st.Transaction(ctx, func(txStore *store.Store, tx *gorm.DB) error {
					var row any = &model.SecretBinding{}
					id := parent.ID
					if scenario == "revision" {
						row = &model.Secret{}
						id = secret.ID
					}
					if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(row, "id = ?", id).Error; err != nil {
						return err
					}
					close(locked)
					<-release
					if scenario == "revision" {
						return tx.Model(&model.Secret{}).Where("id = ?", secret.ID).Update("revision", secret.Revision+1).Error
					}
					if scenario == "expiry" {
						return tx.Model(&model.SecretBinding{}).Where("id = ?", parent.ID).Update("expires_at", time.Now().Add(-time.Millisecond)).Error
					}
					child := &model.SecretBinding{ProjectID: "project-1", SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, EnvName: "GH", ParentID: parent.ID}
					return txStore.CreateSecretBinding(ctx, child, store.BindingChange{Actor: leadID})
				})
			}()
			<-locked
			result := make(chan error, 1)
			go func() {
				if scenario == "revision" {
					answer, err := svc.ApproveSecretRequest(asLead(), "project-1", req.ID, services.ApproveSecretRequestBody{Use: decide(true)})
					if err == nil && (answer.Use.Grant == nil || answer.Use.Grant.SecretRevision != secret.Revision+1) {
						err = fmt.Errorf("approval grant revision = %+v, want %d", answer.Use.Grant, secret.Revision+1)
					}
					result <- err
					return
				}
				if scenario == "expiry" {
					_, err := svc.CreateSandboxSecretBinding(asLead(), "project-1", testSandboxID, services.CreateSandboxSecretBindingBody{Secret: secret.ID, EnvVar: apigen.NewOptString("GH")})
					result <- err
					return
				}
				deleted, err := svc.DeleteSecretBinding(testPrincipalContext(), "project-1", parent.ID)
				if err == nil && len(deleted) != 2 {
					err = fmt.Errorf("revoked %v, want parent and concurrently inserted child", deleted)
				}
				result <- err
			}()
			// Observe the real waiter, rather than guessing whether it reached the lock.
			waiting := waitForLockWaiter(t, admin, "secret")
			close(release)
			if e := <-blocker; e != nil {
				t.Fatal(e)
			}
			got := <-result
			if !waiting {
				t.Fatalf("no row-lock waiter observed: %v", got)
			}
			if scenario == "expiry" {
				if !isStatus(got, 403) {
					t.Fatalf("grant after expiry while blocked = %v", got)
				}
			} else if got != nil {
				t.Fatal(got)
			}
		})
	}
}
