package secrets_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	apigen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
)

// All handoff doors share the same final authority check. Changing creator
// while retaining parent simulates adoption and must not change the outcome.
func TestHandoffAuthorityMatrix(t *testing.T) {
	for _, door := range []string{"create", "direct", "approve"} {
		for _, adopted := range []bool{false, true} {
			for _, tc := range []struct {
				name, grant string
				limit       string
				ttl         int64
				ok          bool
			}{
				{"graphql wider", "POST api.github.com/graphql graphql=mutation", "POST api.github.com/graphql graphql=query", 60, false},
				{"mcp wider", "api.github.com/mcp mcp=write_file", "api.github.com/mcp mcp=read_*", 60, false},
				{"graphql narrower", "POST api.github.com/graphql graphql=query:repository", "POST api.github.com/graphql graphql=query", 60, true},
				{"mcp narrower", "api.github.com/mcp mcp=read_file", "api.github.com/mcp mcp=read_*", 60, true},
				{"allowed", "GET api.github.com/repos/acme/web/**", "GET api.github.com/repos/acme/**", 60, true},
				{"host", "GET evil.example/repos/acme/**", "GET api.github.com/repos/acme/**", 60, false},
				{"method", "DELETE api.github.com/repos/acme/**", "GET api.github.com/repos/acme/**", 60, false},
				{"path", "GET api.github.com/repos/other/**", "GET api.github.com/repos/acme/**", 60, false},
				{"lifetime", "GET api.github.com/repos/acme/**", "GET api.github.com/repos/acme/**", 7200, false},
				{"unlimited", "GET api.github.com/repos/acme/**", "GET api.github.com/repos/acme/**", 0, false},
			} {
				t.Run(door+"/"+map[bool]string{true: "adopted", false: "created"}[adopted]+"/"+tc.name, func(t *testing.T) {
					ctx := testPrincipalContext()
					svc, st := newAgentCredentialService(t)
					secret := createBearerSecret(ctx, t, svc)
					exp := time.Now().UTC().Add(time.Hour)
					limit, _ := secretroute.Parse(tc.limit)
					parent := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: leadID, Delegate: true, Routes: []model.SecretRoute{limit}, ExpiresAt: &exp})
					if adopted {
						if err := st.Transaction(ctx, func(_ *store.Store, tx *gorm.DB) error {
							return tx.Model(&model.Sandbox{}).Where("id = ?", testSandboxID).Update("created_by_sandbox_id", "sbx-original-creator").Error
						}); err != nil {
							t.Fatal(err)
						}
					}
					r, _ := secretroute.Parse(tc.grant)
					routes, err := services.Convert[[]apimodel.SecretRouteInput]([]model.SecretRoute{r})
					if err != nil {
						t.Fatal(err)
					}

					var receipt *model.SecretGrantReceipt
					switch door {
					case "direct":
						receipt, err = svc.CreateSandboxSecretBinding(asLead(), "project-1", testSandboxID, services.CreateSandboxSecretBindingBody{Secret: secret.ID, EnvVar: apigen.NewOptString("GH"), Routes: routes, TtlSeconds: apigen.NewOptInt64(tc.ttl)})
					case "create":
						target := &model.Sandbox{ID: "sbx-new", ProjectID: "project-1", Name: "new", PoolID: testPoolID, CreatedBySandboxID: ptrString(leadID)}
						var prepared resourcesecrets.SandboxBindings
						prepared, err = svc.PrepareSandboxBindings(asLead(), "project-1", target, []apimodel.SandboxBindingInput{{Secret: secret.ID, EnvVar: apigen.NewOptString("GH"), Routes: routes, TtlSeconds: apigen.NewOptInt64(tc.ttl)}})
						if err == nil {
							err = st.Transaction(ctx, func(txStore *store.Store, _ *gorm.DB) error {
								if err := txStore.CreateSandbox(ctx, target); err != nil {
									return err
								}
								return resourcesecrets.StoreSandboxBindingsTx(ctx, txStore, prepared)
							})
						}
						if err == nil {
							receipt = prepared.Bindings[0].Receipt
						} else {
							if _, e := st.GetSandbox(ctx, "project-1", target.ID); e == nil {
								t.Fatal("rejected create left a sandbox")
							}
						}
					case "approve":
						req := createAgentRequest(ctx, t, svc)
						decision := apigen.SecretRequestPartDecision{Decision: apigen.SecretRequestPartDecisionDecisionApprove, Routes: routes, TtlSeconds: apigen.NewOptInt64(tc.ttl)}
						var answer *model.SecretRequest
						answer, err = svc.ApproveSecretRequest(asLead(), "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: apigen.NewOptString(secret.ID), Use: apigen.NewOptSecretRequestPartDecision(decision)})
						if err == nil {
							receipt = answer.Use.Receipt
						} else {
							stored, e := st.GetSecretRequest(ctx, "project-1", req.ID)
							if e != nil || stored.Status != model.SecretRequestStatusPending {
								t.Fatalf("rejection changed request: %+v %v", stored, e)
							}
						}
					}
					if tc.ok {
						if err != nil {
							t.Fatal(err)
						}
						if receipt == nil || receipt.ParentID != parent.ID || receipt.Actor != leadID || receipt.BindingID == "" || len(receipt.RouteLists) < 2 {
							t.Fatalf("receipt = %+v", receipt)
						}
					} else if !isStatus(err, http.StatusForbidden) {
						t.Fatalf("got %v, want 403", err)
					}
				})
			}
		}
	}
}

