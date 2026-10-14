---
name: discobox-access
description: Ask a human for a credential this sandbox was not given, and run one command with it; or ask for a host whose certificate is refused to be trusted. Use when a command fails with 401/403, when a CLI says it is not logged in or has no token (gh, npm, docker, curl against a private API), when a request answers 502 with an X-Discobox-Untrusted-Host header (a Kubernetes API server, an internal service with its own CA), when an outbound request fails and the program that made it does not say why, or to check which credentials you may already use.
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

**Run this before every request**, even for something you were granted a
moment ago:

```bash
discobox-access list
```

Each credential lists the **use IDs** approved for it, and how long each lasts:

```
github (GH_TOKEN → github.com)
  use_7f3a2b  Fix issue #43 in acme/web as a pull request from branch fix-43 into main. … [expires 2026-10-09T18:00:00Z]
  use_9c4d1e  Triage issues in acme/web. Read anything in the repository; … [never expires]
```

If the command you want to run is part of the job one of those uses names,
skip to step 3. A use
listed here has not expired, whatever lifetime you asked for: the person
approving picks the lifetime, often a longer one than you asked, and
`[never expires]` means exactly that. Never assume a use has lapsed because
the time you asked for has passed — `list` is the answer.

If a command *already works*, the credential is already in your environment —
nothing here is needed. Ask only when something actually failed for want of one.

## 2. Ask for one

```bash
discobox-access request --json <<'EOF'
{
  "name": "github",
  "envVar": "GH_TOKEN",
  "hosts": ["api.github.com"],
  "justification": "the task asks me to fix issue #43 and get the fix merged through a pull request",
  "uses": [{"description": "Fix issue #43 in acme/web as a pull request from branch fix-43 into main. Read anything in the repository; push that branch, open and update the pull request, and comment on it. Never push main or any other branch, delete a branch, merge or close the pull request, or delete a comment."}],
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
- `uses` — what you will do with it, **one use per job, not one per
  command**. A model later checks each command against the use it runs under
  (step 3), so a use has to say where the job stops. "Writing a use", below,
  is how.
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
| `ai.discobox.sandbox` | The discobox API: create, list, and get discoboxes; on any discobox in this project but its judge, deliver its source, read and type into its terminals, start, stop, restart, repair, and upgrade it, run its services and read their logs, reach its ports over HTTP, and tag it; and answer the credential requests of the ones you created | `DISCOBOX_TOKEN` | `api.discobox.internal`, through this discobox's pool |

```bash
discobox-access request com.github.api --use "Open a pull request from branch fix-43 into main of acme/web. Read anything in the repository; push that branch and open the pull request. Never push main or another branch, delete a branch, or merge or close the pull request." --why "the task asks for a PR" --wait
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

### Writing a use

A use is a rule a person approves at a glance and a model then holds every
command to. Write it in three parts, always in this order:

```
<The job> in <the target>. Read <what>; <each kind of write>. Never <the lines>.
```

> Fix issue #43 in acme/web as a pull request from branch fix-43 into main.
> Read anything in the repository; push that branch (with --force-with-lease
> after a rebase), open and update the pull request, comment on it and reply
> to its reviews, and re-run its failed CI. Never push main or any other
> branch, delete a branch, merge or close the pull request, delete a comment,
> or start any other workflow.

That one use covers every `git push`, `gh pr …`, `gh run …` and `gh api`
call the job takes, in whatever form you end up writing them, and nothing
past its lines.

1. **One use per job.** A job is what you would tell a colleague you are
   doing: "fix issue #43 as a PR", "triage issues", "debug the web pods".
   Most tasks need one use. Split only where a person might approve one part
   and refuse the other: changing production beside reading it, answering
   credential requests beside watching workers.
2. **Name the target exactly** — the repository, branch, namespace, package,
   channel, tag. The target is what keeps a broad use narrow. Fill in every
   placeholder before you send it.
3. **Say what it reads as a scope**: "read anything in the repository", "read
   anything in namespace web", or "read-only" when it writes nothing. Without
   one the model reads the job narrowly — "fix issue #43" does not obviously
   cover reading issue #40, or a release. Name a second target here if the job
   reads one ("and in acme/homebrew-tap").
4. **Name every kind of write**: push, comment, label, re-run, create, tag.
   Each is something a person is agreeing to; none is implied.
