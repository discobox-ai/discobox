# Getting started

This manual describes the release the documentation site shows it for. The
installers below install the latest release on their channel; to run exactly
these commands, install that release by its tag.

## Install

On macOS or Linux, install the stable channel with Homebrew:

```sh
brew install discobox-ai/tap/discobox
```

Or use the stable installer on macOS or Linux:

```sh
curl -sSfL https://discobox.ai | sh
```

On Windows, run this in PowerShell:

```powershell
irm https://discobox.ai/install.ps1 | iex
```

The edge installer includes release candidates and other prereleases. To
choose a particular published version, pass its tag explicitly, replacing
`TAG` with it (for example the release this manual describes):

```sh
curl -sSfL https://discobox.ai | sh -s -- --version TAG
```

```powershell
& ([scriptblock]::Create((irm https://discobox.ai/install.ps1))) -Version TAG
```

`discobox-beta` is Homebrew's beta channel, the newest dot release; it does not
select an `-rc` tag. To try what is on the main branch, install its newest
build with `curl -sSfL https://main.discobox.ai | sh`; main builds are kept for
fourteen days. Every channel shares local state, so two installs cannot run
their servers simultaneously.

Discobox supports macOS, Linux and Windows. On Linux, the built-in provider
needs Docker installed and accessible to the server user. First use can
download the server, images and agent bundles, so allow time and network
access. A remote server runs its pools on the server side.

Discobox stores generated SSH aliases in its local state directory. Adding
Includes to your SSH config is optional. The first interactive SSH config sync
asks for permission with Yes selected. Without a terminal, Discobox makes no
SSH config edits unless you already opted in. With the Includes enabled, OpenSSH can
connect through the Discobox server with `ssh BOX_ID`, and SSH-based editors can
use the same configuration. Under WSL, the setting covers both Linux and Windows
OpenSSH. Change your choice with `discobox admin ssh-config enable` or
`discobox admin ssh-config disable`. Uninstalling with `discobox admin uninstall`
removes the managed Includes.

## Verify a download

The installers check every binary they download against digests written into
the installer itself. To check a download without trusting `discobox.ai` or the
mirror it came from, verify its build provenance with the GitHub CLI. Every
file a release or main build publishes is attested by the GitHub Actions
workflow that built it, in `discobox-ai/discobox`, whichever repository the
file was downloaded from.

Name both the workflow that signed the file and the ref it ran from. `--repo`
alone accepts an attestation made by any workflow in the repository, and
`--signer-workflow` alone accepts that workflow run from any branch.

| What you installed | `--signer-workflow` | `--source-ref` |
| --- | --- | --- |
| A release (stable, beta, edge, or a version such as `v0.20.0`) | `discobox-ai/discobox/.github/workflows/release.yml` | `refs/tags/TAG` |
| A main build (the main channel, or a version ending `-main.N.gSHA`) | `discobox-ai/discobox/.github/workflows/main-build.yml` | `refs/heads/main` |

Replace `TAG` with the tag of the release you meant to install, taken from
GitHub rather than from the file you are checking, which could name any tag.
The newest stable release's is
`gh release view --repo discobox-ai/discobox --json tagName --jq .tagName`. A
release republished by hand is signed from `main` rather than its tag: if its
tag finds no attestation, check it with `--source-ref refs/heads/main`.

To check a binary you already have, such as one installed by Homebrew or an
installer:

```sh
gh attestation verify "$(command -v discobox)" \
  --repo discobox-ai/discobox \
  --signer-workflow discobox-ai/discobox/.github/workflows/release.yml \
  --source-ref refs/tags/TAG
```

```powershell
gh attestation verify (Get-Command discobox).Source `
  --repo discobox-ai/discobox `
  --signer-workflow discobox-ai/discobox/.github/workflows/release.yml `
  --source-ref refs/tags/TAG
```

To check the installer before it runs, save it to a file, verify it, then run
it. A verified installer checks each binary it installs against the digests it
carries, so those binaries are covered too:

```sh
curl -sSfL https://discobox.ai -o install.sh
gh attestation verify install.sh \
  --repo discobox-ai/discobox \
  --signer-workflow discobox-ai/discobox/.github/workflows/release.yml \
  --source-ref refs/tags/TAG
sh install.sh
```

```powershell
Invoke-WebRequest https://discobox.ai/install.ps1 -OutFile install.ps1
gh attestation verify install.ps1 `
  --repo discobox-ai/discobox `
  --signer-workflow discobox-ai/discobox/.github/workflows/release.yml `
  --source-ref refs/tags/TAG
powershell -ExecutionPolicy Bypass -File .\install.ps1
```

