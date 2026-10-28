---
name: discobox-access
description: Ask a human for a credential this sandbox was not given, or for a host whose certificate is refused to be trusted, and wait for the answer. Use when a command fails with 401/403, when a CLI says it is not logged in or has no token (gh, npm, docker, curl against a private API), when a request answers 502 with an X-Discobox-Untrusted-Host header (a Kubernetes API server, an internal service with its own CA), when an outbound request fails and the program that made it does not say why, or to check which credentials you already hold.
allowed-tools: Bash
---

# Asking for a credential

You are working inside a discobox — a sandbox that deliberately holds no
long-lived secrets. When a command needs a credential you do not have, you do
not have to stop and ask in chat: `discobox-access` asks a person, they answer
in their discobox window, and you carry on.

A credential you hold is already in its environment variable, for every
command you start. It is a stand-in the sandbox's egress swaps for the real
value, and only on the routes it was approved for. Use it as you would any
token: `gh pr create`, `npm publish`, `curl -H "Authorization: Bearer
$API_TOKEN" …`. Nothing here prints a secret.

## Stay with approval requests until they resolve

For credentials and host trust, use `--wait` (or `"wait": true`). If your
execution tool returns a running session or job ID, keep waiting on **that
same execution**. A tool yield is not a timeout or a completed request. In
Codex, use `write_stdin` for an `exec_command` session; if `functions.exec`
itself yields a cell ID, use `functions.wait` for that cell.

While approval is pending, provide occasional progress updates and continue
independent work when useful. Do not end your turn merely to announce that
access was requested, and do not ask the user to say "continue". After
approval, automatically resume the authorized task. Stop waiting only when the
request resolves, the command fails (exit 1), or the user cancels. A wait that
runs out (exit 3, the request still `pending`) is not an answer: wait again on
the same ID. Elapsed time is never approval.

If the running execution is lost, resume waiting on the existing request ID;
do not create a replacement request:

```bash
discobox-access wait --json request sreq_7f3a2b
discobox-access wait --json trust treq_7f3a2b
```

Flags go before the request kind and ID. `--timeout 1h` controls the wait
(default one hour). A timeout stops waiting; it does not cancel the request or
deny it. A wait that runs out prints the request as `pending`, says how to
resume, and exits 3 — wait again on the same ID. A brief network or relay
failure while waiting does not end the wait; it keeps polling. A denial is final: report it rather than requesting the same access
again. Pending notices appear on stderr, including in JSON mode; stdout holds
the result.

## 1. Check what you already have

**Run this before every request:**

```bash
discobox-access list
```

```
github (GH_TOKEN → github.com, api.github.com) [expires 2026-10-09T18:00:00Z]
npm (delegate: give it to the discoboxes you create → registry.npmjs.org) [never expires]
```

Each line is a credential, the variable it is in, and the hosts of the routes
it may be sent on. A host is exact: `github.com` is not `api.github.com`. The person approving picks the
lifetime, often a different one than you asked for; `[never expires]` means
exactly that, and `list` is the answer to whether something has lapsed.

If a command *already works*, the credential is already in your environment —
nothing here is needed. Ask only when something actually failed for want of
one.

## 2. Ask for one

```bash
discobox-access request \
  --name github --env-var GH_TOKEN \
  --route 'github.com/acme/web.git/**,/acme/web/**' \
  --route 'GET api.github.com/repos/acme/web/**' \
  --route 'api.github.com/graphql graphql=query' \
  --route 'POST api.github.com/repos/acme/web/pulls' \
  --route 'api.github.com/graphql graphql=mutation:markPullRequestReadyForReview' \
  --justification "Fix issue #43 in acme/web as a pull request from branch fix-43 into main" \
  --ttl 4h --wait
```

- `--name` names the credential the way a person would recognise it
  (`github`, `npm`, `openai`).
- `--env-var` is the variable you want it in. Once granted, it is set for
  every command you start from then on; a shell already running keeps its old
  environment, so start a new one.
