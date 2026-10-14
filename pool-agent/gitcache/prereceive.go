package gitcache

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	// PreReceiveCommand is the argument that runs this program as a cache's
	// pre-receive hook (RunPreReceive).
	PreReceiveCommand = "git-cache-pre-receive"
	// CallerEnv names, to the hook, the sandbox whose push is being received.
	CallerEnv = "DISCOBOX_GIT_CACHE_CALLER"

	hooksDir = ".hooks"
)

// SandboxReceiveConfig is what a push from a sandbox — one delivering the
// source of another discobox in its project — is received under, on top of
// ReceiveConfig, and the environment its hook reads (ADR 26-10-10-545 §7).
//
// Project members are trusted with everything a cache holds; a sandbox is
// not, and Git alone does not confine it. A cache advertises every other
// namespace's tips to a pusher, and accepts a ref at any object its store
// holds whether or not the push sent it. So the advertisement is cut down to
// the caller's own namespaces and the one it is pushing into, and a hook
// refuses any ref that reaches an object which neither arrived in this push
// nor is reachable from those namespaces.
func (s *Store) SandboxReceiveConfig(path, caller, sandboxID, slug string) (config, env []string, err error) {
	hooks, err := s.hooksPath()
	if err != nil {
		return nil, nil, err
	}
	own, err := ownNamespaces(path, caller)
	if err != nil {
		return nil, nil, err
	}
	config = []string{
		"-c", "core.hooksPath=" + hooks,
		// The pack is kept as it was sent. Unpacked, an object the store
		// already holds is not written again, and the hook could not tell a
		// push that sent it from one that only named it.
		"-c", "receive.unpackLimit=1",
		// Everything hidden, then the namespaces the caller may see: the
		// later entry wins, and a leading ^ matches the ref's full name,
		// which is the only name a ref outside the pushed namespace has.
		"-c", "receive.hideRefs=^refs/",
		"-c", "receive.hideRefs=!^refs/namespaces/" + Namespace(sandboxID, slug) + "/",
	}
	for _, namespace := range own {
		config = append(config, "-c", "receive.hideRefs=!^refs/namespaces/"+namespace+"/")
	}
	return config, []string{CallerEnv + "=" + caller}, nil
}

// MemberReceiveConfig is what a project member's push is received under, on
// top of ReceiveConfig: no hook runs.
func MemberReceiveConfig() []string {
	return []string{"-c", "core.hooksPath=" + os.DevNull}
}

// hooksPath is the directory holding the pre-receive hook, written on each
// call so it names this program as it is installed now.
func (s *Store) hooksPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(s.root, hooksDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(exe, "'", `'\''`) + "' " + PreReceiveCommand + "\n"
	hook := filepath.Join(dir, "pre-receive")
	if current, err := os.ReadFile(hook); err == nil && string(current) == script {
		return dir, nil
	}
	tmp := hook + ".tmp"
	//nolint:gosec // A hook is a program: it has to be executable.
	if err := os.WriteFile(tmp, []byte(script), 0o700); err != nil {
		return "", err
	}
	return dir, os.Rename(tmp, hook)
}

// RunPreReceive is a cache's pre-receive hook for a sandbox's push. Git runs
// it with the pushed objects still in quarantine, which is the one moment
// what arrived can be told from what the cache already held. It reads the ref
// updates from stdin and returns the hook's exit status.
func RunPreReceive(stdin io.Reader, stderr io.Writer) int {
	if err := preReceive(stdin); err != nil {
		fmt.Fprintf(stderr, "discobox: %v\n", err)
		return 1
	}
	return 0
}

func preReceive(stdin io.Reader) error {
	caller := strings.TrimSpace(os.Getenv(CallerEnv))
	quarantine := os.Getenv("GIT_QUARANTINE_PATH")
	if caller == "" || quarantine == "" {
		return errors.New("this push cannot be checked, so it is refused")
	}
	gitDir, err := hookGit(nil, nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	var revisions bytes.Buffer
	scanner := bufio.NewScanner(stdin)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 {
			return errors.New("malformed ref update")
		}
		if strings.Trim(fields[1], "0") == "" {
			continue
		}
		revisions.WriteString(fields[1])
		revisions.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if revisions.Len() == 0 {
		return nil
	}
	// What the caller was told the cache holds, and so what its push leaves
	// out: its own namespaces, and the one it is pushing into. That one is
	// the pushed discobox's own, which a ref there reveals nothing of.
	reach, err := ownNamespaces(strings.TrimSpace(gitDir), caller)
	if err != nil {
		return err
	}
	if target := strings.TrimSpace(os.Getenv("GIT_NAMESPACE")); target != "" && validNamespace(target) {
		reach = append(reach, target)
	}
	if len(reach) > 0 {
		args := []string{"for-each-ref", "--format=%(objectname)"}
		for _, namespace := range reach {
			args = append(args, "refs/namespaces/"+namespace+"/")
		}
		tips, err := hookGit(nil, nil, args...)
		if err != nil {
			return err
		}
		if strings.TrimSpace(tips) != "" {
			revisions.WriteString("--not\n")
			revisions.WriteString(tips)
		}
	}
	// Everything the new refs reach that those namespaces do not.
	needed, err := hookGit(revisions.Bytes(), nil, "rev-list", "--objects", "--stdin")
	if err != nil {
		return err
	}
	var objects bytes.Buffer
	for _, line := range strings.Split(needed, "\n") {
		if object, _, _ := strings.Cut(strings.TrimSpace(line), " "); object != "" {
			objects.WriteString(object)
			objects.WriteByte('\n')
		}
	}
	if objects.Len() == 0 {
		return nil
	}
	// Asked of the quarantine alone, with the cache's own store out of the
	// environment: an object it does not hold did not arrive in this push.
	arrived, err := hookGit(objects.Bytes(), []string{
		"GIT_OBJECT_DIRECTORY=" + quarantine,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=",
	}, "cat-file", "--batch-check=%(objectname)")
	if err != nil {
		return err
	}
	if strings.Contains(arrived, " missing") {
		return errors.New("this push names objects it did not send; a discobox delivers only what it sends and what its own sources hold")
	}
	return nil
}

// hookGit runs git in the environment receive-pack gave the hook, with
// overrides applied on top.
func hookGit(stdin []byte, overrides []string, args ...string) (string, error) {
	// No deadline of its own: receive-pack is waiting on this hook, and ends
	// it with the push.
	//nolint:gosec // The executable is fixed; object ids come from receive-pack and git itself.
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Env = append(os.Environ(), overrides...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