func ptrString(s string) *string { return &s }

func TestHandoffRechecksPreparedAuthority(t *testing.T) {
	for _, change := range []string{"reparent", "api off", "delegate routes", "expiry", "revoke"} {
		t.Run(change, func(t *testing.T) {
			ctx := testPrincipalContext()
			svc, st := newAgentCredentialService(t)
			secret := createBearerSecret(ctx, t, svc)
			p := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: leadID, Delegate: true})
			child, _ := st.GetSandbox(ctx, "project-1", testSandboxID)
			prepared, err := svc.PrepareSandboxBindings(asLead(), "project-1", child, []apimodel.SandboxBindingInput{{Secret: secret.ID, EnvVar: apigen.NewOptString("GH"), Routes: []apimodel.SecretRouteInput{{Hosts: []string{"api.github.com"}}}}})
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "reparent":
				err = st.Transaction(ctx, func(_ *store.Store, tx *gorm.DB) error {
					return tx.Model(&model.Sandbox{}).Where("id = ?", testSandboxID).Update("parent_sandbox_id", "sbx-other").Error
				})
			case "api off":
				err = st.SetSandboxDiscoboxAPI(ctx, "project-1", leadID, false)
			case "delegate routes":
				p.Routes = []model.SecretRoute{{Hosts: []string{"other.example"}}}
				err = st.UpdateSecretBinding(ctx, p, store.BindingChange{Actor: "person"})
			case "expiry":
				at := time.Now().Add(-time.Second)
				p.ExpiresAt = &at
				err = st.UpdateSecretBinding(ctx, p, store.BindingChange{Actor: "person"})
			case "revoke":
				_, err = st.DeleteSecretBinding(ctx, "project-1", p.ID, store.BindingChange{Actor: "person"})
			}
			if err != nil {
				t.Fatal(err)
			}
			err = st.Transaction(ctx, func(txStore *store.Store, _ *gorm.DB) error {
				return resourcesecrets.StoreSandboxBindingsTx(context.Background(), txStore, prepared)
			})
			if !isStatus(err, 403) {
				t.Fatalf("commit after %s = %v", change, err)
			}
		})
	}
}

