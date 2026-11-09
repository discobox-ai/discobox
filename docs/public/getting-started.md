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

`discobox-dev` is Homebrew's latest dot release channel; it does not select
an `-rc` tag. Stable and development clients share local state and cannot run
their servers simultaneously.

Discobox supports macOS, Linux and Windows. On Linux, the built-in provider
needs Docker installed and accessible to the server user. First use can
download the server, images and agent bundles, so allow time and network
access. A remote server runs its pools on the server side.

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
