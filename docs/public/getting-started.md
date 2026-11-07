# Getting started

This is the **v0.19 prerelease** manual. The stable installer may install an
older release; select the version recorded in your docs snapshot when you
need these exact commands.

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
choose a particular published version, pass its tag explicitly:

```sh
curl -sSfL https://discobox.ai | sh -s -- --version v0.19.0-rc.3
```

That example names an earlier candidate, not the revision containing this
manual. Use your snapshot's exact tag. The product release containing the
manual is selected after it lands. `discobox-dev` is Homebrew's latest dot
release channel; it does not select an `-rc` tag. Stable and development
clients share local state and cannot run their servers simultaneously.

Discobox supports macOS, Linux and Windows. On Linux, the built-in provider
needs Docker installed and accessible to the server user. First use can
download the server, images and agent bundles, so allow time and network
access. A remote server runs its pools on the server side.

## Take the tour

With a version supporting repository startup scripts, start the guided tour
from your host:

```sh
discobox run discobox-ai/tour
```

Select a default harness if prompted. The menu runs in the Linux box before
that harness starts, with chapters for shells, tools, access requests and
leaving and returning. No agent account is needed for the menu; continuing
into an agent may require its normal sign-in. You can choose the shell harness.
The access chapter uses a disposable example value and can be denied or skipped.

**Continue to my harness** completes the tour and skips it on subsequent
primary launches in this box. **Exit tour** or Ctrl-C leaves a shell without
starting the harness. To replay, open a shell in the tour checkout and run
`.discobox/scripts/pre-harness --replay`; returning from replay starts no
additional harness. Detach with Ctrl-A then d and reconnect from your host
with `discobox attach BOX`. After collecting any work, archive with
`discobox rm BOX`. Replace BOX with its name or identifier.

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