func TestHandoffRefusesTargetsOutsideCurrentAuthority(t *testing.T) {
	for _, door := range []string{"direct", "approve"} {
		for _, fault := range []string{"unled", "self", "archived", "no api", "wrong secret", "use only"} {
			t.Run(door+"/"+fault, func(t *testing.T) {
				ctx := testPrincipalContext()
				svc, st := newAgentCredentialService(t)
				secret := createBearerSecret(ctx, t, svc)
				p := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: leadID, Delegate: true})
				target := testSandboxID
				switch fault {
				case "unled":
					if err := st.Transaction(ctx, func(_ *store.Store, tx *gorm.DB) error {
						return tx.Model(&model.Sandbox{}).Where("id = ?", target).Update("parent_sandbox_id", nil).Error
					}); err != nil {
						t.Fatal(err)
					}
				case "self":
					target = leadID
				case "archived":
					if err := st.Transaction(ctx, func(_ *store.Store, tx *gorm.DB) error {
						return tx.Model(&model.Sandbox{}).Where("id = ?", target).Update("desired_state", model.DesiredStateArchived).Error
					}); err != nil {
						t.Fatal(err)
					}
				case "no api":
					if err := st.SetSandboxDiscoboxAPI(ctx, "project-1", leadID, false); err != nil {
						t.Fatal(err)
					}
				case "wrong secret":
					secret = createRoutedSecret(ctx, t, svc, "other", "api.github.com", 0)
				case "use only":
					p.Delegate = false
					p.Use = true
					p.EnvName = "OWN"
					if err := st.UpdateSecretBinding(ctx, p, store.BindingChange{Actor: "person"}); err != nil {
						t.Fatal(err)
					}
				}
				var err error
				if door == "direct" {
					_, err = svc.CreateSandboxSecretBinding(asLead(), "project-1", target, services.CreateSandboxSecretBindingBody{Secret: secret.ID, EnvVar: apigen.NewOptString("GH")})
				} else {
					req := createAgentRequest(ctx, t, svc)
					if target != testSandboxID {
						if err := st.Transaction(ctx, func(_ *store.Store, tx *gorm.DB) error {
							return tx.Model(&model.SecretRequest{}).Where("id = ?", req.ID).Update("sandbox_id", target).Error
						}); err != nil {
							t.Fatal(err)
						}
					}
					_, err = svc.ApproveSecretRequest(asLead(), "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: apigen.NewOptString(secret.ID), Use: decide(true)})
				}
				if !isStatus(err, 403) {
					t.Fatalf("%s %s = %v", door, fault, err)
				}
			})
		}
	}
}

func TestReceiptsAreMutationResultsAndPrivateHistory(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: leadID, Delegate: true})
	req := createAgentRequest(ctx, t, svc)
	approved, err := svc.ApproveSecretRequest(asLead(), "project-1", req.ID, services.ApproveSecretRequestBody{Use: decide(true)})
	if err != nil {
		t.Fatal(err)
	}
	if approved.Use.Receipt == nil {
		t.Fatal("mutation returned no receipt")
	}
	person, err := svc.GetSecretRequest(ctx, "project-1", req.ID)
	if err != nil || person.Use.Receipt == nil {
		t.Fatalf("person history = %+v, %v", person, err)
	}
	for _, lead := range []string{leadID, "sbx-adopter"} {
		if err := st.Transaction(ctx, func(_ *store.Store, tx *gorm.DB) error {
			return tx.Model(&model.Sandbox{}).Where("id = ?", testSandboxID).Update("parent_sandbox_id", lead).Error
		}); err != nil {
			t.Fatal(err)
		}
		caller := auth.WithPrincipal(ctx, auth.Principal{Type: auth.PrincipalTypeSandbox, SandboxID: lead, ProjectID: "project-1"})
		got, err := svc.GetSecretRequest(caller, "project-1", req.ID)
		if err != nil || got.Use.Receipt != nil {
			t.Fatalf("sandbox get disclosed receipt: %+v %v", got, err)
		}
		listed, err := svc.ListSecretRequests(caller, "project-1", "")
		if err != nil || len(listed) != 1 || listed[0].Use.Receipt != nil {
			t.Fatalf("sandbox list disclosed receipt: %+v %v", listed, err)
		}
	}
}
