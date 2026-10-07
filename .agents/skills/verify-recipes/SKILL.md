---
name: verify-recipes
description: This repository's recipes for verifying a change at runtime inside a discobox — a CLI or console (TUI) change against the running `task dev` loop (opening the console in an isolated tmux, signalling it, forcing a real out-of-memory kill), the nested-Docker runc wrapper (runcca / sandbox-agent/cmd/discobox-runc) via docker run and kind, and a change to a skill (`.discobox/skills`, `.agents/skills`) via a headless agent in an isolated HOME. Inside a discobox, the generic `verify` skill (from `.discobox/skills/verify`) reads it first and records what it learns here; outside one, Claude Code's built-in `/verify` does not know this name. Use when verifying a change to the discobox console, its terminal guard, anything reached from bare `./build/discobox`, the runc wrapper, or a skill.
---

# Verifying the console

Read `.agents/skills/test-fix/driving-task-dev.md` first: the dev loop is
already running, and every CLI call needs `--server http://127.0.0.1:8080`.
Confirm `build/discobox` is newer than your last edit before driving it.

## Open the console

Bare `discobox` opens the console without creating a box; it is the cheapest
way to reach `runConsole` (and the terminal guard it starts).

```bash
tmux -L verify new-session -d -s v -x 150 -y 40 -c "$PWD" \
  "env -u DISCOBOX_SERVER PS1='\$ ' bash --norc --noprofile"
tmux -L verify send-keys -t v './build/discobox --server http://127.0.0.1:8080; echo "exit=$?"' Enter
tmux -L verify capture-pane -p -t v
```

The console is `pgrep -f '^./build/discobox --server'`; its guard is
`pgrep -f 'admin console-guard'` (`pgrep -f` also matches your own tool shell,
so filter it out). Ctrl-C at the welcome screen quits it normally.

## Gotchas

- `sudo` works without a password in the box.
- A real kernel OOM kill: `sudo mkdir /sys/fs/cgroup/<name>`, move the pane's
  shell (`tmux list-panes -F '#{pane_pid}'`) into its `cgroup.procs`, open the
  console, then set `memory.swap.max` to 0 and lower `memory.max` to a few MB.
  Without the swap limit the kernel swaps instead of killing. `rmdir` the
  cgroup after the tmux server is gone.
- bpftrace is not installed: `nix build --no-link --print-out-paths
  nixpkgs#bpftrace`. Inside a box, `args->pid` on a tracepoint is the host's
  pid, not the box's.
- `sudo pkill -f <pattern>` kills your own tool shell when the pattern is in
  its command line; kill by pid instead.
- Kill the tmux server (`tmux -L verify kill-server`) when done.

# Verifying the runc wrapper (runcca, sandbox-agent/cmd/discobox-runc)

The installed wrapper is `/opt/discobox/bin/runc`; dockerd and containerd call
it for every container. Swap a build in, drive `docker run`, then put the
original back.

```bash
S=<scratchpad>
(cd sandbox-agent && CGO_ENABLED=0 go build -o $S/discobox-runc ./cmd/discobox-runc)
cp /opt/discobox/bin/runc $S/runc.orig
sudo install -m 0755 $S/discobox-runc /opt/discobox/bin/runc
docker run --rm -e HTTP_PROXY busybox:1.36 sh -c 'env | grep -i proxy | sort'
sudo install -m 0755 $S/runc.orig /opt/discobox/bin/runc   # always restore
```

- Run the same commands against the original wrapper first, as a baseline.
- Inputs: `/etc/discobox/proxy/bridge.json` (root-only, loopback forwarder)
  and `/run/discobox/proxy/nested-forwarder.json` (bridge forwarder). To get
  the "forwarder unpublished" state, `sudo mv` the second one aside and restore it.
- `--network host` and `--network container:X` exercise the namespace cases.
- End to end with kind: `GOBIN=$S/bin go install sigs.k8s.io/kind@v0.30.0`,
  `kind create cluster --name <n> --wait 0`, then
  `docker exec <n>-control-plane crictl pull docker.io/library/alpine:3.20`.
  kind forwards the caller's proxy env into the node, so its containerd's env
  (`/proc/$(pidof containerd)/environ`) shows what the wrapper left there.
  Delete the cluster afterwards.

# Verifying a skill change (`.discobox/skills`, `.agents/skills`)

The surface is an agent loading the skill. Install it the way
`sandbox-agent/terminal/skills.go` does — the image's skills, then
`.discobox/skills`, into both `.claude/skills` and `.agents/skills` of an
isolated `HOME` — and run a headless agent in a throwaway repo built to reach
the changed instruction. A changed `.agents/skills/<name>` is a project skill:
the harness reads it from the repo it runs in, so copy it into the throwaway
repo's `.agents/skills/` and link `.claude/skills -> ../.agents/skills` there,
as this repository does: Claude Code reads only a repo's `.claude/skills`
(a run with the skill in `.agents/skills` alone did not list it).

```bash
S=<scratchpad>; H=$S/home; mkdir -p $H/.claude
cp ~/.claude/.credentials.json ~/.claude/settings.json $H/.claude/; cp ~/.claude.json $H/
for d in .claude/skills .agents/skills; do mkdir -p $H/$d
  cp -r /usr/local/share/discobox/skills/. .discobox/skills/. $H/$d/; done
# in a throwaway git repo holding the scenario:
env -u CLAUDECODE -u CLAUDE_CODE_ENTRYPOINT -u CLAUDE_CODE_SESSION_ID -u CLAUDE_PID \
  -u CLAUDE_CODE_MESSAGING_SOCKET -u CLAUDE_CODE_MESSAGING_TOKEN -u CLAUDE_CODE_CHILD_SESSION \
  -u CLAUDE_CODE_SESSION_ATTENDED -u CLAUDE_CODE_EXECPATH -u CLAUDE_EFFORT \
  HOME=$H claude -p '/<skill>' --model sonnet --dangerously-skip-permissions \
  --output-format stream-json --verbose > $S/run.jsonl
```

- The box's credential file is a sentinel the proxy swaps, so copies of it
  authenticate; the unset `CLAUDE_CODE_*` variables keep the run from
  attaching to your session.
- Prove which copy loaded by grepping `run.jsonl` for a phrase only the new
  text has. A personal `~/.claude/skills/<name>` outranks Claude Code's
  built-in of the same name.
- Read what it did from the `tool_use` entries
  (`jq 'select(.type=="assistant") | .message.content[]'`) and the repo's
  `git status`, not only its final report.
- Baseline against the original (the built-in, or the skill at `HEAD`) on an
  identical copy of the repo before calling a behavior a regression.