5. **For every write, name its dangerous neighbor in "Never …".** A judge
   reads a write generously, so its nearest overreach has to be ruled out in
   words, or it comes with the write. Ask three questions of each write you
   named (the table below works them):
   - **Wider** — the same action on more than you named: removing a label
     from an issue is next to deleting the label from the repository.
   - **Harsher** — the irreversible or final version: pushing is next to
     deleting the branch, opening a pull request next to merging or closing
     it, commenting next to deleting comments.
   - **Elsewhere** — the same action on the target beside yours: your
     branch is next to `main`, your namespace next to another, your tag next
     to an older one.

   For a read, the neighbor is secrets, and whatever is readable just
   outside the scope you stated. Each answer the job does not need goes in
   "Never …", and each must be one a command can show (rule 7): "never
   delete a comment", not "never edit others' comments". This is not a list
   of everything bad — that is already outside — but of what a reader might
   think came with the job, usually two to five things.
6. **Actions, not commands.** "Push that branch", not
   `git push --force-with-lease=refs/heads/…:<sha>`. Name a flag only when the
   flag is the line (`--force-with-lease`, not `--force`).
7. **Only lines a command can show.** The model sees one command, not the
   state of the world, so "only once CI is green" or "only my workers'
   requests" are conditions nobody checks. Leave out what the server already
   enforces.
8. **Never vouch for yourself.** No "approved", "safe", "pre-cleared", and
   nothing addressed to the model checking it: text that argues for itself is
   refused.

| | Use |
| --- | --- |
| Too fine | `gh api GET repos/acme/web/pulls/<n>, pulls/<n>/reviews and pulls/<n>/comments, and POST pulls/<n>/comments (replies with in_reply_to)` |
| Too vague | `GitHub operations for the task` |
| Right | `Answer the review on pull request #812 in acme/web. Read anything in the repository; reply to its comments and push fixes to its branch fix-43. Never merge.` |
| Too fine | `kubectl get pods -n shop`, `kubectl logs -n shop`, `kubectl describe pod -n shop` |
| Right | `Find out why the api pods restart in namespace shop of the prod cluster. Read anything in namespace shop; change nothing. Never exec into a pod or read secrets.` |

Common writes and the neighbors to rule out:

| Write | Wider | Harsher | Elsewhere |
| --- | --- | --- | --- |
| add and remove issue labels | delete or rename a repository label | — | label another repository's issues |
| push that branch | — | push without a lease, delete the branch | push `main` or another branch |
| open and update the pull request | — | merge it, close it | open one in another repository |
| comment and reply | — | delete a comment | comment on unrelated issues |
| re-run its failed CI | start any other workflow | cancel or delete runs | — |
| push tag vX.Y.Z | — | move or delete a tag | push another tag |
| type into the workers' terminals | start a command in them | delete a worker | — |
| approve or deny requests | create or change a secret | — | — |

A use without its neighbors ruled out reads as permission for them:

> ✗ Triage issues in acme/web. Read anything in the repository; add and remove
> issue labels, and post my triage comments. Never close an issue.
>
> ✓ Triage issues in acme/web. Read anything in the repository; add and remove
> issue labels, and post my triage comments. Never delete or rename a label,
> delete a comment, or close, assign or retitle an issue.

A judge allowed `gh label delete` under the first.

When a command is refused under a use you hold, read the reason. Do not ask
again for the same job with the refused command spelled out. Either:

- **the command was not the job** — do it another way;
- **the job grew** — ask for a use that names the new job; or
- **it plainly is a step of the job and was refused anyway** (a judge can
  refuse a read the use covers) — ask once for that step as a job of its
  own, in the same shape: "Read the CI of pull request #812 in acme/web,
  read-only. Never re-run or cancel a run." Say in the justification which
  use it is a step of and what the refusal said.

An approved request prints the use IDs you may now run with, and when it
reports one, the expiry the person chose; `list` is what says how long a use
lasts, forever included — read it there rather than remembering your ask. A
denial exits
non-zero — that is an answer, not an error. Do not re-ask for the same thing;
tell the user it was denied and what you cannot do without it.

## 3. Use it

```bash
discobox-access run --use use_7f3a2b -- gh pr create --fill
```

- The credential goes into that one child process's environment and nowhere
  else. It exits with your command's own status, like `env`(1).