- `--route` is each place you will send it, one route per flag, as narrow
  as the task: the person approving reads every route, and a narrow one is
  easier to say yes to. It is sent nowhere else: a request carrying it
  anywhere else is refused by the egress (§6). A route is

  ```
  [METHODS ]HOSTS[PATHS][ graphql=OPS[:FIELDS]][ mcp=TOOLS][=blocked]
  ```

  - `METHODS`: upper-case, joined by commas (`GET`, `GET,HEAD`). Left out,
    any method.
  - `HOSTS`: joined by commas. **A host is exact**: `github.com` is not
    `api.github.com`; `*.github.com` is every host beneath `github.com` (but
    not `github.com` itself), and `*` alone is any host.
  - `PATHS`: from the first `/`, several joined by `,/`
    (`github.com/acme/web.git/**,/acme/web/**`). A path is matched whole, a
    segment at a time and ignoring case: `*` is exactly one segment, `**`
    (only last) any number, so `/repos/acme` is that one path and
    `/repos/acme/**` everything beneath it too. Left out, any path. A
    `:port` on a host is dropped.
  - `graphql=OPS[:FIELDS]`: the GraphQL operations allowed (`query`,
    `mutation`, `subscription`, or `*`), and optionally the root fields they
    may select — the mutation's name, such as
    `markPullRequestReadyForReview`. The document itself is read, not
    `operationName`. A mutation's arguments are not: one that takes a node
    ID acts on any repository the token can write, and `graphql=query`
    reads any it can see, so where REST can do the write (opening a PR,
    commenting), ask for the REST route under `repos/OWNER/REPO/`, which is
    held to that repository.
  - `mcp=TOOLS`: the MCP tools a `tools/call` may name (`search*` for a
    prefix, `*` for any); the protocol's own methods pass.
  - `=blocked` at the end carves a route out of a wider one.

  A route that does not parse is refused by the service as `invalid`, with
  the route named. `--hosts
  HOST[,HOST...]` is shorthand for one route to each exact host, any method
  and path; prefer routes.
- `--justification` is what the person reads: the task and its target, in
  one line.
- `--ttl` asks how long to keep it (`30m`, `4h`, up to `720h`). The person
  may choose otherwise. Leave it out to let them pick.
- `--delegate` asks to give the credential to the discoboxes you create,
  rather than to use it yourself (§5).

With `--json`, the same request is read from stdin, which keeps quotes and
apostrophes in your justification out of the shell's hands:

```bash
discobox-access request --json <<'EOF'
{
  "name": "github",
  "envVar": "GH_TOKEN",
  "routes": [
    "github.com/acme/web.git/**,/acme/web/**",
    "GET api.github.com/repos/acme/web/**",
    "api.github.com/graphql graphql=query",
    "POST api.github.com/repos/acme/web/pulls",
    "api.github.com/graphql graphql=mutation:markPullRequestReadyForReview"
  ],
  "justification": "Fix issue #43 in acme/web as a pull request from branch fix-43 into main",
  "grantTTLSeconds": 14400,
  "wait": true
}
EOF
```

`hosts` (the `--hosts` shorthand), `purpose` (`"use"`, the default, or
`"delegate"`) and `timeoutSeconds` (how long `wait` waits) are the other
fields. An unknown field is refused, so a
typo cannot drop your justification.

A granted request says what you now hold, as the person chose it — the routes
and lifetime may differ from your ask.

## 3. Use it

Run the command that needs it. The credential is in the variable you named,
for every command started once it has reached your discobox — which follows
the approval by a few seconds, as the pool is told. If the variable is not there
yet, or a request carrying it is refused as unauthenticated, run `env` again
in a moment. A shell that was already
running — your harness's tool shell inherits the environment the harness was
launched with — takes it up first:

```bash
eval "$(discobox-access env)"
```

`env` prints an export for every credential variable and what is delivered in
it now: a placeholder your egress exchanges, never the credential itself
(`--json` for the map). A person may instead hand you a credential's real
value (`list` says `the value itself`): one in a file is printed as the file's
path, one in its variable is left out of `env` and reaches only commands
started after it arrived. Send it only where it is meant to go; a request
carrying it anywhere else is refused, and recorded.

