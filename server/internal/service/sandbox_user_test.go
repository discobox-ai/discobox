package service_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// A sandbox's user is the account boot creates and the pool agent chowns a
// source tree to before the sandbox exists, so create refuses one it could not
// make with a usable uid: an id outside root and the guest's account range, or
// a name with no uid at all. Refused, not clamped (ADR 0141).
func TestCreateSandboxRefusesAUserNoSandboxAccountCanHave(t *testing.T) {
	for _, tc := range []struct {
		name string
		user serverapi.SandboxUser
		want string
	}{
		{"a macOS uid", sandboxUser("ada", 501, 20), "uid 501"},
		{"a system gid", sandboxUser("ada", 1000, 100), "gid 100"},
		{"a uid above the range", sandboxUser("ada", 70000, 70000), "uid 70000"},
		{"a name with no uid", serverapi.SandboxUser{Name: serverapi.NewOptString("ada")}, "must give its uid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, _, projectID := newSandboxTestService(t, nil)
			config := serverapi.SandboxCreateConfig{Name: "alpha"}
			config.SetUser(serverapi.NewOptSandboxUser(tc.user))
			_, err := svc.CreateSandbox(context.Background(), projectID, services.CreateSandboxBody{HarnessName: serverapi.NewOptString("shell"), Config: config})
			var statusErr interface{ StatusCode() int }
			if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusBadRequest || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("create err = %v, want a 400 naming %q", err, tc.want)
			}
		})
	}
}

func TestCreateSandboxAcceptsAUserInTheGuestRangeAndRoot(t *testing.T) {
	for _, user := range []serverapi.SandboxUser{
		sandboxUser("ada", 1000, 1000),
		sandboxUser("ada", 60000, 60000),
		sandboxUser("root", 0, 0),
	} {
		svc, _, _, projectID := newSandboxTestService(t, nil)
		config := serverapi.SandboxCreateConfig{Name: "alpha"}
		config.SetUser(serverapi.NewOptSandboxUser(user))
		if _, err := svc.CreateSandbox(context.Background(), projectID, services.CreateSandboxBody{HarnessName: serverapi.NewOptString("shell"), Config: config}); err != nil {
			t.Fatalf("create with %+v: %v", user, err)
		}
	}
}

func sandboxUser(name string, uid, gid int64) serverapi.SandboxUser {
	return serverapi.SandboxUser{
		Name: serverapi.NewOptString(name),
		UID:  serverapi.NewOptInt64(uid),
		Gid:  serverapi.NewOptInt64(gid),
	}
}
