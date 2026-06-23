package sandboxruntime

import (
	"testing"

	workerclient "github.com/obot-platform/discobox/worker-agent/api/gen"
	workerapimodel "github.com/obot-platform/discobox/worker-agent/api/model"
)

func TestSandboxUserUsesUIDAndGID(t *testing.T) {
	req := &workerapimodel.WorkerSandboxCreateRequest{
		User: workerclient.NewOptWorkerSandboxUser(workerapimodel.WorkerSandboxUser{
			UID: workerclient.NewOptInt64(1000),
			Gid: workerclient.NewOptInt64(1001),
		}),
	}
	if got := sandboxUser(req); got != "1000:1001" {
		t.Fatalf("sandboxUser = %q, want 1000:1001", got)
	}
}

func TestSandboxUserSkipsRootOrIncompleteIdentity(t *testing.T) {
	for name, req := range map[string]*workerapimodel.WorkerSandboxCreateRequest{
		"nil":      nil,
		"root":     {User: workerclient.NewOptWorkerSandboxUser(workerapimodel.WorkerSandboxUser{UID: workerclient.NewOptInt64(0), Gid: workerclient.NewOptInt64(0)})},
		"no gid":   {User: workerclient.NewOptWorkerSandboxUser(workerapimodel.WorkerSandboxUser{UID: workerclient.NewOptInt64(1000)})},
		"no uid":   {User: workerclient.NewOptWorkerSandboxUser(workerapimodel.WorkerSandboxUser{Gid: workerclient.NewOptInt64(1000)})},
		"username": {User: workerclient.NewOptWorkerSandboxUser(workerapimodel.WorkerSandboxUser{Name: workerclient.NewOptString("darren")})},
	} {
		t.Run(name, func(t *testing.T) {
			if got := sandboxUser(req); got != "" {
				t.Fatalf("sandboxUser = %q, want empty", got)
			}
		})
	}
}
