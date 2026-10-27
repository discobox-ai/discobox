package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0755); err != nil {
		t.Fatal(err)
	}
}
func command(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return string(out)
}

// Exercise the actual publication script, including resuming after an upload
// failure. The gh executable is a recording fake; no network is available to it.
func TestReleasePublicationRetry(t *testing.T) {
	root := t.TempDir()
	script, err := filepath.Abs("release-publish.sh")
	if err != nil {
		t.Fatal(err)
	}
	fixtureFile(t, filepath.Join(root, "artifacts", "binary"), "actual artifact")
	fixtureFile(t, filepath.Join(root, "bin", "gh"), `#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "$STATE/calls"
case "$1 $2" in
 'release view') test -f "$STATE/release"; if [[ "$*" == *--json* ]]; then echo https://example.invalid/release; fi ;;
 'release create') touch "$STATE/release" ;;
 'release upload') if [ -f "$STATE/fail-upload" ]; then rm "$STATE/fail-upload"; exit 1; fi; test -f "$3" || test "$3" = v1.2.3 ;;
 *) exit 97 ;;
esac
`)
	env := []string{"PATH=" + filepath.Join(root, "bin") + ":" + os.Getenv("PATH"), "STATE=" + root, "RELEASE_VERSION=v1.2.3"}
	fixtureFile(t, filepath.Join(root, "fail-upload"), "")
	cmd := exec.Command("bash", script, filepath.Join(root, "artifacts"))
	cmd.Env = append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("failed upload returned success: %s", out)
	}
	command(t, root, env, "bash", script, filepath.Join(root, "artifacts"))
	command(t, root, env, "bash", script, filepath.Join(root, "artifacts"))
	calls, err := os.ReadFile(filepath.Join(root, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "release create") != 1 || strings.Count(string(calls), "release upload") != 3 {
		t.Fatal(string(calls))
	}
	if !strings.Contains(string(calls), "--verify-tag --title v1.2.3 --generate-notes --prerelease") || strings.Count(string(calls), "--clobber") != 3 {
		t.Fatal(string(calls))
	}
}

// Use a real local bare Git repository and a rejecting receive hook to exercise
// push retries and no-op updates. Only GitHub release discovery and formula
// generation are fake: this tests orchestration, not GitHub API compatibility.
func TestBrewRefreshRetriesAndChannels(t *testing.T) {
	root := t.TempDir()
	script, err := filepath.Abs("brew-refresh.sh")
	if err != nil {
		t.Fatal(err)
	}
	tap := filepath.Join(root, "tap.git")
	seed := filepath.Join(root, "seed")
	env := []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(root, "gitconfig"), "GH_REPO=test/product", "TAP_REPOSITORY=test/tap", "STATE=" + root, "PATH=" + filepath.Join(root, "bin") + ":" + os.Getenv("PATH")}
	command(t, root, env, "git", "init", "--bare", "--initial-branch=main", tap)
	command(t, root, env, "git", "init", "--initial-branch=main", seed)
	fixtureFile(t, filepath.Join(seed, "README"), "tap")
	command(t, seed, env, "git", "add", ".")
	command(t, seed, env, "git", "-c", "user.name=test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "initial")
	command(t, seed, env, "git", "push", tap, "main")
	fixtureFile(t, filepath.Join(root, "gitconfig"), "[url \"file://"+tap+"\"]\n\tinsteadOf = https://github.com/test/tap.git\n")
	fixtureFile(t, filepath.Join(tap, "hooks", "pre-receive"), "#!/bin/sh\nif [ ! -f '"+root+"/rejected' ]; then touch '"+root+"/rejected'; exit 1; fi\n")
	fixtureFile(t, filepath.Join(root, "bin", "gh"), `#!/usr/bin/env bash
set -eu
[ "$1 $2" = 'release list' ] || exit 97
printf '%s\n' "$*" >> "$STATE/reads"
if [[ "$*" == *--exclude-pre-releases* ]]; then
  jq '[.[] | select(.isPrerelease == false)][:1]' "$STATE/releases"
else
  jq '.[:30]' "$STATE/releases"
fi | jq -r "${@: -1}"
`)
	fixtureFile(t, filepath.Join(root, "releases"), `[
 {"tagName":"v9.0.0-rc.1","isPrerelease":true},
 {"tagName":"v2.0.0","isPrerelease":true},
 {"tagName":"v1.1.0","isPrerelease":false},
 {"tagName":"v1.0.0","isPrerelease":false}
]`)
	fixtureFile(t, filepath.Join(root, "scripts", "brew-formula.sh"), `#!/bin/sh
set -eu
if [ "$1" = --dev ]; then shift; fi
printf '%s\n' "$1" > "$3"
`)
	out := command(t, root, env, "bash", script)
	if !strings.Contains(out, "retry 1") {
		t.Fatal(out)
	}
	stable := command(t, root, env, "git", "--git-dir="+tap, "show", "main:Formula/discobox.rb")
	dev := command(t, root, env, "git", "--git-dir="+tap, "show", "main:Formula/discobox-dev.rb")
	if strings.TrimSpace(stable) != "v1.1.0" || strings.TrimSpace(dev) != "v2.0.0" {
		t.Fatalf("stable %s dev %s", stable, dev)
	}
	before := command(t, root, env, "git", "--git-dir="+tap, "rev-parse", "main")
	if out = command(t, root, env, "bash", script); !strings.Contains(out, "current") {
		t.Fatal(out)
	}
	if after := command(t, root, env, "git", "--git-dir="+tap, "rev-parse", "main"); before != after {
		t.Fatal("no-op changed tap")
	}
	reads, err := os.ReadFile(filepath.Join(root, "reads"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(reads), "release list --repo test/product") != 6 {
		t.Fatal("retry did not reread state: " + string(reads))
	}
	// A persistent push failure must propagate, without changing the remote.
	releasesPath := filepath.Join(root, "releases")
	releases, err := os.ReadFile(releasesPath)
	if err != nil {
		t.Fatal(err)
	}
	fixtureFile(t, releasesPath, strings.ReplaceAll(string(releases), "v2.0.0", "v3.0.0"))
	fixtureFile(t, filepath.Join(tap, "hooks", "pre-receive"), "#!/bin/sh\nexit 1\n")
	failed := exec.Command("bash", script)
	failed.Dir = root
	failed.Env = append(os.Environ(), env...)
	output, err := failed.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "failed after three") {
		t.Fatalf("persistent push failure was hidden: %v\n%s", err, output)
	}
	if after := command(t, root, env, "git", "--git-dir="+tap, "rev-parse", "main"); before != after {
		t.Fatal("rejected update changed tap")
	}
}

