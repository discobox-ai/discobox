# 26-10-27-840 — A binding may hand over the raw value, held in memory and watched for at the proxy

- **Status**: Accepted
- **Date**: 2026-10-27
- **Narrows**:
  [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md)
  §1 (a secret and a binding gain a raw flag), §3 (raw is never handed on),
  §4 (a raw binding's routes are enforced by watching for the value, not by a
  swap) and §6 (the audit records a raw binding's spend by match).

## Context

A binding delivers a sentinel, and the pool proxy swaps it for the value in a
request's headers or query string, on a route the binding allows. The sandbox
never holds the credential. That fails for programs that need the value
itself:

- one that signs with it (an HMAC key, a private key, AWS secret keys) rather
  than sending it;
- one that sends it in a body or a protocol the proxy does not rewrite
  (database wire protocols, SSH, a JSON login body);
- one that validates its shape or length beyond what a sentinel format
  mimics, or derives something from it locally.

Today the workaround is to paste the value into the sandbox by hand, where it
lands on disk, is held to nothing, and is recorded nowhere.

Everything that carries a sentinel to a sandbox is written down along the
way: the pool's `runtime-config.json` and proxy `secrets.json` (its
`Environment`), the sandbox's kept `/var/lib/discobox/runtime-config.json`,
and `/run/discobox/secrets`, which a Docker sandbox bind-mounts from the
pool's disk. Exec environments are sent to the server in the inventory. None
of that is acceptable for a raw value, so it cannot ride the sentinel path.

## Decision

### 1. Raw is opted into by the secret and by the binding

- `Secret.AllowRaw bool`: the secret may ever be handed over raw. It is part
  of "the furthest it may ever be sent" (26-10-27-880 §1). Off by default,
  and on upgrade.
- `SecretBinding.Raw` is the delivery: empty (a sentinel, as today), `env`
  (the variable holds the value), or `file` (the value is in a file under
  `/run`, and the variable holds its path). `Raw` needs `Use` and a secret
  with `AllowRaw`. Clearing `AllowRaw` deletes the secret's raw bindings,
  with a binding event each.
- Every type may be raw. An OAuth or exchange secret's raw value is its
  current access token, and is replaced when it rotates (§3).

### 2. Only a person makes a binding raw

A raw binding is written only by a person: directly, at create, or by
approving a request that asks for raw. A `Delegate` holder hands on `Use`
without `Raw` (§3 of 26-10-27-880 stays the only way a sandbox gives a
binding), and a binding written under a parent is never raw. A raw value
cannot be taken back once seen, so who saw it is a person's decision.

### 3. The value is pulled into memory and never written to disk

- The server never puts a raw value in a runtime config, a sentinel push, or
  any record. It pushes the pool the raw binding's ID, env name, delivery,
  and routes, beside the sentinel set.
- The sandbox agent asks for the value over its mTLS link to its pool (the
  credentials relay). The pool asks the server, which checks the binding is
  live, raw, applies to that sandbox, and its secret still allows raw, and
  answers with the value. The pool keeps it in memory, as the swap cache
  does, and never writes it.
- The sandbox agent writes it to `/run/discobox/raw-secrets/<ENV_NAME>`,
  mode 0400, owned by the sandbox user, in a 0700 directory. That directory
  is on the sandbox's own `/run` tmpfs and is never bind-mounted from the
  pool. It is excluded from export, and gone when the sandbox stops; the
  agent pulls again on start.
- `file` delivery sets `ENV_NAME` to that path. `env` delivery reads the
  file when an exec or terminal starts and sets `ENV_NAME` to the value in
  that process's environment only.
- No record a sandbox keeps on disk or sends out carries the value: the
  agent drops raw names from an exec's stored and inventoried environment,
  `discobox-access env` prints the path for `file` and omits `env` names,
  and harness-file templates cannot read a raw binding (`.secrets.NAME`
  renders into `$HOME`, which is on disk).
- On rotation, revocation, or expiry the pool tells the agent, which
  rewrites or removes the file. A running process started with `env`
  delivery keeps the value it was started with; `file` delivery sees the
  new one.

### 4. The proxy watches for the value, and refuses it off its routes

The proxy cannot swap what the sandbox already holds, so a raw binding's
routes are enforced by recognising the value on the way out:

- The pool holds each raw binding's value and its effective routes (secret,
  binding, parents, as for a sentinel). Every request from the sandbox is
  scanned for the literal value, and for its base64, base64url and
  percent-encoded forms, in headers, query string and the captured body.
- A request carrying it to a place its routes do not allow is refused and
  audited as a refusal naming the binding. One on its routes is forwarded
  and audited as spending the binding.
- The value is redacted in the HTTP audit wherever it was found, and
  responses are not scanned.

This is a guard against mistakes and naive exfiltration, not a boundary. A
sandbox holding the value can split, encrypt, or encode it otherwise, send
it over a connection the proxy cannot read, or keep it for later. The
binding records that a person accepted that, and its `Raw` is shown wherever
bindings are, so a reader knows which credentials have left the pool.

### 5. Audit

A raw binding's handover is a binding event (26-10-27-880 §6) with the
pulling sandbox, so when a value reached which sandbox is on record. Its
spends and refusals are in the pool's HTTP audit by binding ID, by match
rather than by swap.

## Alternatives rejected

- **Raw on the binding alone.** A secret is the furthest it may go; letting
  any binder expose any secret makes `AllowRaw` the one control a secret's
  owner keeps.
- **A sandbox may delegate raw.** A delegated sentinel can be revoked; a
  handed-over value cannot. One person's decision should not become a tree
  of sandboxes holding the value.
- **Deliver it in `SecretEnv` beside sentinels.** That document is written to
  the pool's disk, the sandbox's data volume, and exports, and exec
  environments reach the server.
- **Put it under the existing `/run/discobox/secrets`.** In a Docker
  sandbox that path is a bind mount of the pool's disk.
- **Raw values with no proxy check.** Most leaks are an agent pasting the
  value somewhere ordinary, which the scan catches cheaply; without it, the
  value would also be written unredacted into the HTTP audit.
- **Only static tokens may be raw.** The programs that need raw values
  rotate tokens as often as not; rewriting the file is cheap.

## Consequences

A person can give a sandbox a real value when a sentinel cannot work, without
it touching disk on the pool or in the sandbox. Its routes still refuse the
obvious ways out, and its use is still audited, but its confinement is
best-effort, and the records say which bindings carry that risk. Scanning
every request of a sandbox with a raw binding costs proxy CPU in proportion
to captured body size.
