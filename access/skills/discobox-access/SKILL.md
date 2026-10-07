---
name: discobox-access
description: Ask a human for a credential this sandbox was not given, and run one command with it; or ask for a host whose certificate is refused to be trusted. Use when a command fails with 401/403, when a CLI says it is not logged in or has no token (gh, npm, docker, curl against a private API), when a request answers 502 with an X-Discobox-Untrusted-Host header (a Kubernetes API server, an internal service with its own CA), or to check which credentials you may already use.
allowed-tools: Bash
---

# Asking for a credential

You are working inside a discobox — a sandbox that deliberately holds no
long-lived secrets. When a command needs a credential you do not have, you do
not have to stop and ask in chat: `discobox-access` asks a person, they answer
in their discobox window, and you carry on.

Every command below is safe to run and none of them prints a secret.

## Stay with approval requests until they resolve

For credentials and host trust, use `wait: true` or `--wait`. If your execution
tool returns a running session or job ID, keep waiting on **that same
execution**. A tool yield is not a timeout or a completed request. In Codex,
use `write_stdin` for an `exec_command` session; if `functions.exec` itself
yields a cell ID, use `functions.wait` for that cell.

While approval is pending, provide occasional progress updates and continue
independent work when useful. Do not end your turn merely to announce that
access was requested, and do not ask the user to say "continue". After approval,
automatically resume the authorized task. Stop waiting only when the request
resolves, the command reports an actual timeout or failure, or the user cancels.
Elapsed time is never approval.

If the running execution is lost, resume waiting on the existing request ID;
do not create a replacement request:

```bash
discobox-access wait --json request sreq_7f3a2b
discobox-access wait --json trust treq_7f3a2b
```

Flags go before the request kind and ID. `--timeout 1h` controls the wait
(default one hour). A timeout stops waiting; it does not cancel the request or
deny it. A denial is final: report it rather than requesting the same access
again. Pending notices appear on stderr, including in JSON mode; stdout holds
the result.

## 1. Check what you already have

```bash
discobox-access list
```

Each credential lists the **use IDs** approved for it:

```
github (GH_TOKEN → api.github.com)
  use_7f3a2b  Open a pull request against the current repo
```

If the command you want to run is one of those uses, skip to step 3.

If a command *already works*, the credential is already in your environment —
nothing here is needed. Ask only when something actually failed for want of one.

## 2. Ask for one

```bash
discobox-access request --json <<'EOF'
{
  "name": "github",
  "envVar": "GH_TOKEN",
  "hosts": ["api.github.com"],
  "justification": "the task asks me to open a pull request with the review fixes",
  "uses": [{"description": "Open a pull request against the current repo"}],
  "grantTTLSeconds": 3600,
  "wait": true,
  "timeoutSeconds": 3600
}
EOF
```

- `name` — what the credential is called, in ordinary words (`github`, `npm`).
- `envVar` — the variable the command expects it in. Get this right: it is the
  variable your command will actually read.
- `hosts` — where it will be sent, as narrow as the truth allows. A tool that
  sends one credential to several sites gets them all in one request: Copilot
  CLI sends its GitHub token to `["api.github.com", "githubcopilot.com"]`.
- `justification` — why *this task* needs it. A person reads this to decide.
- `uses` — one sentence per thing you intend to do with it. **Write these as
  what you will actually run**, because a model later checks your command
  against this sentence (see step 3). "Open a pull request against the current
  repo" is answerable; "GitHub operations" is not.
- `grantTTLSeconds` — optional: how long you need the credential, in seconds,
  from 1 to 2592000 (thirty days). The person sees it as the answer already
  picked and may pick another. Ask for about as long as the task will take, not
  longer; leave it out to let them choose. You cannot ask for forever, and an
  ask outside that range is refused as `invalid`.
- `purpose` — optional: `"delegate"` (`--delegate`) asks to hand the
  credential on to other discoboxes rather than to use it. Its `uses` then say
  what you would delegate it for, and the approval lets you run nothing with it
  yourself — its use IDs are not `run` uses and `list` does not show them.
  Leave it out to ask to use the credential; if you need both, ask twice.
- `wait: true` blocks until a human answers. Without it you get a request ID
  back and the request sits pending — use `wait --json request REQUEST_ID`.

### Well-known credentials

Some credentials Discobox already knows the shape of. Ask for one of these by
its ID, and leave out `name`, `envVar`, and `hosts` — the ID says them, and
getting them wrong is then impossible:

