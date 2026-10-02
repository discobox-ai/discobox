package runuser

import (
	"errors"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/sandboxuser"
)

// The single-account resolver is the one darwin and windows use. It is built
// on every platform so that it is tested here, on Linux, rather than only on a
// machine the CI lane for it does not exist on yet.
func TestResolveOneAccountIsTheAgentsOwnAccount(t *testing.T) {
	t.Cleanup(FixedDatabase())
	for name, layers := range map[string]Layers{
		"nobody named":            {Image: currentAccount()},
		"the manifest names it":   {Image: currentAccount(), Manifest: &User{Name: "image"}},
		"a request names it":      {Image: currentAccount(), Manifest: &User{Name: "image"}, Request: &User{Name: " image "}},
		"a request names nothing": {Image: currentAccount(), Request: &User{}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := resolveOneAccount("darwin", layers, sandboxuser.FieldName|sandboxuser.FieldHome)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got.Name != "image" || got.HomeDirectory != "/home/image" {
				t.Fatalf("resolved %+v, want the image account and its home", got)
			}
			if got.UID != nil || got.GID != nil || got.AdditionalGroups != nil {
				t.Fatalf("resolved %+v, want no ids and no groups", got)
			}
		})
	}
}

// A request naming another user or a group set is refused with that reason,
// never ignored, which would run it as the one account anyway.
func TestResolveOneAccountRefusesARequestNamingWhatItCannotHave(t *testing.T) {
	t.Cleanup(FixedDatabase())
	for name, tc := range map[string]struct {
		request *User
		field   Fields
	}{
		"another user":     {&User{Name: "root"}, sandboxuser.FieldName},
		"a uid":            {&User{UID: sandboxuser.ID(1000)}, sandboxuser.FieldUID},
		"a primary group":  {&User{GroupName: "staff"}, sandboxuser.FieldGID},
		"a group set":      {&User{AdditionalGroups: []string{"docker"}}, sandboxuser.FieldGroups},
		"its name and ids": {&User{Name: "image", UID: sandboxuser.ID(1500), GID: sandboxuser.ID(1600)}, sandboxuser.FieldUID},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := resolveOneAccount("darwin", Layers{Image: currentAccount(), Request: tc.request}, Identity)
			var refused *sandboxuser.OneAccountError
			if !errors.As(err, &refused) {
				t.Fatalf("err = %v, want a OneAccountError", err)
			}
			if refused.Field != tc.field || refused.Account != "image" || refused.OS != "darwin" {
				t.Fatalf("refused %+v, want field %s of darwin's account image", refused, tc.field)
			}
		})
	}
}

// The manifest names the account the template provisioned, and the agent runs
// as it. A manifest naming another, or ids for it, is a sandbox assembled
// wrong, and is reported rather than papered over by running as the agent.
func TestResolveOneAccountRefusesAManifestTheAgentIsNot(t *testing.T) {
	t.Cleanup(FixedDatabase())
	for name, tc := range map[string]struct {
		manifest *User
		says     string
	}{
		"another account": {&User{Name: "dev"}, `names account "dev", but the sandbox agent runs as "image"`},
		"a uid":           {&User{Name: "image", UID: sandboxuser.ID(1500)}, "a uid (1500)"},
		"a group set":     {&User{AdditionalGroups: []string{"docker"}}, "a group set (docker)"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := resolveOneAccount("darwin", Layers{Image: currentAccount(), Manifest: tc.manifest}, Identity)
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("err = %v, want it to say %q", err, tc.says)
			}
		})
	}
}

// Ids are not fields an identity has here, so a caller that needs one to call
// setuid is told so instead of handed a zero.
func TestResolveOneAccountHasNoIDsToGive(t *testing.T) {
	t.Cleanup(FixedDatabase())
	for _, field := range []Fields{sandboxuser.FieldUID, sandboxuser.FieldGID, sandboxuser.FieldGroups} {
		_, err := resolveOneAccount("windows", Layers{Image: currentAccount()}, field)
		var unresolved *sandboxuser.UnresolvedError
		if !errors.As(err, &unresolved) || unresolved.Field != field || !strings.Contains(err.Error(), "no POSIX ids") {
			t.Errorf("need %s: err = %v, want it unresolved for want of POSIX ids", field, err)
		}
	}
}

// With no account the OS could name, only the manifest can say which it is,
// and a caller needing the name of an account nobody names is told so.
func TestResolveOneAccountWithoutAnImageAccount(t *testing.T) {
	got, err := resolveOneAccount("darwin", Layers{Manifest: &User{Name: "dev"}}, sandboxuser.FieldName)
	if err != nil || got.Name != "dev" {
		t.Fatalf("resolve = %+v, %v; want the manifest's account", got, err)
	}
	_, err = resolveOneAccount("darwin", Layers{}, sandboxuser.FieldName)
	var unresolved *sandboxuser.UnresolvedError
	if !errors.As(err, &unresolved) || unresolved.Field != sandboxuser.FieldName {
		t.Fatalf("err = %v, want the name unresolved", err)
	}
}

func TestAccountNameDropsTheWindowsDomain(t *testing.T) {
	for in, want := range map[string]string{"DESKTOP\\ada": "ada", "ada": "ada", " ada ": "ada"} {
		if got := accountName(in); got != want {
			t.Errorf("accountName(%q) = %q, want %q", in, got, want)
		}
	}
}