- Before it runs, a model checks your command against the use it runs under.
  **Stay inside that job.** Any command that does the job on its target is
  fine, written however you need it; a command past the use's lines, or on
  another target, is refused with `denied` and never starts. If the work has
  grown into another job, ask for that job in step 2 rather than stretching
  an existing use.
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
  --use "Debug the web app in namespace web of this cluster. Read anything in namespace web; change nothing. Never exec into a pod or read secrets." \
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
  you name**, so write them as in "Writing a use": the job and its target,
  what it reads and writes, and its lines.
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
     there. Its use is the same job as the trust's.

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
    {"description": "Create worker discoboxes from <this directory>, each with any prompt and no grants or secrets. Read the discoboxes I create; create them and push this directory's sources to them. Never give one a grant or secret, or create one from another directory or repository."},
    {"description": "Supervise the discoboxes I created. Read their status and terminals; type into their terminals, and start, stop, restart and tag them. Never delete them or start a command in them."},
    {"description": "Answer pending credential requests. Read them and the secrets I was delegated; approve or deny them. Never create, change or delete a secret."}
  ],
  "grantTTLSeconds": 28800,
  "wait": true
}
EOF
```

Then, for each credential your workers will need, ask to delegate it
(`"purpose": "delegate"`, in §2), with uses saying what you will hand it on
for, each the job a worker will do in "Writing a use"'s shape with "for the
discoboxes I create" in front — "For the discoboxes I create: triage issues in
org/repo. Read anything in the repository; add and remove issue labels, and
post my triage comments. Never delete or rename a label, delete a comment, or
close, assign or retitle an issue." — and a lifetime as long as the
orchestration. Without one you approve nothing: the server hands on only what a delegation grant you hold covers —
that credential, to its host, for no longer than it lasts — so a person sees
what you mean to hand on before you hand on any of it.

These are three jobs, split where a person might approve one and not
another: making boxes, driving the ones you made, and handing out
credentials.

Word the answer use as above: no condition on whose request it is. Only the
server can tell whose a request is, and it enforces that; a condition in the
use is one the judge cannot check (rule 7 of "Writing a use").

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
  uses to fewer or tighter ones; never widen them. A narrowed use keeps the
  whole "Never …" sentence of the delegated line it falls under: the judge
  reads a use that drops any of it as wider, however narrow the rest is.
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
--discobox-id <discobox-id>` lists the others. All of these run under the
supervise use above. Any discobox in this project but its judge answers;
which ones a use reaches is the judge's to read from its words, and the
judge is told which discobox created the one a call names, so the supervise
use's "the discoboxes I created" holds you to yours.

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

## 6. A request that failed without saying why

Every outbound HTTP request this sandbox makes goes through an egress proxy
that records what became of it. When it refuses one — a host that is not
trusted, a credential used outside its approved use, a call the judge would
not allow — it says why in the response, and many programs drop that: an SDK
that reports "request failed", a build that says "could not fetch". Read the
record instead:

```
discobox-access audit --blocked --since 10m
```

```
2026-10-02T12:00:01Z  403 BLOCKED  POST https://api.github.com/repos/o/r/pulls
    reason: judge: opening a pull request is not the approved use
    uses: use_7f3c…
```

The reason tells you what to do next:

| reason begins | what happened | what to do |
| --- | --- | --- |
| `judge:` | the request was held up to the use its credential was approved for, and refused | send what that use covers, or ask for the use you need (§2) |
| `upstream certificate …` | the host's certificate is not trusted | ask for a trust (§4) |
| `gate:` | a call to the discobox API without a live use of `ai.discobox.sandbox` | do what the reason says (§5) |
| `host denied` | this discobox's network policy does not allow the host | nothing here changes it; tell the user |

A request the destination refused is not `BLOCKED`: a 401 or 403 for a missing
or wrong credential is recorded with that status. Leave out `--blocked` to see
those — then check what you have (§1) and ask (§2). `uses:` names the approved
uses the request carried. `--host` narrows to one destination (any port when
you name none), and `--json` gives the same fields structured. It shows only
this sandbox's requests.

## Never do this with the value

- Do not echo it, log it, or include it in a message to the user.
- Do not write it into a file, a `.env`, a config, or a shell export.
- Do not reuse the value later — it expires in minutes. Run the use again
  instead; the use itself lasts as long as `list` says.
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

To see exactly what was sent and answered, put `--debug` before the command
(`discobox-access --debug run --use ... -- ...`): every call to the credentials
service is printed on stderr with its body and the answer's. A credential's
value shows as `<redacted>`.
