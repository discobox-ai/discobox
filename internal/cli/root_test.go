package cli

import (
	"bytes"
	"errors"
	"testing"
)

func TestRootCommandHelp(t *testing.T) {
	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute help: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("sandbox")) || !bytes.Contains(out.Bytes(), []byte("events")) {
		t.Fatalf("help output = %q, want sandbox and events commands", out.String())
	}
}

func TestProjectUUIDDefaultsToLocal(t *testing.T) {
	app := &App{projectID: defaultProjectAlias}

	projectID, err := app.projectUUID()
	if err != nil {
		t.Fatalf("projectUUID: %v", err)
	}
	if projectID.String() != "00000000-0000-0000-0000-000000000002" {
		t.Fatalf("projectID = %s", projectID)
	}
}

func TestRootCommandRejectsInvalidOutputFormat(t *testing.T) {
	cmd := NewRootCommand()
	cmd.SetArgs([]string{"--output", "yaml", "sandbox", "list"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("execute error = nil, want invalid output error")
	}
}

func TestProjectUUIDRejectsEmptyExplicitProject(t *testing.T) {
	app := &App{projectID: " "}
	if _, err := app.projectUUID(); !errors.Is(err, errMissingProject) {
		t.Fatalf("projectUUID error = %v, want errMissingProject", err)
	}
}
