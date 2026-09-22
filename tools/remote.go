package tools

import (
	"errors"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/discobox-ai/discobox/sandboxconfig"
)

// Remote is the discobox as a host tool is handed it: the ssh_config host that
// reaches it, and the working tree to open there (ADR 0125 §4).
type Remote struct {
	SandboxID string
	Host      string
	// Workdir is the working tree in the discobox, empty when none is known.
	Workdir string
}

// workdir is the directory a tool opens: the working tree, or the sandbox's
// working root when there is none — the directory boot seeds and every shell
// in the box starts in, so a window opens where the box's own work happens
// (ADR 0141).
func (r Remote) workdir() string {
	if r.Workdir == "" {
		return sandboxconfig.DefaultWorkingRoot
	}
	return r.Workdir
}

// sshURL is the working tree as an `ssh://` URL whose authority is the
// ssh_config host, so whatever takes it hands the whole connection back to the
// ssh that already knows how to reach the discobox. Built with net/url so a
// workdir with a space or a percent sign in it arrives encoded.
func (r Remote) sshURL() string {
	return (&url.URL{Scheme: "ssh", Host: r.Host, Path: r.workdir()}).String()
}

// GitURL is sshURL for git, and empty when there is no working tree: a URL to
// the working root of a box with no source is a clone of nothing.
func (r Remote) GitURL() string {
	if r.Workdir == "" {
		return ""
	}
	return r.sshURL()
}

// errNoGitURL is what a tool that asked for {git.url} is told when there is
// none to give it.
var errNoGitURL = errors.New("the discobox has no working tree to give a git URL for")

// Expand resolves the placeholders in a host tool's args.
func (r Remote) Expand(args []string) ([]string, error) {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if strings.Contains(arg, "{git.url}") && r.GitURL() == "" {
			return nil, errNoGitURL
		}
		out = append(out, strings.NewReplacer(
			"{ssh.host}", r.Host,
			"{ssh.url}", r.sshURL(),
			"{git.url}", r.GitURL(),
			"{workdir.urlpath}", (&url.URL{Path: r.workdir()}).EscapedPath(),
			"{workdir}", r.workdir(),
			"{discobox.id}", r.SandboxID,
		).Replace(arg))
	}
	return out, nil
}

// Environ is the discobox as environment, for a host script, which has no args
// of its own to have placeholders in.
func (r Remote) Environ() []string {
	return []string{
		"DISCOBOX_ID=" + r.SandboxID,
		"DISCOBOX_SSH_HOST=" + r.Host,
		"DISCOBOX_SSH_URL=" + r.sshURL(),
		"DISCOBOX_GIT_URL=" + r.GitURL(),
		"DISCOBOX_WORKDIR=" + r.workdir(),
	}
}

// ResolveProgram finds the program a `.yaml` host tool runs: the one named —
// by the caller, or by the tool's ProgramEnv — or else the first of its
// programs on PATH.
func (d Definition) ResolveProgram(named string) (string, error) {
	if strings.TrimSpace(named) == "" && d.ProgramEnv != "" {
		named = strings.TrimSpace(os.Getenv(d.ProgramEnv))
	}
	if named != "" {
		found, err := exec.LookPath(named)
		if err != nil {
			return "", &ProgramError{Tool: d, Named: named, Err: err}
		}
		return found, nil
	}
	for _, candidate := range d.Program {
		if found, err := exec.LookPath(candidate); err == nil {
			return found, nil
		}
	}
	return "", &ProgramError{Tool: d}
}

// ProgramError is a host tool whose program is not on this machine.
type ProgramError struct {
	Tool  Definition
	Named string
	Err   error
}

func (e *ProgramError) Error() string {
	if e.Named != "" {
		return e.Named + " is not installed, or not on PATH: " + e.Err.Error()
	}
	// The programs it looked for lead the sentence. This error is the whole of
	// what a window says when the tool cannot run, and a status line truncates
	// from the middle — the names are the part worth reading, because a
	// packager renaming the binary is the ordinary way to get here.
	msg := "looked for " + strings.Join(e.Tool.Program, ", ") + " on PATH and found none; install " + e.Tool.Label() + ", or name the program with --program"
	if e.Tool.ProgramEnv != "" {
		msg += " or $" + e.Tool.ProgramEnv
	}
	return msg
}

func (e *ProgramError) Unwrap() error { return e.Err }