A request the credential is not allowed to go on is refused by the egress
before it leaves, and the reason is in the response and in the audit (§6):
the routes you were approved for, and the ones the secret itself carries, are
all held to — a method, a path, a GraphQL mutation outside them is refused.
Ask again for what you need rather than working around it.

## 4. A host whose certificate is refused

A host with its own CA — a Kubernetes API server, an internal service — is
refused by this sandbox's egress. You see it as a `502` whose
`X-Discobox-Untrusted-Host` header names the host (`curl -i`, or
`kubectl -v=8`). No credential fixes that; ask for the host to be trusted:

```bash
discobox-access trust 34.70.64.109:443 \
  --justification "the user's GKE cluster; its API server uses the cluster's own CA" \
  --wait
```

- The pool connects to the host itself and shows the person the certificates
  it was given; they pin one. A host that already verifies answers `unneeded`.
- `--ca-file` offers a CA you already have from a source you trust — for GKE,
  `gcloud container clusters describe NAME --format='value(masterAuth.clusterCaCertificate)' | base64 -d`.
  It is refused unless the host's chain verifies against it.
- The trust is this sandbox's alone, for exactly that `host:port`, and it
  lapses. It makes that certificate acceptable there and nowhere else.
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
  2. `request` the Google access token (`--env-var CLOUDSDK_AUTH_ACCESS_TOKEN`)
     with `--route` set to that IP, not `googleapis.com`: kubectl sends it to
     the cluster.

  Then point the kubeconfig at the MITM CA (above) and run kubectl.
- An `unneeded` answer whose reason names an upstream proxy means this
  sandbox's egress goes through another proxy that checks the host itself;
  if the host is still refused, it has to be trusted where that proxy runs.
- `discobox-access trusts` lists what this sandbox trusts. `--json` reads
  `host`, `justification`, `suppliedCA`, `grantTTLSeconds`, `wait` and
  `timeoutSeconds` from stdin.

## 5. Launching other discoboxes

The `discobox` CLI is installed and already pointed at the discobox API. It
works only in a discobox a person has given API access; without it every call
is refused, and only a person can turn it on — ask the user in chat, since
`discobox-access` cannot ask for it. With it you can create discoboxes, and
start, stop, tag and read the terminals of the ones **you created**; nothing
else in the project.

**Launch workers with no credentials, and give them only what they ask for.**
A worker starts with none. When it needs one it asks with `discobox-access`,
as you do, and you approve or deny that request.

Before the first worker, ask to delegate each credential your workers will
need — `--delegate`, with the routes they will reach and no `--env-var`, since
a delegation is delivered to nothing in your discobox — so a person approves
once and you can answer your workers afterwards. Every worker's binding is
held to the delegation's routes as well as its own, so they must cover every
route a worker will ask for:

```bash
discobox-access request --delegate \
  --name github \
  --route 'github.com/acme/web.git/**,/acme/web/**' \
  --route 'GET api.github.com/repos/acme/web/**' \
  --route 'api.github.com/graphql graphql=query' \
  --route 'POST api.github.com/repos/acme/web/pulls' \
  --route 'api.github.com/graphql graphql=mutation:markPullRequestReadyForReview' \
  --justification "hand GitHub to the worker discoboxes I create for issues #43 and #44 of acme/web" \
  --ttl 8h --wait
```

A delegation authorizes nothing you send yourself; ask separately to use the
credential if you need it too. A person may already have given you one
(`discobox secret bind <secret> <this discobox> --delegate`): `discobox-access
list` shows it as a delegation. Under it you may also give a worker the
credential when you create it, `discobox run --bind <secret>:<VAR> …`, which
the server holds to your delegation as it does an approval.

### Create a worker

```bash
discobox run -d . \
  -p "Implement issue #43 in org/repo. Follow CLAUDE.md, make the tests pass, and commit locally on main. Do not push or open a pull request."
```

- **The prompt states the task and nothing else.** Do not tell the worker how
  to get credentials or which to ask for: its own skills say how, and it asks
  for what it needs when it knows.
