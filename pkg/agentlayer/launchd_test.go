package agentlayer

import (
	"bytes"
	"encoding/xml"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
)

// plistDict reads a launchd plist's top-level dict as key and the raw XML of
// its value, which is all a test of the agent's daemon needs.
func plistDict(t *testing.T, body string) map[string]string {
	t.Helper()
	var doc struct {
		Dict struct {
			Inner []byte `xml:",innerxml"`
		} `xml:"dict"`
	}
	if err := xml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("the plist is not XML: %v", err)
	}
	dec := xml.NewDecoder(bytes.NewReader(doc.Dict.Inner))
	out := map[string]string{}
	key := ""
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		var raw struct {
			Inner string `xml:",innerxml"`
		}
		if err := dec.DecodeElement(&raw, &start); err != nil {
			t.Fatal(err)
		}
		if start.Name.Local == "key" {
			key = raw.Inner
			continue
		}
		out[key] = start.Name.Local + ":" + strings.TrimSpace(raw.Inner)
	}
	return out
}

func plistStrings(t *testing.T, value string) []string {
	t.Helper()
	var arr struct {
		Strings []string `xml:"string"`
	}
	if err := xml.Unmarshal([]byte("<array>"+strings.TrimPrefix(value, "array:")+"</array>"), &arr); err != nil {
		t.Fatal(err)
	}
	return arr.Strings
}

// servicePreStart is discobox-sandbox-agent.service's ExecStartPre lines and
// its ExecStart, each without the path of the program it runs.
func servicePreStart(t *testing.T) (pre []string, start string) {
	t.Helper()
	unit, err := unitFiles.ReadFile("units/discobox-sandbox-agent.service")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(unit), "\n") {
		if step, ok := strings.CutPrefix(line, "ExecStartPre="); ok {
			pre = append(pre, strings.TrimPrefix(step, "/usr/bin/"))
		}
		if cmd, ok := strings.CutPrefix(line, "ExecStart="); ok {
			start = cmd
		}
	}
	if len(pre) == 0 || start == "" {
		t.Fatalf("the service has no ExecStartPre or ExecStart:\n%s", unit)
	}
	return pre, start
}

// launchdRootDirs are the directories the daemon makes as root: those at the
// root of the agent's state, whose parents are root's. Anything under them is
// the account's once the agent drops, so the agent makes it as the account,
// never root, which would follow a link the account put there.
var launchdRootDirs = []string{"/run/discobox", "/var/lib/discobox", "/workspace"}

// launchdScript is what the daemon runs: the service's removal of the last
// start's provisioning files, the root directories, then the agent as the
// service runs it, dropping to the sandbox's account.
func launchdScript(t *testing.T) string {
	t.Helper()
	pre, start := servicePreStart(t)
	var removes []string
	for _, step := range pre {
		if strings.HasPrefix(step, "rm ") {
			removes = append(removes, step)
		}
	}
	if len(removes) != 1 {
		t.Fatalf("the service's pre-start removes in %d steps, want 1: %q", len(removes), pre)
	}
	return removes[0] + " && install -d -m 0755 " + strings.Join(launchdRootDirs, " ") + " && exec " + start + " --drop-to-account"
}

// Every directory the service's pre-start makes is made on macOS too: by the
// daemon when it is a root directory of the agent's state, otherwise by the
// agent as the account, which makes the terminals' directory the bootstrap
// names (the pool names /run/discobox/agent-terminals).
func TestTheDaemonMakesWhatTheServiceMakes(t *testing.T) {
	pre, _ := servicePreStart(t)
	for _, step := range pre {
		fields := strings.Fields(step)
		if fields[0] != "install" {
			continue
		}
		for _, dir := range fields[1:] {
			if strings.HasPrefix(dir, "-") || dir == "0700" || dir == "0755" {
				continue
			}
			if slices.Contains(launchdRootDirs, dir) || dir == "/run/discobox/agent-terminals" {
				continue
			}
			t.Errorf("the service makes %s, which nothing makes on macOS", dir)
		}
	}
}

// A macOS guest is placed the agent and its one launch daemon, which runs the
// placed agent at boot against the bootstrap's path, dropping to the
// sandbox's account, and nothing of systemd's.
func TestArchiveLaysTheLaunchDaemon(t *testing.T) {
	var buf bytes.Buffer
	if err := Archive(&buf, testAgent("agent-binary"), false, testPins(), LaunchdUnits); err != nil {
		t.Fatal(err)
	}
	_, entries := readArchive(t, buf.Bytes())
	for name := range entries {
		if strings.HasPrefix(name, "etc/systemd/") {
			t.Errorf("placed %s on a launchd guest", name)
		}
	}
	daemon, ok := entries["Library/LaunchDaemons/"+LaunchdAgentLabel+".plist"]
	if !ok {
		t.Fatal("no launch daemon placed")
	}
	if daemon.header.Mode != 0o644 || daemon.header.Uid != 0 || daemon.header.Gid != 0 {
		t.Fatalf("daemon header = mode %o uid %d gid %d; launchd loads only a root-owned file no one else can write", daemon.header.Mode, daemon.header.Uid, daemon.header.Gid)
	}
	dict := plistDict(t, daemon.body)
	if got := dict["Label"]; got != "string:"+LaunchdAgentLabel {
		t.Fatalf("Label = %q, want %q", got, LaunchdAgentLabel)
	}
	args := plistStrings(t, dict["ProgramArguments"])
	if len(args) != 3 || args[0] != "/bin/sh" || args[1] != "-c" {
		t.Fatalf("ProgramArguments = %q, want a /bin/sh -c script", args)
	}
	if want := launchdScript(t); args[2] != want {
		t.Fatalf("the daemon's script is\n  %s\nbut the systemd service runs\n  %s", args[2], want)
	}
	if !strings.HasSuffix(args[2], "exec "+AgentPath+" --config "+sandboxconfig.SandboxConfigDir+"/sandbox.json --drop-to-account") {
		t.Fatalf("the daemon does not exec the placed agent on the bootstrap's path: %s", args[2])
	}
	if got := dict["RunAtLoad"]; got != "true:" {
		t.Fatalf("RunAtLoad = %q: launchd has no path unit, so the agent starts at boot and waits", got)
	}
	if got := dict["KeepAlive"]; !strings.Contains(got, "<key>SuccessfulExit</key>") || !strings.Contains(got, "<false/>") {
		t.Fatalf("KeepAlive = %q, want a restart only after a failure", got)
	}
}