Run a verified installer with no arguments, and with `DISCOBOX_VERSION` and
`DISCOBOX_CHANNEL` unset. Given a version or a channel, it downloads and runs
another release's installer, and nothing checks that one. To verify a
particular release, download `install.sh` or `install.ps1` from that release's
assets on GitHub, verify it as above, and run it with no arguments.

A passing check says which workflow, ref and commit built the file. It does not
say that commit was reviewed or is safe to run: a release's workflow runs from
whatever commit its tag points at, so the check is only as strong as control
over who can push tags to `discobox-ai/discobox`.

Releases published before attestations were added, `v0.20.0-rc.1` and earlier,
and main builds from before then, have none: `gh attestation verify` finds no
attestation for them.

## Take the tour

Start the guided tour from your host:

```sh
discobox run discobox-ai/tour
```

Select a default harness if prompted. The tour opens full screen in the box
before that harness starts. Use the mouse or the keyboard and take the chapters
in any order. It needs no agent account; the shell harness works too.

**Continue to my harness** starts the harness and skips the tour on later
starts of this box. **Exit tour** leaves a shell without starting it. Replay
the tour from a shell in the box with `.discobox/scripts/pre-harness --replay`.
Detach with Ctrl-A then d, and come back from your host with
`discobox attach BOX`. When you are done, archive the box with
`discobox rm BOX`. Replace BOX with its name or ID.

The tour is ordinary source code running inside the sandbox. See
[Repository startup scripts](repository-scripts.md) for the execution contract.

## Configure and run

Open a terminal in a committed Git repository:

```sh
cd ~/src/my-project
discobox configure
```

Choose a harness with the arrow keys and press **s** to make it the default.
A harness is the agent program and its saved configuration. Sign in and change
its settings inside a running box, as you would on your own machine. Then use
the console list's **H** action to review and save those changes for future
boxes. Credentials may need human approval;
[Concepts](concepts.md#credentials) explains how access works.

Create a box and open its workspace:

```sh
discobox run . 'fix the failing tests'
```

`.` is the source; the words after it are the agent's prompt. Each box gets
its own checkout at your selected commit. Uncommitted files stay on your
machine unless you explicitly add `--include-dirty`.

A run names three things: the harness, the source and the task. Choose a
harness for one run with `-H`, and give a repository as the source instead of
a directory:

```sh
discobox run -H codex OWNER/REPO -p 'fix the flaky retry test'
```

Without `-d`, your terminal becomes the box's window: the harness's own
interface, with shells and services beside it. Press **Ctrl-A**, then **d**,
to detach; the box keeps running.

For an empty box, use `discobox run --no-source`. To start a task without
opening its workspace, add `-d`; it prints the box identifier:

```sh
discobox run -d . 'investigate the slow build'
discobox ls
```

Open the console with `discobox console`, or return directly with
`discobox attach BOX`, replacing `BOX` with its name or identifier. Bare
`discobox` prints help. A name shared by multiple boxes is refused: use an ID.

## Work and collect the result

In the workspace, the agent terminal is on the left. Press **Ctrl-A**, then
**s** to open a shell. Use it to run your project's checks. **Ctrl-A q**
leaves the console and keeps sessions running; it does not stop the box.
[Console guide](console.md) covers the other keys.

Ask the agent to commit its changes and push a branch to a Git host for
review. Grant only the access that task needs. Review and merge through your
normal Git workflow. The box's source origin is read-only; committing in a
box does not change your original checkout. A commit held only in a box is
not a remote backup.

Use `discobox stop BOX` when you want to stop it, and `discobox start BOX` to
return. `discobox rm BOX` archives it and removes it from the normal list;
purge destroys its stored data. Collect your work before archiving and do
not rely on an archived box as your only copy.

## If something does not work

Run `discobox --help` or append `--help` to any command. `discobox version`
prints the client version and asks the server for its version without
starting a server. `discobox admin server status` and
`discobox admin server logs` help diagnose the local server.

If a key is intercepted by your outer terminal, check its keyboard settings.
You can change the console prefix with `DISCOBOX_LEADER` or
`discobox console --leader ctrl+b`. A remote endpoint can be selected with
`--server SERVER`; connection and provider failures concern that server's
host, which may be different from your machine.
