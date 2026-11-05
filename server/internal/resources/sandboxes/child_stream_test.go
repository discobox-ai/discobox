package sandboxes

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/proxy/egress"
	"github.com/discobox-ai/discobox/server/internal/auth"
	poolauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
)

func childFixture(t *testing.T, handler http.HandlerFunc) (*Service, context.Context) {
	t.Helper()
	s := metaFixture(t, handler)
	ctx := context.Background()
	lead := "lead"
	if err := s.store.CreateSandbox(ctx, &model.Sandbox{ID: lead, ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", DiscoboxAPI: true, Name: lead}); err != nil {
		t.Fatal(err)
	}
	req := &model.AdoptionRequest{ProjectID: "project-1", RequesterSandboxID: lead, ChildSandboxID: "sb-1", Justification: "manage"}
	if err := s.store.CreateAdoptionRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.DecideAdoptionRequest(ctx, "project-1", req.ID, "user-1", true); err != nil {
		t.Fatal(err)
	}
	return s, auth.WithPrincipal(ctx, auth.Principal{Type: auth.PrincipalTypeSandbox, ProjectID: "project-1", SandboxID: lead, UserID: "user-1"})
}
func TestDirectAcquireChecksChildBoundary(t *testing.T) {
	s, ctx := childFixture(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	for _, scope := range []string{poolauth.ScopeExecRead, poolauth.ScopeExecWrite, poolauth.ScopeFilesRead, poolauth.ScopeFilesWrite} {
		lease, _, err := s.AcquireSandboxHTTPClient(ctx, "project-1", "sb-1", []string{scope}, services.SandboxRead)
		if err != nil {
			t.Fatalf("authorized %s: %v", scope, err)
		}
		lease.Release()
		for _, target := range []string{"lead", "missing"} {
			if lease, _, err := s.AcquireSandboxHTTPClient(ctx, "project-1", target, []string{scope}, services.SandboxRead); err == nil {
				lease.Release()
				t.Fatalf("permitted %s on %s", scope, target)
			}
		}
	}
	if err := s.store.SetSandboxDiscoboxAPI(context.Background(), "project-1", "lead", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcquireSandboxHTTPClient(ctx, "project-1", "sb-1", []string{poolauth.ScopeFilesRead}, services.SandboxRead); err == nil {
		t.Fatal("disabled API allowed direct acquire")
	}
}

//nolint:bodyclose // Every response is deferred closed; a goroutine observes cancellation.
func TestChildStreamsRevokeOnCommittedChanges(t *testing.T) {
	for _, change := range []string{"release", "api-disable", "caller-delete", "egress"} {
		t.Run(change, func(t *testing.T) {
			started := make(chan struct{})
			s, ctx := childFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				close(started)
				<-r.Context().Done()
			})
			scope := poolauth.ScopeFilesRead
			if change == "egress" {
				scope = poolauth.ScopeFilesWrite
			}
			lease, _, err := s.AcquireSandboxHTTPClient(ctx, "project-1", "sb-1", []string{scope}, services.SandboxRead)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, lease.BaseURL+"/files/content", nil)
			//nolint:bodyclose // Closed by the defer below; the goroutine only reads it.
			resp, err := lease.Client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			<-started
			done := make(chan error, 1)
			go func() { _, err := io.Copy(io.Discard, resp.Body); done <- err }()
			switch change {
			case "release":
				err = s.store.ReleaseSandbox(context.Background(), "project-1", "sb-1", "lead")
			case "api-disable":
				err = s.store.SetSandboxDiscoboxAPI(context.Background(), "project-1", "lead", false)
			case "caller-delete":
				caller, e := s.store.GetSandbox(context.Background(), "project-1", "lead")
				if e != nil {
					t.Fatal(e)
				}
				caller.DesiredState = model.DesiredStateDeleted
				err = s.store.UpdateSandbox(context.Background(), caller)
			case "egress":
				err = s.store.SetProjectEgress(context.Background(), "project-1", egress.Policy{DenyByDefault: true})
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("revoked stream ended as successful body")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("stream retained authority")
			}
		})
	}
}

//nolint:bodyclose // The response is deferred closed; a goroutine observes cancellation.
func TestReadStreamsSurviveEgressOnlyChanges(t *testing.T) {
	started := make(chan struct{})
	finish := make(chan struct{})
	defer close(finish)
	s, ctx := childFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-r.Context().Done():
		case <-finish:
		}
	})
	lease, _, err := s.AcquireSandboxHTTPClient(ctx, "project-1", "sb-1", []string{poolauth.ScopeFilesRead}, services.SandboxRead)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, lease.BaseURL+"/files/content", nil)
	resp, err := lease.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-started
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, resp.Body); done <- err }()
	if err := s.store.SetProjectEgress(context.Background(), "project-1", egress.Policy{DenyByDefault: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("egress edit ended readable stream: %v", err)
	case <-time.After(250 * time.Millisecond):
	}
	if err := s.store.ReleaseSandbox(context.Background(), "project-1", "sb-1", "lead"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("release did not close read")
	}
}

func TestDeletedCallerCannotStartAStreamAfterAcquire(t *testing.T) {
	reached := false
	s, ctx := childFixture(t, func(w http.ResponseWriter, _ *http.Request) { reached = true; w.WriteHeader(200) })
	lease, _, err := s.AcquireSandboxHTTPClient(ctx, "project-1", "sb-1", []string{poolauth.ScopeFilesRead}, services.SandboxRead)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	caller, err := s.store.GetSandbox(context.Background(), "project-1", "lead")
	if err != nil {
		t.Fatal(err)
	}
	caller.DesiredState = model.DesiredStateDeleted
	if err := s.store.UpdateSandbox(context.Background(), caller); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, lease.BaseURL+"/files/content", nil)
	resp, err := lease.Client.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil || reached {
		t.Fatalf("deleted caller began request: reached=%t err=%v", reached, err)
	}
}
