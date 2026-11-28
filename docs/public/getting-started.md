# Getting started

This manual describes the release the documentation site shows it for. The
installers below install the latest release on their channel. To run exactly
these commands, install the manual's release by its tag.

## Install

Discobox runs on Linux, Windows 11, and macOS on Apple Silicon. Windows 10 and
Intel Macs are not supported, and the installers refuse them.

On macOS or Linux, install the stable channel with Homebrew:

```sh
brew install discobox-ai/tap/discobox
```

Or use the stable installer on macOS or Linux:

```sh
curl -sSfL https://discobox.ai | sh
```

On Windows 11, run this in PowerShell:

```powershell
irm https://discobox.ai/install.ps1 | iex
```

Some network resolvers block newly registered domains. If `discobox.ai` does
not resolve on your network, use Homebrew, which downloads from GitHub. Or
fetch the same stable installer from the GitHub release:

```sh
curl -sSfL https://github.com/discobox-ai/discobox/releases/latest/download/install.sh | sh
```

```powershell
irm https://github.com/discobox-ai/discobox/releases/latest/download/install.ps1 | iex
```

When the Discobox download mirror is unreachable, both installers fall back
to GitHub. So does the server download the CLI makes on first use.

The edge installer includes release candidates and other prereleases. To
install one published version, pass its tag in place of `TAG`, such as the
tag of the release this manual describes:

```sh
curl -sSfL https://discobox.ai | sh -s -- --version TAG
```

```powershell
& ([scriptblock]::Create((irm https://discobox.ai/install.ps1))) -Version TAG
```

`discobox-beta` is Homebrew's beta channel. It installs the newest dot
release, never an `-rc` tag. To try the main branch, install its newest build
with `curl -sSfL https://main.discobox.ai | sh`. Main builds are kept for
fourteen days. All channels share one local state, so two installs cannot run
their servers at the same time.

On Linux, the built-in provider needs Docker, installed and usable by the
user the server runs as. On first use Discobox may download the server, images
and agent bundles, which takes time and needs network access. A remote server
runs its pools on the server side.

Discobox writes an SSH alias for each box to its local state directory. It
can also add Include lines for them to your SSH config, if you agree. The
first time Discobox syncs the SSH config at a terminal, it asks, with Yes
selected. Without a terminal, it edits your SSH config only if you already
agreed. With the Includes in place, `ssh BOX_ID` connects through the Discobox
server, and editors that connect over SSH use the same configuration. Under
WSL, your choice covers both the Linux and the Windows OpenSSH. Change it with
`discobox admin ssh-config enable` or `discobox admin ssh-config disable`.
`discobox admin uninstall` removes the Includes Discobox manages.

## Take the tour

Start the guided tour from your host:

```sh
discobox run discobox-ai/tour
```

If asked, select a default harness. The tour opens full screen in the box,
before that harness starts. Use the mouse or the keyboard, and take the
chapters in any order. It needs no agent account, and the shell harness works
too.

**Continue to my harness** starts the harness and skips the tour on later
starts of this box. **Exit tour** leaves you in a shell and does not start
the harness. Replay
the tour from a shell in the box with `.discobox/scripts/pre-harness --replay`.
Detach with Ctrl-A then d, and come back from your host with
`discobox attach BOX`. When you are done, archive the box with
`discobox rm BOX`. Replace BOX with its name or ID.

The tour is ordinary repository code that runs inside the sandbox.
[Repository startup scripts](repository-scripts.md) says how and when that code
runs.

## Configure and run

Open a terminal in a committed Git repository:

```sh
cd ~/src/my-project
discobox configure
```

A harness is an agent program with its saved configuration. Choose one with
the arrow keys and press **s** to make it the default. Sign in and change its
settings inside a running box, as you would on your own machine. Then use the
console list's **H** action to review those changes and save them for future
boxes. Credentials may need a person's approval.
[Concepts](concepts.md#credentials) explains how access works.

Create a box and open its workspace:

```sh
discobox run . 'fix the failing tests'
```

`.` is the source, and the words after it are the agent's prompt. Each box
gets its own checkout at the commit you select. Uncommitted files stay on your
machine unless you add `--include-dirty`.

A run names three things: the harness, the source and the task. Choose a
harness for one run with `-H`, and give a repository as the source instead of
a directory:

```sh
discobox run -H codex OWNER/REPO -p 'fix the flaky retry test'
```

Without `-d`, your terminal becomes the box's window. It shows the harness's
own interface, with shells and services beside it. Press **Ctrl-A**, then
**d**, to detach. The box keeps running.

For an empty box, use `discobox run --no-source`. To start a task without
opening its workspace, add `-d`. Run then prints the box's ID:

```sh
discobox run -d . 'investigate the slow build'
discobox ls
```

To come back, open the console with `discobox console`, or attach to the box
with `discobox attach BOX`. Replace `BOX` with the box's name or ID. When
several boxes share a name, Discobox refuses the name, so use the ID.
`discobox` with no command prints help.

## Work and collect the result

In the workspace, the agent terminal is on the left. Press **Ctrl-A**, then
**s**, to open a shell. Use it to run your project's checks. **Ctrl-A**, then
**q**, leaves the console. Sessions keep running, and the box does not stop.
The [Console guide](console.md) covers the other keys.

Ask the agent to commit its changes and push a branch to a Git host for
review. Grant only the access that task needs. Review and merge through your
normal Git workflow. The box's source origin is read-only, so a commit in the
box does not change your original checkout. A commit that exists only in a
box has no remote backup.

`discobox stop BOX` stops a box, and `discobox start BOX` starts it again.
`discobox rm BOX` archives it and removes it from the normal list. Purging
destroys its stored data. Collect your work before you archive a box, and do
not keep your only copy in an archived box. [Disk use](concepts.md#disk-use)
says how much space boxes take and how to get it back.

## If something does not work

Run `discobox --help` or append `--help` to any command. `discobox version`
prints the client version and asks the server for its version. It does not
start a server. `discobox admin server status` and
`discobox admin server logs` show the local server's state and logs.

If your outer terminal intercepts a key, check its keyboard settings. You can
change the console's leader key with `DISCOBOX_LEADER` or
`discobox console --leader ctrl+b`. `--server SERVER` selects a remote server.
Connection and provider failures then concern that server's host, which may
not be your machine.
