package sandboxruntime

import (
	"testing"

	workerclient "github.com/obot-platform/discobox/worker-agent/api/gen"
	workerapimodel "github.com/obot-platform/discobox/worker-agent/api/model"
)

func TestSandboxUserUsesUIDAndGID(t *testing.T) {
	req := &workerapimodel.WorkerSandboxCreateRequest{
		UserUid: workerclient.NewOptInt64(1000),
		UserGid: workerclient.NewOptInt64(1001),
	}
	if got := sandboxUser(req); got != "1000:1001" {
		t.Fatalf("sandboxUser = %q, want 1000:1001", got)
	}
}

func TestSandboxUserSkipsRootOrIncompleteIdentity(t *testing.T) {
	for name, req := range map[string]*workerapimodel.WorkerSandboxCreateRequest{
		"nil":      nil,
		"root":     {UserUid: workerclient.NewOptInt64(0), UserGid: workerclient.NewOptInt64(0)},
		"no gid":   {UserUid: workerclient.NewOptInt64(1000)},
		"no uid":   {UserGid: workerclient.NewOptInt64(1000)},
		"username": {UserName: workerclient.NewOptString("darren")},
	} {
		t.Run(name, func(t *testing.T) {
			if got := sandboxUser(req); got != "" {
				t.Fatalf("sandboxUser = %q, want empty", got)
			}
		})
	}
}
