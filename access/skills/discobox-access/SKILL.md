---
name: discobox-access
description: Ask a human for a credential this sandbox was not given, and run one command with it. Use when a command fails with 401/403, when a CLI says it is not logged in or has no token (gh, npm, docker, curl against a private API), or to check which credentials you may already use.
allowed-tools: Bash
---

# Asking for a credential

You are working inside a discobox — a sandbox that deliberately holds no
long-lived secrets. When a command needs a credential you do not have, you do
not have to stop and ask in chat: `discobox-access` asks a person, they answer
in their discobox window, and you carry on.

Every command below is safe to run and none of them prints a secret.

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
- `wait: true` blocks until a human answers. Without it you get a request ID
  back and the request sits pending — poll by asking again with `wait`.

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

For a script that genuinely cannot be wrapped there is
`discobox-access get --use USE_ID`, which prints the raw value. Prefer `run`:
`get` hands you a secret that then becomes your problem to not leak.

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
