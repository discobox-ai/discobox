package cli

import "testing"

func TestCreateSandboxExecBodyParsesUserObject(t *testing.T) {
	body, err := createSandboxExecBody(sandboxExecCreateOptions{
		user: "darren:1001",
	}, []string{"id"})
	if err != nil {
		t.Fatalf("create body: %v", err)
	}
	user, ok := body.User.Get()
	if !ok {
		t.Fatal("user was not set")
	}
	if user.Name.Or("") != "darren" || user.Gid.Or(0) != 1001 || user.UID.Set {
		t.Fatalf("user = %#v, want name darren and gid 1001", user)
	}
}

func TestCreateSandboxExecBodyParsesNumericUserObject(t *testing.T) {
	body, err := createSandboxExecBody(sandboxExecCreateOptions{
		user: "1000:1001",
	}, []string{"id"})
	if err != nil {
		t.Fatalf("create body: %v", err)
	}
	user, ok := body.User.Get()
	if !ok {
		t.Fatal("user was not set")
	}
	if user.UID.Or(0) != 1000 || user.Gid.Or(0) != 1001 || user.Name.Set {
		t.Fatalf("user = %#v, want uid 1000 and gid 1001", user)
	}
}

func TestCreateSandboxExecBodyRequestsShellWithoutNamingIt(t *testing.T) {
	body, err := createSandboxExecBody(sandboxExecCreateOptions{shell: true}, nil)
	if err != nil {
		t.Fatalf("create body: %v", err)
	}
	if !body.Shell.Or(false) {
		t.Fatal("shell was not requested")
	}
	// The sandbox resolves the shell, so the client must not send a command.
	if len(body.Command) != 0 {
		t.Fatalf("command = %v, want none", body.Command)
	}

	if _, err := createSandboxExecBody(sandboxExecCreateOptions{shell: true}, []string{"ls"}); err == nil {
		t.Fatal("a command combined with --shell was accepted")
	}
}

func TestCreateSandboxExecBodyUIDGIDFlagsPopulateUserObject(t *testing.T) {
	body, err := createSandboxExecBody(sandboxExecCreateOptions{
		uid: "2000",
		gid: "2001",
	}, []string{"id"})
	if err != nil {
		t.Fatalf("create body: %v", err)
	}
	user, ok := body.User.Get()
	if !ok {
		t.Fatal("user was not set")
	}
	if user.UID.Or(0) != 2000 || user.Gid.Or(0) != 2001 {
		t.Fatalf("user = %#v, want uid 2000 and gid 2001", user)
	}
}