| ID | What it is for | Delivered in | Sent to |
| --- | --- | --- | --- |
| `com.github.api` | GitHub: repositories over HTTPS, the REST and GraphQL API as `gh` uses it, and the Copilot API | `GH_TOKEN` | `github.com`, and the hosts beneath it such as `api.github.com`; `githubcopilot.com` too when named, as `--hosts api.github.com,githubcopilot.com` for Copilot CLI |
| `ai.discobox.sandbox` | The discobox API: create, list, and get discoboxes, read and type into the terminals of the ones you created and start and stop them, and answer credential requests | `DISCOBOX_TOKEN` | `api.discobox.internal`, through this discobox's pool |

```bash
discobox-access request com.github.api --use "Open a pull request against the current repo" --why "the task asks for a PR" --wait
```

or `"id": "com.github.api"` in the `--json` body. Everything else — `uses`,
`justification`, `grantTTLSeconds`, `purpose`, `wait` — is asked for exactly
as above.
For anything not in this table, spell out `name`, `envVar`, and `hosts`.

`ai.discobox.sandbox` is how you drive other discoboxes; §5 says how to ask
for it and launch them.

Use `--json` with a heredoc rather than flags: your justification will contain
apostrophes and quotes, and the shell would eat them. Unknown JSON fields are
rejected, so a misspelled key fails loudly instead of being dropped.

An approved request prints the use IDs you may now run with. A denial exits
non-zero — that is an answer, not an error. Do not re-ask for the same thing;
tell the user it was denied and what you cannot do without it.

## 3. Use it

```bash
discobox-access run --use use_7f3a2b -- gh pr create --fill
```

- The credential goes into that one child process's environment and nowhere
  else. It exits with your command's own status, like `env`(1).
- Before it runs, a model checks your command against the sentence the use was
  approved for. **Stay inside what was approved.** A command broader than the
  approved use is refused with `denied` and never starts. If you need something
  else, ask for it in step 2 rather than stretching an existing use.
- Everything after `--` is your command, run exactly as written.
- What your command reads on stdin — a here-document, a file, a pipe — is shown
  to the checker with it, up to 8 KiB, and your command still reads every
  byte. A command that takes its request on stdin (`discobox new --json`,
  `gh api --input -`) is judged by that request, so keep it to what the use
  approves; past 8 KiB the checker is told it was not shown the rest.

There is no command that prints the value on its own. `run` is the only way to
use one — if what you need to run cannot be `exec`'d directly, wrap it in a
shell: `discobox-access run --use use_7f3a2b -- sh -c '...'`.

## 4. A host whose certificate is refused

A host with its own CA — a Kubernetes API server, an internal service — is
refused by this sandbox's egress. You see it as a `502` whose
`X-Discobox-Untrusted-Host` header names the host (`curl -i`, or
`kubectl -v=8`). No credential fixes that; ask for the host to be trusted:

```bash
discobox-access trust 34.70.64.109:443 \
  --use "read-only kubectl: get/list/describe pods, deployments, events" \
  --why "the user's GKE cluster; its API server uses the cluster's own CA" \
  --wait
```

- The pool connects to the host itself and shows the person the certificates
  it was given; they pin one. A host that already verifies answers `unneeded`.
- `--ca-file` offers a CA you already have from a source you trust — for GKE,
  `gcloud container clusters describe NAME --format='value(masterAuth.clusterCaCertificate)' | base64 -d`.
  It is refused unless the host's chain verifies against it.
- The trust is this sandbox's alone, for exactly that `host:port`, and it
  lapses. **Every request you then send the host is judged against the uses
  you name**, so write them as what you will actually send.
- Trusting a host is not authenticating to it: a token for it is still a
  credential, asked for with `request` for that host — the cluster's own
  host, since a token approved for `googleapis.com` is never sent to
  `34.70.64.109`.
- **A client that carries its own CA must trust this sandbox's instead.**
  Every HTTPS connection here ends at the egress proxy, which presents a
  certificate signed by `/etc/discobox/proxy/mitm-ca.crt`; the proxy is what
  checks the host's real certificate against the pin. kubectl trusts only the
  CA in its kubeconfig, so it fails with `x509: certificate signed by unknown
  authority` until it is pointed at the proxy's:

  ```bash
  kubectl config set-cluster "$(kubectl config view --minify -o jsonpath='{.clusters[0].name}')" \
    --certificate-authority=/etc/discobox/proxy/mitm-ca.crt --embed-certs
  ```

  This is not skipping verification — the cluster's certificate is still
  verified, by the proxy, against the pin a person approved. Never set
  `insecure-skip-tls-verify`.
