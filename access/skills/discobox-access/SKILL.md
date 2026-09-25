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
  "host": "api.github.com",
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
- `host` — where it will be sent. As narrow as the truth allows.
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
its ID, and leave out `name`, `envVar`, and `host` — the ID says them, and
getting them wrong is then impossible:

| ID | What it is for | Delivered in | Sent to |
| --- | --- | --- | --- |
| `com.github.api` | GitHub: repositories over HTTPS, and the REST and GraphQL API as `gh` uses it | `GH_TOKEN` | `github.com`, and the hosts beneath it such as `api.github.com` |
| `ai.discobox.sandbox` | The discobox API: create, list, and get discoboxes, give a new one uses of project secrets, and answer credential requests | `DISCOBOX_TOKEN` | `api.discobox.internal`, through this discobox's pool |

```bash
discobox-access request com.github.api --use "Open a pull request against the current repo" --why "the task asks for a PR" --wait
```

or `"id": "com.github.api"` in the `--json` body. Everything else — `uses`,
`justification`, `grantTTLSeconds`, `purpose`, `wait` — is asked for exactly
as above.
For anything not in this table, spell out `name`, `envVar`, and `host`.

`ai.discobox.sandbox` is how you drive other discoboxes. The `discobox` CLI is
installed and already pointed at the API; run it under an approved use, as
with any credential.

Make a discobox with `discobox new --json`, the request on stdin:

```bash
discobox-access run --use <id> -- discobox new --json <<'EOF'
{
  "prompt": "Fix issue 42 in org/repo, then push the branch fix-42.",
  "grants": [
    {"id": "com.github.api", "uses": [{"description": "push the branch fix-42 to org/repo"}]}
  ]
}
EOF
```

- It is cut from the directory you run it in: your repository at its current
  commit, **with your uncommitted work on top**. Set `"includeDirty": false`
  to hand over only what is committed. `"noSource": true` gives it nothing
  checked out; `"include": ["../other"]` brings in another source beside it.
- It runs the project's default harness. Leave `"harness"` out unless the
  person asked for a particular one.
- It runs as your user, with your Git identity, like a discobox a person
  starts with `discobox new`.
- `"grants"` gives it uses of credentials: a well-known one by `"id"`, or any
  other project secret as `"secret"` (its name) and `"envVar"`, with an
  optional `"host"` to narrow it. Write each use as the command it will run,
  exactly as you would ask for one yourself — its agent is judged against
  that sentence when it runs it.
- It answers with the new discobox as JSON. Its `"id"` is how you read it
  again: `discobox admin box get <id>`.

`discobox new --help` lists every field. The rest of what you may do:

```bash
discobox-access run --use <id> -- discobox admin box ls
discobox-access run --use <id> -- discobox admin box get <discobox-id>
discobox-access run --use <id> -- discobox secret request ls --status pending
discobox-access run --use <id> -- discobox secret request approve <request-id> --secret-id github
```

You cannot give a discobox `ai.discobox.sandbox`: a person grants that, when
the new discobox asks for it itself. What you give is recorded as given by
you. Anything outside those commands is refused — you cannot attach to,
stop, or delete a discobox you made.

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
