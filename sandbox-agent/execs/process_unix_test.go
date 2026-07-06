//go:build !windows

package execs

import (
	osuser "os/user"
	"strconv"
	"testing"
)

func TestUserCredentialDefaultsGIDFromUID(t *testing.T) {
	uid := int64(1234)
	credential, ok, err := userCredential(&User{UID: &uid})
	if err != nil {
		t.Fatalf("user credential: %v", err)
	}
	if !ok {
		t.Fatal("credential was not set")
	}
	if credential.Uid != 1234 || credential.Gid != 1234 {
		t.Fatalf("credential = %d:%d, want 1234:1234", credential.Uid, credential.Gid)
	}
	if !credential.NoSetGroups {
		t.Fatal("credential should not grant supplementary groups")
	}
}

func TestUserEnvDefaultsResolveHomeForNamedUser(t *testing.T) {
	current, err := osuser.Current()
	if err != nil {
		t.Skipf("current user unavailable: %v", err)
	}
	if current.Username == "" || current.HomeDir == "" {
		t.Skipf("current user lacks username or home: %#v", current)
	}

	env, err := userEnvDefaults(&User{Name: current.Username})
	if err != nil {
		t.Fatalf("user env defaults: %v", err)
	}
	if env["USER"] == "" || env["LOGNAME"] == "" || env["HOME"] != current.HomeDir {
		t.Fatalf("env = %#v, want current user home", env)
	}
}

func TestUserEnvDefaultsResolveHomeForNumericUser(t *testing.T) {
	current, err := osuser.Current()
	if err != nil {
		t.Skipf("current user unavailable: %v", err)
	}
	uid, err := strconv.ParseInt(current.Uid, 10, 64)
	if err != nil || current.HomeDir == "" {
		t.Skipf("current user lacks numeric uid or home: %#v", current)
	}

	env, err := userEnvDefaults(&User{UID: &uid})
	if err != nil {
		t.Fatalf("user env defaults: %v", err)
	}
	if env["HOME"] != current.HomeDir {
		t.Fatalf("env = %#v, want current user home", env)
	}
}