- **GKE with gcloud** needs both halves for the cluster's endpoint IP (from
  `gcloud container clusters describe NAME --format='value(endpoint)'`):
  1. `trust` that IP, with `--ca-file` holding the cluster CA from the same
     `describe` (`masterAuth.clusterCaCertificate`, base64-decoded).
  2. `request` the Google access token (`CLOUDSDK_AUTH_ACCESS_TOKEN`) with
     `host` set to that IP, not `googleapis.com`: kubectl sends it to the
     cluster, and a sentinel approved for another host is never swapped
     there. Name the kubectl commands in its uses.

  Then point the kubeconfig at the MITM CA (above) and run kubectl under that
  use: `discobox-access run --use <id> -- kubectl get pods`.
- An `unneeded` answer whose reason names an upstream proxy means this
  sandbox's egress goes through another proxy that checks the host itself;
  if the host is still refused, it has to be trusted where that proxy runs.
- `discobox-access trusts` lists what this sandbox trusts. `--json` reads the
  same fields from stdin as `request` does: `host`, `justification`, `uses`,
  `suppliedCA`, `grantTTLSeconds`, `wait`, `timeoutSeconds`.

## 5. Launching other discoboxes

`ai.discobox.sandbox` is how you drive other discoboxes: create workers, watch
them, and answer what they ask for. The `discobox` CLI is installed and already
pointed at the API; run it under an approved use, as with any credential.

**Launch workers with no credentials, and give them only what they ask for.**
A worker starts with none. When it needs one it asks with `discobox-access`,
as you do, and you approve or deny that request. Do not pass `--grant` or
`"grants"` to `discobox new`: a create is judged against your use, and
credentials folded into it are judged there as if handing them on were part
of the prompt. Answering a request is where handing a credential on is
decided.

### Ask for everything up front, once

Before the first worker, ask for everything the orchestration needs in one
request, so the person approves once and you can work on your own afterwards:

```bash
discobox-access request --json <<'EOF'
{
  "id": "ai.discobox.sandbox",
  "justification": "the task asks me to split the work across worker discoboxes; I create them with no credentials and answer what they ask for",
  "uses": [
    {"description": "discobox new -d --include-dirty=false -p <any prompt>, run in <this directory>: create a discobox with any prompt and no grants or secrets, from this directory and the sources it declares in .discobox/sources.json, including the polling, source push and complete-source-push that discobox new makes for the discobox it just created"},
    {"description": "discobox admin box ls and discobox admin box get <discobox-id>, to watch the discoboxes I created"},
    {"description": "discobox secret request ls, to see what the discoboxes I created are asking for"},
    {"description": "discobox secret ls, to see the secrets I was delegated"},
    {"description": "discobox secret request approve <request-id> [--secret-id <secret-id>] [--use <use>]: approve a pending credential request (the server lets me answer only my own discoboxes' requests, within the delegation grants I hold)"},
    {"description": "discobox secret request deny <request-id>: deny a pending credential request"},
    {"description": "discobox admin terminal ls --discobox-id <discobox-id>, discobox admin terminal screen <terminal-id> --discobox-id <discobox-id> [--scrollback N], and discobox admin terminal wait <terminal-id> --discobox-id <discobox-id> [flags]: read what a discobox I created shows in its terminals"},
    {"description": "discobox admin terminal input <terminal-id> --discobox-id <discobox-id> [--literal] <keys or text>: type keys and messages into the terminal of a discobox I created, to answer its questions or tell it to continue"},
    {"description": "discobox start <discobox-id>, discobox stop <discobox-id>, and discobox restart <discobox-id>: start, stop, or restart a discobox I created"}
  ],
  "grantTTLSeconds": 28800,
  "wait": true
}
EOF
```

Then, for each credential your workers will need, ask to delegate it
(`"purpose": "delegate"`, in §2), with uses saying what you will hand it on
for — "read-only GitHub access to issues in org/repo, for the discoboxes I
create" — and a lifetime as long as the orchestration. Without one you approve
nothing: the server hands on only what a delegation grant you hold covers —
that credential, to its host, for no longer than it lasts — so a person sees
what you mean to hand on before you hand on any of it.

Word the approve and deny uses as above: what the call does, with no condition
on whose request it is. Only the server can tell whose a request is, and it
enforces that; a condition in the use is one the judge cannot check.

Word the create use as broadly as above: **any prompt**. A use that quotes the
prompt, or names which work it is for, is read against the prompt of every
create, and the judge mistakes the worker's instructions for your purpose.