func TestWingetPublicationBranches(t *testing.T) {
	script, err := filepath.Abs("winget-publish.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"new", "existing-pr", "already-published", "bad-scope", "blob-fails"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			fixtureFile(t, filepath.Join(root, "manifests", "Discobox.Discobox.installer.yaml"), "PackageIdentifier: Discobox.Discobox\nPackageVersion: 1.2.3\nInstallerUrl: https://example.invalid/discobox-windows-amd64.zip\n")
			fixtureFile(t, filepath.Join(root, "manifests", "Discobox.Discobox.yaml"), "PackageIdentifier: Discobox.Discobox\n")
			fixtureFile(t, filepath.Join(root, "bin", "gh"), `#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "$STATE/calls"
case "$*" in
 'api -i user') if [ "$MODE" = bad-scope ]; then echo 'X-OAuth-Scopes: read:user'; else echo 'X-OAuth-Scopes: public_repo'; fi ;;
 'api user -q .login') echo test-user ;;
 'repo fork '*) : ;;
 'api repos/microsoft/winget-pkgs/contents/manifests/d/Discobox/Discobox/1.2.3') [ "$MODE" = already-published ] ;;
 'api repos/microsoft/winget-pkgs/contents/manifests/d/Discobox/Discobox') exit 1 ;;
 'api repos/test-user/winget-pkgs/git/ref/heads/master -q .object.sha') echo base ;;
 'api repos/test-user/winget-pkgs/git/commits/base -q .tree.sha') echo base-tree ;;
 'api repos/test-user/winget-pkgs/git/blobs --input - -q .sha') cat >/dev/null; [ "$MODE" != blob-fails ]; echo blob ;;
 'api repos/test-user/winget-pkgs/git/trees --input - -q .sha') cat > "$STATE/tree"; echo new-tree ;;
 'api repos/test-user/winget-pkgs/git/commits --input - -q .sha') cat > "$STATE/commit"; echo new-commit ;;
 'api -X PATCH '*) cat >/dev/null; exit 1 ;;
 'api repos/test-user/winget-pkgs/git/refs --input -') cat > "$STATE/ref" ;;
 'pr list '*) if [ "$MODE" = existing-pr ]; then echo https://example.invalid/pr/1; fi ;;
 'pr create '*) echo https://example.invalid/pr/2 ;;
 *) echo "unexpected gh: $*" >&2; exit 97 ;;
esac
`)
			cmd := exec.Command("bash", script, filepath.Join(root, "manifests"))
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "PATH="+filepath.Join(root, "bin")+":"+os.Getenv("PATH"), "STATE="+root, "MODE="+mode, "RELEASE_VERSION=v1.2.3")
			output, err := cmd.CombinedOutput()
			fail := mode == "bad-scope" || mode == "blob-fails"
			if (err != nil) != fail {
				t.Fatalf("unexpected result %v: %s", err, output)
			}
			data, e := os.ReadFile(filepath.Join(root, "calls"))
			if e != nil {
				t.Fatal(e)
			}
			calls := string(data)
			if mode == "new" && (!strings.Contains(calls, "pr create") || !strings.Contains(calls, "New package: Discobox.Discobox version 1.2.3")) {
				t.Fatal(calls)
			}
			if mode != "new" && strings.Contains(calls, "pr create") {
				t.Fatal("duplicate/failed submission created PR: " + calls)
			}
			if mode == "already-published" && strings.Contains(calls, "repo fork") {
				t.Fatal("published package rewrote fork")
			}
			if mode == "bad-scope" && strings.Contains(calls, "git/blobs") {
				t.Fatal("invalid token attempted write")
			}
			if mode == "blob-fails" && strings.Contains(calls, "git/trees") {
				t.Fatal("blob error did not stop publication")
			}
		})
	}
}