- It is cut from SOURCE, here `.`, the directory you run it in, at its
  current commit. Only what is committed is handed over; `--include-dirty`
  carries your uncommitted work too. `--no-source` gives it nothing checked
  out; `-i ../other` brings in another source beside it.
- It runs the project's default harness, as your user, with your Git identity.
  Leave `-H` out unless the person asked for a particular harness.
- It prints the new discobox; its ID is how you read it again:
  `discobox admin box get <id>`.

### Answer what your workers ask for

```bash
discobox secret request ls -o json
discobox secret request get <request-id>
discobox secret request approve <request-id>
discobox secret request deny <request-id>
```

- **You see and answer only your own discoboxes' requests.** The listing holds
  nothing else, and the server refuses any other.
- Read each request's `justification` and routes. Approve what the worker's
  task needs and what you were delegated; deny the rest, or approve it
  narrower with `--route` (one per flag).
- The server approves only under a delegate binding you hold of that secret:
  the worker's binding is made under it, held to its routes as well as its
  own, and lasts no longer than it. Pass `--secret` only when the server says
  you were delegated more than one secret that fits. If the delegation is
  removed, every binding made under it goes too.
- A request to delegate waits for a person, as does every request when you
  hold no delegation that covers it.

### See and talk to what your workers are doing

A worker that stops with uncommitted work is either between steps or waiting
on a question. Read its screen to tell which, and answer it:

```bash
discobox admin terminal screen <discobox-id> primary
discobox admin terminal input <discobox-id> primary "carry on and commit when the tests pass" Enter
discobox admin terminal wait <discobox-id> primary --hook Stop
```

`primary` is the worker's harness terminal; `discobox admin terminal ls
<discobox-id>` lists the others. A worker that has stopped is started again
with `discobox start`, and a wedged one with `discobox restart`. Stop a worker
only when you are done with it. Its workspace and uncommitted work survive
either way. Anything else — attaching, archiving, deleting — is a person's.

## 6. A request that failed without saying why

Every outbound HTTP request this sandbox makes goes through an egress proxy
that records what became of it. When it refuses one, it says why in the
response, and many programs drop that: an SDK that reports "request failed", a
build that says "could not fetch". Read the record instead:

```
discobox-access audit --blocked --since 10m
```

```
2026-10-02T12:00:01Z  403 BLOCKED  POST https://api.github.com/repos/o/r/pulls
    reason: route: this credential has no route to POST api.github.com/repos/o/r/pulls
```

| reason begins | what happened | what to do |
| --- | --- | --- |
| `route:` | the credential's routes do not allow that method, path or GraphQL/MCP call | send it where its routes allow, or ask for the route you need (§2) |
| `upstream certificate …` | the host's certificate is not trusted | ask for a trust (§4) |
| `gate:` | a call to the discobox API was refused | this discobox has no API access; tell the user (§5) |
| `host denied` | this discobox's network policy does not allow the host | nothing here changes it; tell the user |

A request the destination refused is not `BLOCKED`: a 401 or 403 for a
missing or wrong credential is recorded with that status. Leave out
`--blocked` to see those — then check what you have (§1) and ask (§2).
`--host` narrows to one destination (any port when you name none), and
`--json` gives the same fields structured. It shows only this sandbox's
requests.

## When it fails

Failures go to stderr, with `--json` as `{"error":{"code":"...","message":"..."}}`:

| code | what it means | what to do |
| --- | --- | --- |
| `invalid` | the call was malformed | fix the flags or JSON and retry |
| `denied` | you may not do this | ask for it with `request`, or accept the answer |
| `not_found` | that request ID means nothing here | check the ID; ask again only if it is gone |
| `unavailable` | the service could not answer | retry once, then tell the user |

Exit status: `0` fine, `1` the call failed or the answer was no, `2` you
invoked it wrongly, `3` a wait ran out with the request still pending — wait
again on the same ID.

To see exactly what was sent and answered, put `--debug` before the command
(`discobox-access --debug request ...`): every call to the credentials service
is printed on stderr with its body and the answer's.
