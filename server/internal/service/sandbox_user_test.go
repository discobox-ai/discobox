package service_test

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/platform"
	"github.com/discobox-ai/discobox/pkg/randomname"
	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
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
			config := serverapi.SandboxCreateConfig{Name: serverapi.NewOptString("alpha")}
			config.SetUser(serverapi.NewOptSandboxUser(tc.user))
			_, err := svc.CreateSandbox(context.Background(), projectID, services.CreateSandboxBody{HarnessName: serverapi.NewOptString("shell"), Config: serverapi.NewOptSandboxCreateConfig(config)})
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
		config := serverapi.SandboxCreateConfig{Name: serverapi.NewOptString("alpha")}
		config.SetUser(serverapi.NewOptSandboxUser(user))
		if _, err := svc.CreateSandbox(context.Background(), projectID, services.CreateSandboxBody{HarnessName: serverapi.NewOptString("shell"), Config: serverapi.NewOptSandboxCreateConfig(config)}); err != nil {
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

// declareDefaultPoolPlatform has the project's default pool declare the
// platform its sandboxes run, as its agent does on its first report.
func declareDefaultPoolPlatform(t *testing.T, st *store.Store, projectID string, hosts platform.Platform, kind platform.ImageKind) {
	t.Helper()
	ctx := context.Background()
	project, err := st.GetProject(ctx, projectID)
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if _, _, err := st.UpdatePoolStatus(ctx, project.DefaultPoolID, hosts, kind, store.AgentDeclarations{}, true, true, false, 1, 1, 1, nil); err != nil {
		t.Fatalf("declare pool platform: %v", err)
	}
}

// A create that names no user gets an ordinary account, never the image's
// own user, which is root on the built-in image (ADR 26-11-19-859): an
// omitted user and an empty one alike.
func TestCreateSandboxDefaultsToAnOrdinaryAccount(t *testing.T) {
	for name, config := range map[string]serverapi.SandboxCreateConfig{
		"no user":    {Name: serverapi.NewOptString("alpha")},
		"empty user": {Name: serverapi.NewOptString("alpha"), User: serverapi.NewOptSandboxUser(serverapi.SandboxUser{})},
		"blank user": {Name: serverapi.NewOptString("alpha"), User: serverapi.NewOptSandboxUser(serverapi.SandboxUser{
			Name: serverapi.NewOptString(""), HomeDirectory: serverapi.NewOptString(" "), GroupName: serverapi.NewOptString(""),
		})},
	} {
		t.Run(name, func(t *testing.T) {
			svc, _, _, projectID := newSandboxTestService(t, nil)
			created, err := svc.CreateSandbox(context.Background(), projectID, services.CreateSandboxBody{
				HarnessName: serverapi.NewOptString("shell"),
				Config:      serverapi.NewOptSandboxCreateConfig(config),
			})
			if err != nil {
				t.Fatalf("create sandbox: %v", err)
			}
			if created.UserUID == nil {
				t.Fatal("sandbox UID is unset, want a default non-root UID")
			}
			if *created.UserUID == 0 {
				t.Fatal("sandbox UID is root, want a default non-root UID")
			}
			if created.UserName == nil || *created.UserName != "discobox" || *created.UserUID != 1000 ||
				created.UserGID == nil || *created.UserGID != 1000 ||
				created.HomeDirectory == nil || *created.HomeDirectory != "/home/discobox" {
				t.Fatalf("sandbox user = %s, want discobox 1000:1000 /home/discobox", describeSandboxUser(created))
			}
		})
	}
}

// Root is only ever asked for, and is kept when it is.
func TestCreateSandboxKeepsAnExplicitRoot(t *testing.T) {
	svc, _, _, projectID := newSandboxTestService(t, nil)
	config := serverapi.SandboxCreateConfig{}
	config.SetUser(serverapi.NewOptSandboxUser(serverapi.SandboxUser{UID: serverapi.NewOptInt64(0)}))
	created, err := svc.CreateSandbox(context.Background(), projectID, services.CreateSandboxBody{
		HarnessName: serverapi.NewOptString("shell"),
		Config:      serverapi.NewOptSandboxCreateConfig(config),
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if created.UserUID == nil || *created.UserUID != 0 || created.UserName != nil || created.HomeDirectory != nil {
		t.Fatalf("sandbox user = %s, want uid 0 alone", describeSandboxUser(created))
	}
}

// Which default account a sandbox gets is its guest's, so a pool that has not
// said what its sandboxes run is waited for, never judged as Linux.
func TestCreateSandboxWithNoUserWaitsForThePoolsPlatform(t *testing.T) {
	svc, _, st, projectID := newSandboxTestService(t, nil)
	ctx := context.Background()
	project, err := st.GetProject(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	defaultPool, err := st.GetPool(ctx, projectID, project.DefaultPoolID)
	if err != nil {
		t.Fatal(err)
	}
	undeclared := &model.Pool{ID: "pool-undeclared", ProjectID: projectID, PoolManifest: model.PoolManifest{
		Name: "Undeclared", ProviderInstanceID: defaultPool.ProviderInstanceID,
	}}
	if err := st.CreatePool(ctx, undeclared); err != nil {
		t.Fatal(err)
	}
	_, err = svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{HarnessName: serverapi.NewOptString("shell"), PoolId: serverapi.NewOptString(undeclared.ID)})
	var statusErr interface{ StatusCode() int }
	if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("create err = %v, want 503", err)
	}
}

// A macOS sandbox's one account is its template's, so a create naming none
// records none.
func TestCreateSandboxWithNoUserOnMacOSTakesTheTemplatesAccount(t *testing.T) {
	svc, _, st, projectID := newSandboxTestService(t, nil)
	declareDefaultPoolPlatform(t, st, projectID, platform.Platform{OS: platform.Darwin, Arch: "arm64"}, platform.VM("vz"))
	created, err := svc.CreateSandbox(context.Background(), projectID, services.CreateSandboxBody{HarnessName: serverapi.NewOptString("shell")})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if created.UserName != nil || created.UserUID != nil || created.UserGID != nil || created.HomeDirectory != nil {
		t.Fatalf("sandbox user = %s, want none recorded", describeSandboxUser(created))
	}
}

// A create that names no handle is given one, unique in the project; one that
// names it keeps it as given, and a blank one is refused.
func TestCreateSandboxDrawsAnOmittedHandle(t *testing.T) {
	svc, _, _, projectID := newSandboxTestService(t, nil)
	ctx := context.Background()
	seen := map[string]bool{}
	for range 3 { // with only a harness named
		created, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{HarnessName: serverapi.NewOptString("shell")})
		if err != nil {
			t.Fatalf("create sandbox naming only its harness: %v", err)
		}
		if strings.TrimSpace(created.Name) == "" || seen[created.Name] {
			t.Fatalf("drawn handle %q is blank or repeated (%v)", created.Name, seen)
		}
		seen[created.Name] = true
	}
	named := serverapi.SandboxCreateConfig{Name: serverapi.NewOptString("Given_Handle")}
	created, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{HarnessName: serverapi.NewOptString("shell"), Config: serverapi.NewOptSandboxCreateConfig(named)})
	if err != nil || created.Name != "Given_Handle" {
		t.Fatalf("create named = %v, %v; want it kept as given", created, err)
	}
	var statusErr interface{ StatusCode() int }
	if _, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{HarnessName: serverapi.NewOptString("shell"), Config: serverapi.NewOptSandboxCreateConfig(named)}); !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusConflict {
		t.Fatalf("create with a taken handle err = %v, want 409", err)
	}
	blank := serverapi.SandboxCreateConfig{Name: serverapi.NewOptString("  ")}
	if _, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{HarnessName: serverapi.NewOptString("shell"), Config: serverapi.NewOptSandboxCreateConfig(blank)}); !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusBadRequest {
		t.Fatalf("create with a blank handle err = %v, want 400", err)
	}
}

