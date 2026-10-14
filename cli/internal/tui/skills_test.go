package tui

import (
	"strings"
	"testing"
	"time"
)

// The skills screen lists what every new discobox gets, wherever each is kept,
// and files a value by what it is (ADR 26-10-14-894 §3).
func skillsFixture(t *testing.T) (*Model, *fakeSource) {
	t.Helper()
	ds := newFakeSource(testSandboxes()...)
	ds.skills = []SkillsEntry{
		{Project: true, ID: "skills_1", Value: "https://github.com/acme/tools//agents/skills", Added: time.Now().Add(-time.Hour)},
		{Value: "/home/me/team-skills"},
	}
	m := newTestModel(t, ds)
	send(t, m, keyPress(skillsKey))
	if !m.skillsOpen {
		t.Fatal("the skills screen did not open")
	}
	return m, ds
}

func TestTheSkillsScreenListsWhereEachIsKept(t *testing.T) {
	t.Parallel()
	m, _ := skillsFixture(t)
	body := strings.Join(frame(m), "\n")
	for _, want := range []string{"Skills", "1 project", "1 client.yaml", "https://github.com/acme/tools//agents/skills", "/home/me/team-skills"} {
		if !strings.Contains(body, want) {
			t.Fatalf("frame does not carry %q:\n%s", want, body)
		}
	}
	// The screens are peers: opening one puts the others away.
	send(t, m, keyPress(secretsKey))
	if m.skillsOpen || !m.secretsOpen {
		t.Fatalf("skills %v, secrets %v: want the secrets screen alone", m.skillsOpen, m.secretsOpen)
	}
	send(t, m, keyPress(skillsKey))
	if !m.skillsOpen || m.secretsOpen {
		t.Fatalf("skills %v, secrets %v: want the skills screen alone", m.skillsOpen, m.secretsOpen)
	}
	send(t, m, keyPress(skillsKey))
	if m.skillsOpen {
		t.Fatal("the key did not close the screen it opened")
	}
}

func TestTheSkillsScreenAddsAndRemoves(t *testing.T) {
	t.Parallel()
	m, ds := skillsFixture(t)

	send(t, m, keyPress("n"))
	if m.dialog == nil || m.dialog.kind != dlgInput {
		t.Fatalf("n did not ask for a value: %s", describe(m.dialog))
	}
	drain(t, m, m.dialog.action("https://github.com/acme/shared"), 0)
	if m.dialog != nil {
		t.Fatalf("dialog = %s, want it closed after the add", describe(m.dialog))
	}
	if len(m.skills.all) != 3 || !m.skills.all[2].Project || m.skills.all[2].Value != "https://github.com/acme/shared" {
		t.Fatalf("listing = %+v, want the new project entry listed", m.skills.all)
	}

	m.skills.moveTo(1)
	send(t, m, keyPress("d"))
	if m.dialog == nil || m.dialog.kind != dlgConfirm || !m.dialog.defaultNo {
		t.Fatalf("d did not ask before removing: %s", describe(m.dialog))
	}
	if !strings.Contains(dialogText(m), "client.yaml") {
		t.Fatalf("confirm = %q, want it to say where the entry is kept", dialogText(m))
	}
	drain(t, m, m.dialog.action("yes"), 0)
	for _, entry := range ds.skills {
		if entry.Value == "/home/me/team-skills" {
			t.Fatalf("skills = %+v, want the directory removed", ds.skills)
		}
	}
	if len(m.skills.all) != 2 {
		t.Fatalf("listing = %+v, want it re-read after the remove", m.skills.all)
	}
}