### Create a worker

```bash
discobox-access run --use <id> -- discobox new -d --include-dirty=false \
  -p "Implement issue #43 in org/repo. Follow CLAUDE.md, make the tests pass, and commit locally on main. Do not push or open a pull request."
```

- **The prompt states the task and nothing else.** Do not tell the worker how
  to get credentials or which to ask for, and do not name skills: its own
  skills say how, and credentials named in a prompt are read as credentials
  you are handing out. It asks for what it needs when it knows.
- It is cut from the directory you run it in, at its current commit.
  `--include-dirty=false` hands over only what is committed; leave it out to
  carry your uncommitted work too. `--no-source` gives it nothing checked out;
  `-i ../other` brings in another source beside it.
- It runs the project's default harness, as your user, with your Git identity.
  Leave `-H` out unless the person asked for a particular harness.
- It prints the new discobox; its ID is how you read it again:
  `discobox admin box get <id>`.

### Answer what your workers ask for

```bash
discobox-access run --use <id> -- discobox secret request ls -o json
discobox-access run --use <id> -- discobox secret request approve <request-id>
discobox-access run --use <id> -- discobox secret request deny <request-id>
```

- **You see and answer only your own discoboxes' requests.** The listing holds
  nothing else, and the server refuses any other.
- Read each request's `uses` and `justification`. Approve what the worker's
  task needs and what you were delegated; deny the rest. `--use` narrows the
  uses to fewer or tighter ones; never widen them.
- **Approve with the full request ID and nothing else.** The server answers
  with the secret of the delegation grant you hold, for what the worker asked,
  fitted within your delegation's remaining time — `approve` is then one call.
  Do not pass `--grant-ttl`: a lifetime you name that outlasts your
  delegation is refused rather than fitted. Pass `--secret-id` only when the
  server says you were delegated more than one secret that fits, naming one it
  lists. `discobox secret ls` shows you the secrets you were delegated, and
  nothing else of the project's.
- A request to delegate, one that names no uses, and anything your delegation
  grants do not cover are refused: those wait for a person. So does every
  request when you hold no delegation grant.

You cannot give a discobox `ai.discobox.sandbox`: a person grants that, when
the new discobox asks for it itself. What you approve is recorded as given by
you.

### See and talk to what your workers are doing

A worker that stops with uncommitted work is either between steps or waiting
on a question. Read its screen to tell which, and answer it:

```bash
discobox-access run --use <id> -- discobox admin terminal screen primary --discobox-id <discobox-id>
discobox-access run --use <id> -- discobox admin terminal input primary --discobox-id <discobox-id> "carry on and commit when the tests pass" Enter
discobox-access run --use <id> -- discobox admin terminal wait primary --discobox-id <discobox-id> --hook Stop
```

`primary` is the worker's harness terminal; `discobox admin terminal ls
--discobox-id <discobox-id>` lists the others. Ask for these in the up-front
request above, as their own uses — reading (`ls`, `screen`, `wait`) and typing
(`input`) — worded as the commands. Only the discoboxes you created answer.

A worker that has stopped — `discobox admin box ls` shows it, and `screen`
answers that it is stopped rather than starting it — is started again with
`discobox start`, and a wedged one with `discobox restart`. Stop a worker
only when you are done with it: stopping ends whatever it is doing. Its
workspace and uncommitted work survive either way.

```bash
discobox-access run --use <id> -- discobox start <discobox-id>
discobox-access run --use <id> -- discobox stop <discobox-id>
```

If a start fails, the error says why; a worker it cannot bring back is a
person's to repair.

Anything outside these commands is refused: you cannot attach to, start a
command in, archive, or delete a discobox you made.

## Never do this with the value

- Do not echo it, log it, or include it in a message to the user.
- Do not write it into a file, a `.env`, a config, or a shell export.
- Do not reuse it later — it expires in minutes. Ask again instead.
- Do not commit anything containing it.

## When it fails

Failures go to stderr, with `--json` as `{"error":{"code":"...","message":"..."}}`:

| code | what it means | what to do |
| --- | --- | --- |
| `invalid` | the call was malformed | fix the flags or JSON and retry |
| `denied` | you may not use this | ask for it with `request`, or accept the answer |
| `not_found` | that use ID means nothing here | run `list` again; it may have expired |
| `unavailable` | the service could not answer | retry once, then tell the user |

Exit status: `0` fine, `1` the call failed or the answer was no, `2` you
invoked it wrongly. Under `run`, your command's own status passes through.