func describeSandboxUser(sb *model.Sandbox) string {
	field := func(name string, v any) string { return name + "=" + fmtPtr(v) }
	return strings.Join([]string{
		field("name", sb.UserName), field("uid", sb.UserUID), field("gid", sb.UserGID), field("home", sb.HomeDirectory),
	}, " ")
}

func fmtPtr(v any) string {
	switch p := v.(type) {
	case *string:
		if p != nil {
			return *p
		}
	case *int:
		if p != nil {
			return strconv.Itoa(*p)
		}
	}
	return "<unset>"
}

// A drawn handle the index refuses is drawn again, each in a transaction of
// its own, a bounded number of times: with every handle the draw can give
// taken, the create gives up with 503 and creates nothing.
func TestCreateSandboxGivesUpWhenEveryDrawnHandleIsTaken(t *testing.T) {
	svc, _, st, projectID := newSandboxTestService(t, nil)
	ctx := context.Background()
	project, err := st.GetProject(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	// Every handle the draw gives: draw until a long run brings no new one.
	taken := map[string]struct{}{}
	for quiet := 0; quiet < 200000; quiet++ {
		name, err := randomname.Generate()
		if err != nil {
			t.Fatal(err)
		}
		if _, seen := taken[name]; !seen {
			taken[name] = struct{}{}
			quiet = 0
		}
	}
	rows := make([]model.Sandbox, 0, len(taken))
	for name := range taken {
		rows = append(rows, model.Sandbox{ID: "sbx_taken_" + name, ProjectID: projectID, PoolID: project.DefaultPoolID, CreatedByUserID: "user-1", Name: name})
	}
	if err := st.Transaction(ctx, func(_ *store.Store, tx *gorm.DB) error {
		return tx.CreateInBatches(rows, 20).Error
	}); err != nil {
		t.Fatalf("take every handle: %v", err)
	}
	_, err = svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{HarnessName: serverapi.NewOptString("shell")})
	var statusErr interface{ StatusCode() int }
	if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("create err = %v, want 503", err)
	}
	var count int64
	if err := st.Transaction(ctx, func(_ *store.Store, tx *gorm.DB) error {
		return tx.Model(&model.Sandbox{}).Where("project_id = ?", projectID).Count(&count).Error
	}); err != nil {
		t.Fatal(err)
	}
	if count != int64(len(rows)) {
		t.Fatalf("%d sandboxes after the refused create, want the %d taken", count, len(rows))
	}
}
